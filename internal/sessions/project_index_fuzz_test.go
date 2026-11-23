package sessions

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

type projectIndexOperation byte

const (
	appendDocument projectIndexOperation = iota
	editDocument
	deleteDocument
	fillBatch
	fillAll
	cancelFill
	prepareQuery
	changeModel
	observeIndex
	projectIndexOperationCount
)

type indexedDocument struct {
	account, body string
	models        uint // Bit set for each cached model.
}

func FuzzProjectBackfillModel(f *testing.F) {
	f.Add(false, []byte{0, 4, 1, 4, 2, 0, 5, 4})
	f.Add(false, []byte{0, 9, 3, 24, 8})
	f.Add(true, []byte{0, 4, 0, 7, 4, 6, 4, 7, 6, 4})
	f.Add(false, []byte{0, 9, 18, 27, 36, 45, 54, 63, 72, 81, 3, 5, 4})
	f.Add(true, []byte{0, 9, 18, 27, 36, 45, 54, 63, 72, 81, 3, 5, 4})

	f.Fuzz(func(t *testing.T, compatible bool, trace []byte) {
		if len(trace) > 36 {
			trace = trace[:36]
		}
		provider := "ollama"
		if compatible {
			provider = "openai-compatible"
		}
		fixture := newProjectIndexFixture(t, provider)
		scopes := []string{"work", "personal", ""}
		account := scopes[0]
		activeModel, queryModel := 1, 1
		search := fixture.prepare(t, account)
		var documents []indexedDocument

		for step, encoded := range trace {
			op := projectIndexOperation(encoded) % projectIndexOperationCount
			arg := int(projectIndexOperation(encoded) / projectIndexOperationCount)
			body := fmt.Sprintf("relevant document %d", step)
			if arg%3 == 1 {
				body = fmt.Sprintf("unrelated document %d", step)
			}
			switch op {
			case appendDocument:
				owner := scopes[arg%2]
				fixture.append(t, owner, body)
				documents = append(documents, indexedDocument{account: owner, body: body})
			case editDocument, deleteDocument:
				if len(documents) != 0 {
					selected := arg % len(documents)
					doc := &documents[selected]
					if op == editDocument {
						fixture.exec(t, "UPDATE messages SET body=? WHERE body=?", body, doc.body)
						doc.body, doc.models = body, 0
					} else {
						fixture.exec(t, "DELETE FROM messages WHERE body=?", doc.body)
						documents = slices.Delete(documents, selected, selected+1)
					}
				}
			case fillBatch, fillAll:
				var pending []*indexedDocument
				for i := range documents {
					doc := &documents[i]
					if (account == "" || doc.account == account) && doc.models&(1<<queryModel) == 0 {
						pending = append(pending, doc)
					}
				}
				budget := time.Hour
				if op == fillBatch {
					budget = 0
				}
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				more, err := search.Backfill(ctx, budget)
				cancel()
				stale := activeModel != queryModel
				// Ollama can inspect its tag before sending a document batch.
				// Compatible providers reveal their identity in the reply.
				modelChanged := stale && (len(pending) > 0 || !compatible && hasProjectDocuments(documents, account))
				if modelChanged {
					if err == nil || more {
						t.Fatalf("step %d: changed provider returned more=%v, err=%v", step, more, err)
					}
					break
				}
				wantMore := op == fillBatch && len(pending) >= 8
				if more != wantMore || err != nil {
					t.Fatalf("step %d: backfill more=%v, err=%v, want more=%v", step, more, err, wantMore)
				}
				if op == fillBatch {
					pending = pending[:min(8, len(pending))]
				}
				for _, doc := range pending {
					doc.models |= 1 << queryModel
				}
			case cancelFill:
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if more, err := search.Backfill(ctx, time.Hour); more || !errors.Is(err, context.Canceled) {
					t.Fatalf("step %d: canceled backfill returned more=%v, err=%v", step, more, err)
				}
			case prepareQuery:
				account, queryModel = scopes[arg%len(scopes)], activeModel
				search = fixture.prepare(t, account)
			case changeModel:
				activeModel = 3 - activeModel
				fixture.tag = []string{"", "a", "b"}[activeModel]
				if compatible {
					fixture.model = []string{"", fixture.cfg.Model, "replacement"}[activeModel]
				}
			case observeIndex:
			}
			checkProjectIndex(t, search, documents, account, queryModel, step)
		}
	})
}

func hasProjectDocuments(documents []indexedDocument, account string) bool {
	for _, doc := range documents {
		if account == "" || account == doc.account {
			return true
		}
	}
	return false
}

func checkProjectIndex(t *testing.T, search *ProjectSearch, documents []indexedDocument, account string, model, step int) {
	t.Helper()
	var wantWindow, wantIndexed int
	var wantBodies []string
	for i := len(documents) - 1; i >= 0; i-- {
		doc := documents[i]
		if account != "" && doc.account != account {
			continue
		}
		wantWindow++
		if doc.models&(1<<model) != 0 {
			wantIndexed++
			if len(wantBodies) < 10 && strings.HasPrefix(doc.body, "relevant") {
				wantBodies = append(wantBodies, doc.body)
			}
		}
	}
	hits, coverage, err := search.Matches(t.Context())
	if err != nil || coverage.Window != wantWindow || coverage.Indexed != wantIndexed {
		t.Fatalf("step %d: coverage=%+v, want %d/%d, err=%v", step, coverage, wantIndexed, wantWindow, err)
	}
	var bodies []string
	for _, hit := range hits {
		bodies = append(bodies, hit.Snippet)
	}
	if !slices.Equal(bodies, wantBodies) {
		t.Fatalf("step %d: hits=%v, want %v", step, bodies, wantBodies)
	}
}
