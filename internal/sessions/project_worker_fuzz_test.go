package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func FuzzProjectWorker(f *testing.F) {
	f.Add(false, byte(0), []byte{1, 7, 2, 4, 9})
	f.Add(true, byte(1), []byte{5, 2, 1, 12})
	f.Add(false, byte(2), []byte{24, 33, 14})
	f.Fuzz(checkProjectWorker)
}

func TestProjectWorkerGeneratedHistories(t *testing.T) {
	const seed = 528292
	random := rand.New(rand.NewSource(seed))
	for i := range 12 {
		trace := make([]byte, 4)
		_, _ = random.Read(trace)
		t.Run(fmt.Sprintf("%d/%x", i, trace), func(t *testing.T) {
			checkProjectWorker(t, i%2 == 0, byte(i), trace)
		})
	}
}

type workerEmbeddingRequest struct {
	inputs []string
	reject chan bool
}

func checkProjectWorker(t *testing.T, compatible bool, scope byte, trace []byte) {
	if len(trace) == 0 {
		return
	}
	trace = trace[:min(len(trace), 6)]
	provider := "ollama"
	if compatible {
		provider = "openai-compatible"
	}
	f := newProjectIndexFixture(t, provider)
	scopes := []string{"work", "personal", ""}
	account := scopes[int(scope)%len(scopes)]
	owner := account
	if owner == "" {
		owner = "work"
	}

	var documents []indexedDocument
	nextBody := 0
	newBody := func() string {
		nextBody++
		if nextBody%2 == 0 {
			return fmt.Sprintf("relevant message %d", nextBody)
		}
		return fmt.Sprintf("unrelated message %d", nextBody)
	}
	appendMessages := func(owner string, count int) {
		messages := make([]Message, 0, count)
		for range count {
			body := newBody()
			messages = append(messages, Message{Role: "assistant", Body: body})
			documents = append(documents, indexedDocument{account: owner, body: body})
		}
		if err := SaveTranscript(f.cwd, owner, messages, false); err != nil {
			t.Fatal(err)
		}
	}
	appendMessages("work", 1)
	appendMessages("personal", 1)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	requests := make(chan workerEmbeddingRequest)
	embeddingClient.Transport = historyTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/tags" {
			return f.respond(r)
		}
		payload, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		var request struct{ Input []string }
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(payload))
		if slices.Equal(request.Input, []string{projectIndexQuery}) {
			return f.respond(r)
		}
		operation := workerEmbeddingRequest{inputs: request.Input, reject: make(chan bool)}
		select {
		case requests <- operation:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		select {
		case reject := <-operation.reject:
			if reject {
				return nil, errors.New("provider offline")
			}
			return f.respond(r)
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})

	idle, done := make(chan struct{}), make(chan struct{})
	wake := make(chan struct{}, 1)
	start := func() {
		go func() {
			defer close(done)
			IndexProject(ctx, f.cwd, account, f.cfg, wake, func() bool {
				select {
				case idle <- struct{}{}:
				case <-ctx.Done():
				}
				return false
			})
		}()
	}
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	model := 1
	searches := map[int]*ProjectSearch{model: f.prepare(t, account)}
	changeModel := func() {
		model = 3 - model
		f.tag = []string{"", "a", "b"}[model]
		if compatible {
			f.model = []string{"", f.cfg.Model, "replacement"}[model]
		}
		searches[model] = f.prepare(t, account)
	}
	for step, operation := range trace {
		// Always add new work so a compatible provider can reveal its model.
		count := 1
		if operation&16 != 0 {
			count += 8
		}
		appendMessages(owner, count)
		if operation&8 != 0 {
			changeModel()
		}
		if step == 0 {
			start()
			defer func() { cancel(); <-done }()
		} else {
			signal()
		}

		firstRequest := true
		failedBatch := false
		draining := true
		for draining {
			select {
			case call := <-requests:
				if len(call.inputs) == 0 || len(call.inputs) > 8 {
					t.Fatalf("step %d: invalid batch %v", step, call.inputs)
				}
				for _, body := range call.inputs {
					found := slices.ContainsFunc(documents, func(doc indexedDocument) bool {
						return doc.body == body && (account == "" || doc.account == account)
					})
					if !found {
						t.Fatalf("step %d: unexpected provider input %q for scope %q", step, body, account)
					}
				}
				rejectResponse := false
				if firstRequest {
					// Change the archive after the worker read the batch.
					target := slices.IndexFunc(documents, func(doc indexedDocument) bool {
						return doc.body == call.inputs[0]
					})
					switch operation % 4 {
					case 1:
						body := newBody()
						f.exec(t, "UPDATE messages SET body=? WHERE body=?", body, documents[target].body)
						documents[target].body, documents[target].models = body, 0
					case 2:
						f.exec(t, "DELETE FROM messages WHERE body=?", documents[target].body)
						documents = slices.Delete(documents, target, target+1)
					case 3:
						appendMessages(owner, 1)
					}
					rejectResponse = operation&4 != 0
					changedDuringRequest := operation&32 != 0
					if changedDuringRequest {
						changeModel()
					}
					failedBatch = rejectResponse || changedDuringRequest && !compatible
					firstRequest = false
				}
				select {
				case call.reject <- rejectResponse:
				case <-ctx.Done():
					t.Fatalf("step %d: provider reply blocked: %v", step, ctx.Err())
				}
				if !failedBatch && len(call.inputs) == 8 {
					// Advance the next batch immediately during fuzzing.
					signal()
				}
			case <-idle:
				if failedBatch {
					for identity, search := range searches {
						checkProjectIndex(t, search, documents, account, identity, step)
					}
					failedBatch = false
					// The failed batch may have lost its last uncached row to
					// deletion. Ensure the retry reaches the provider.
					appendMessages(owner, 1)
					signal()
					continue
				}
				for i := range documents {
					if account == "" || documents[i].account == account {
						documents[i].models |= 1 << model
					}
				}
				for identity, search := range searches {
					checkProjectIndex(t, search, documents, account, identity, step)
				}
				draining = false
			case <-ctx.Done():
				t.Fatalf("step %d: indexing did not reach idle: %v", step, ctx.Err())
			}
		}
	}
}

func TestProjectWorkerCancellationReleasesBackpressure(t *testing.T) {
	f := newProjectIndexFixture(t, "openai-compatible")
	f.append(t, "work", "relevant work document")
	f.append(t, "personal", "relevant personal document")
	workSearch := f.prepare(t, "work")
	personalSearch := f.prepare(t, "personal")
	started := make(chan struct{})
	f.beforeReply = func(r *http.Request) {
		close(started)
		<-r.Context().Done()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	active, stopActive := context.WithCancel(ctx)
	activeDone := make(chan struct{})
	go func() {
		defer close(activeDone)
		IndexProject(active, f.cwd, "work", f.cfg, nil, nil)
	}()
	defer func() { stopActive(); <-activeDone }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first worker never reached the provider")
	}

	queued, stopQueued := context.WithCancel(ctx)
	defer stopQueued()
	waiting := &indexWaitContext{Context: queued, waiting: make(chan struct{})}
	queuedDone := make(chan struct{})
	go func() {
		defer close(queuedDone)
		IndexProject(waiting, f.cwd, "personal", f.cfg, nil, nil)
	}()
	defer func() { stopQueued(); <-queuedDone }()
	select {
	case <-waiting.waiting:
	case <-ctx.Done():
		t.Fatal("second worker never waited for the slot")
	}
	stopQueued()
	select {
	case <-queuedDone:
	case <-ctx.Done():
		t.Fatal("second worker did not cancel while the first held the slot")
	}
	stopActive()
	select {
	case <-activeDone:
	case <-ctx.Done():
		t.Fatal("first worker did not cancel during the provider call")
	}
	checkProjectIndex(t, workSearch, []indexedDocument{{account: "work", body: "relevant work document"}}, "work", 1, 0)

	f.beforeReply = nil
	IndexProject(ctx, f.cwd, "personal", f.cfg, nil, func() bool { return true })
	checkProjectIndex(t, personalSearch, []indexedDocument{{account: "personal", body: "relevant personal document", models: 1 << 1}}, "personal", 1, 1)
}

type indexWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *indexWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestProjectWorkerRetriesWithoutWake(t *testing.T) {
	f := newProjectIndexFixture(t, "openai-compatible")
	for i := range 9 {
		f.append(t, "work", fmt.Sprintf("relevant message %d", i))
	}
	search := f.prepare(t, "work")
	var attempts []time.Time
	embeddingClient.Transport = historyTransport(func(r *http.Request) (*http.Response, error) {
		attempts = append(attempts, time.Now())
		if len(attempts) == 1 {
			return nil, errors.New("provider offline")
		}
		return f.respond(r)
	})

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		IndexProject(ctx, f.cwd, "work", f.cfg, nil, func() bool {
			return len(attempts) >= 3
		})
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		if len(attempts) != 3 {
			t.Fatalf("got %d provider calls, want one failure followed by two batches", len(attempts))
		}
		if retry, nextBatch := attempts[1].Sub(attempts[0]), attempts[2].Sub(attempts[1]); retry <= nextBatch || nextBatch <= 0 {
			t.Fatalf("provider retry took %s and full batch follow-up took %s", retry, nextBatch)
		}
		_, coverage, err := search.Matches(ctx)
		if err != nil || coverage.Indexed != 9 || coverage.Window != 9 {
			t.Fatalf("coverage=%+v, err=%v", coverage, err)
		}
	})
}
