package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

func childJournalFixture(t *testing.T) (string, ChildBinding) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	for _, id := range []string{"parent", "other", "child"} {
		if _, err := Create(cwd, Record{ID: id, Model: "model"}); err != nil {
			t.Fatal(err)
		}
	}
	binding := ChildBinding{ParentID: "parent", ChildID: "child"}
	if err := BindChild(t.Context(), cwd, binding); err != nil {
		t.Fatal(err)
	}
	return cwd, binding
}

func FuzzChildJournalHistory(f *testing.F) {
	f.Add([]byte{0, 8, 1, 16, 17, 2, 3, 4, 5, 6, 7, 0, 1})
	f.Add([]byte{1, 9, 0, 8, 16, 24, 33, 2, 1})
	// Run generated traces during ordinary go test as well. Bias toward
	// appends and reads, then place completion at different points.
	random := rand.New(rand.NewPCG(5, 29))
	for range 18 {
		operations := make([]byte, 24)
		kinds := []byte{0, 0, 0, 0, 1, 1, 1, 3, 4, 5, 6, 7}
		for i := range operations {
			operations[i] = (byte(random.Uint32()) &^ 7) | kinds[random.IntN(len(kinds))]
		}
		end := random.IntN(len(operations) - 2)
		operations[end], operations[end+1], operations[end+2] = 2, 2, 10
		f.Add(operations)
	}
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 64 {
			t.Skip()
		}
		cwd, binding := childJournalFixture(t)
		ctx := t.Context()
		wrong := ChildBinding{ParentID: "other", ChildID: binding.ChildID}
		history := []ChildActivity{}
		var final *ChildResult
		for step, op := range operations {
			switch op % 8 {
			case 0:
				data := json.RawMessage(strconv.Itoa(step))
				sequence, err := AppendChildActivity(ctx, cwd, binding, ChildActivity{Kind: "message", Data: data})
				if final != nil {
					requireJournalError(t, err, ErrChildFinished)
					continue
				}
				if err != nil || sequence != int64(len(history)+1) {
					t.Fatalf("append at step %d got %d, %v", step, sequence, err)
				}
				history = append(history, ChildActivity{Sequence: sequence, Kind: "message", Data: data})
			case 1:
				after, limit := int64(op>>5), int(op>>3&3)+1
				checkJournalPage(t, ctx, cwd, binding, after, limit, history, final)
			case 2:
				result := ChildResult{Status: "completed", Data: json.RawMessage(strconv.Itoa(int(op)))}
				err := FinishChild(ctx, cwd, binding, result)
				if final != nil && string(final.Data) != string(result.Data) {
					requireJournalError(t, err, ErrChildFinished)
				} else if err != nil {
					t.Fatal(err)
				} else {
					final = &result
				}
			case 3:
				_, err := ReadChildActivity(ctx, cwd, wrong, 0, 1)
				requireJournalError(t, err, ErrChildNotFound)
			case 4:
				_, err := AppendChildActivity(ctx, cwd, wrong, ChildActivity{Kind: "message"})
				requireJournalError(t, err, ErrChildNotFound)
			case 5:
				requireJournalError(t, FinishChild(ctx, cwd, wrong, ChildResult{Status: "failed"}), ErrChildNotFound)
			case 6:
				requireJournalError(t, BindChild(ctx, cwd, wrong), ErrChildAlreadyBound)
			case 7:
				if err := BindChild(ctx, cwd, binding); err != nil {
					t.Fatal(err)
				}
			}
		}
		checkJournalPage(t, ctx, cwd, binding, 0, 100, history, final)
	})
}

func checkJournalPage(t *testing.T, ctx context.Context, cwd string, binding ChildBinding, after int64, limit int, history []ChildActivity, final *ChildResult) {
	t.Helper()
	page, err := ReadChildActivity(ctx, cwd, binding, after, limit)
	if err != nil {
		t.Fatal(err)
	}
	status := "running"
	if final != nil {
		status = final.Status
	}
	expected := ChildActivityPage{Status: status, Activities: []ChildActivity{}, NextAfter: after, Result: final}
	for _, activity := range history {
		if activity.Sequence <= after {
			continue
		}
		if len(expected.Activities) == limit {
			expected.HasMore = true
			break
		}
		expected.Activities = append(expected.Activities, activity)
		expected.NextAfter = activity.Sequence
	}
	if !reflect.DeepEqual(page, expected) {
		t.Fatalf("read after %d (limit %d) got %+v, want %+v", after, limit, page, expected)
	}
}

func requireJournalError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestConcurrentChildAppendsKeepUniqueOrder(t *testing.T) {
	cwd, binding := childJournalFixture(t)
	ctx := t.Context()
	const workers = 12
	type appendResult struct {
		sequence int64
		kind     string
		err      error
	}
	appended := make(chan appendResult, workers)
	for i := range workers {
		go func() {
			kind := fmt.Sprintf("activity-%d", i)
			sequence, err := AppendChildActivity(ctx, cwd, binding, ChildActivity{Kind: kind})
			appended <- appendResult{sequence, kind, err}
		}()
	}
	bySequence := make(map[int64]string, workers)
	for range workers {
		result := <-appended
		if result.err != nil {
			t.Error(result.err)
		}
		bySequence[result.sequence] = result.kind
	}
	final := ChildResult{Status: "completed"}
	if err := FinishChild(ctx, cwd, binding, final); err != nil {
		t.Fatal(err)
	}
	page, err := ReadChildActivity(ctx, cwd, binding, 0, 0)
	if err != nil || page.Result == nil || page.Result.Status != final.Status || len(page.Activities) != workers {
		t.Fatalf("reopened journal got %+v, %v", page, err)
	}
	for index, activity := range page.Activities {
		if activity.Sequence != int64(index+1) || activity.Kind != bySequence[activity.Sequence] {
			t.Fatalf("activity %d got %+v", index, activity)
		}
	}
}

func TestForgetParentRemovesJournalAndOwnership(t *testing.T) {
	cwd, binding := childJournalFixture(t)
	ctx := t.Context()
	if _, err := AppendChildActivity(ctx, cwd, binding, ChildActivity{Kind: "message"}); err != nil {
		t.Fatal(err)
	}
	if err := FinishChild(ctx, cwd, binding, ChildResult{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	records, err := List(cwd, []string{"model"})
	if err != nil || len(records) != 3 || records[0].ID != binding.ChildID || records[0].DelegatedParent != binding.ParentID || records[0].DelegationStatus != "completed" {
		t.Fatalf("saved sessions got %+v, %v", records, err)
	}
	if err := Forget(cwd, "model", binding.ParentID); err != nil {
		t.Fatal(err)
	}
	_, err = ReadChildActivity(ctx, cwd, binding, 0, 0)
	requireJournalError(t, err, ErrChildNotFound)
	binding.ParentID = "other"
	if err := BindChild(ctx, cwd, binding); err != nil {
		t.Fatal(err)
	}
	checkJournalPage(t, ctx, cwd, binding, 0, 100, nil, nil)
}

func TestInterruptedChildRecovery(t *testing.T) {
	cwd, existing := childJournalFixture(t)
	binding := ChildBinding{ParentID: existing.ParentID, ChildID: "new"}
	record := Record{ID: binding.ChildID, Model: "other-model"}
	release, err := StartChild(t.Context(), cwd, binding, record, "check the route")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	page, err := ReadChildActivity(t.Context(), cwd, binding, 0, 1)
	if err != nil || page.Status != "running" {
		t.Fatalf("live child: %+v, %v", page, err)
	}
	release()
	records, err := List(cwd, []string{record.Model})
	if err != nil || len(records) != 1 || records[0].DelegationStatus != "failed" || !records[0].UnreadChildResult {
		t.Fatalf("reopened history kept an interrupted child active: %+v, %v", records, err)
	}
	page, err = ReadChildActivity(t.Context(), cwd, binding, 0, 1)
	if err != nil || page.Result == nil || page.Result.Status != "failed" {
		t.Fatalf("orphaned child: %+v, %v", page, err)
	}
	missing := ChildBinding{ParentID: "missing", ChildID: "orphan"}
	if _, err := StartChild(t.Context(), cwd, missing, Record{ID: "orphan"}, "task"); err == nil {
		t.Fatal("started child without a parent")
	}
	orphan, err := Lookup(cwd, "orphan")
	if err != nil || orphan.ID != "" {
		t.Fatalf("failed delegation left a session: %+v, %v", orphan, err)
	}
}

func TestChildResultConsumptionIsScopedAndIdempotent(t *testing.T) {
	cwd, binding := childJournalFixture(t)
	ctx := t.Context()
	sibling := ChildBinding{ParentID: binding.ParentID, ChildID: "sibling"}
	if _, err := Create(cwd, Record{ID: sibling.ChildID, Model: "model"}); err != nil {
		t.Fatal(err)
	}
	if err := BindChild(ctx, cwd, sibling); err != nil {
		t.Fatal(err)
	}
	requireJournalError(t, ConsumeChildResult(ctx, cwd, binding), ErrChildNotFound)
	result := ChildResult{Status: "completed", Data: json.RawMessage(`{"answer":"done"}`)}
	for _, child := range []ChildBinding{binding, sibling} {
		if err := FinishChild(ctx, cwd, child, result); err != nil {
			t.Fatal(err)
		}
	}
	unread := func(want map[string]bool) {
		t.Helper()
		records, err := List(cwd, []string{"model"})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, record := range records {
			if record.DelegatedParent == binding.ParentID {
				got[record.ID] = record.UnreadChildResult
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unread flags = %v, want %v", got, want)
		}
	}
	wrong := ChildBinding{ParentID: "other", ChildID: binding.ChildID}
	requireJournalError(t, ConsumeChildResult(ctx, cwd, wrong), ErrChildNotFound)
	requireJournalError(t, ConsumeChildResult(ctx, t.TempDir(), binding), ErrChildNotFound)
	checkJournalPage(t, ctx, cwd, binding, 0, 1, []ChildActivity{}, &result)
	unread(map[string]bool{"child": true, "sibling": true})

	for range 2 {
		if err := ConsumeChildResult(ctx, cwd, binding); err != nil {
			t.Fatal(err)
		}
	}
	if err := FinishChild(ctx, cwd, binding, result); err != nil {
		t.Fatal(err)
	}
	checkJournalPage(t, ctx, cwd, binding, 0, 1, []ChildActivity{}, &result)
	unread(map[string]bool{"child": false, "sibling": true})
}

func TestConcurrentChildJournalRejectsInvalidWrites(t *testing.T) {
	cwd, _ := childJournalFixture(t)
	binding := ChildBinding{ParentID: "parent", ChildID: "active"}
	release, err := StartChild(t.Context(), cwd, binding, Record{ID: "active", Model: "model"}, "task")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 12 {
		wg.Go(func() {
			<-start
			ctx := t.Context()
			b := binding
			var want error
			switch i % 3 {
			case 1:
				b.ParentID = "other"
				want = ErrChildNotFound
			case 2:
				ctx = canceled
				want = context.Canceled
			}
			_, err := AppendChildActivity(ctx, cwd, b, ChildActivity{Kind: "tool"})
			if !errors.Is(err, want) {
				t.Errorf("append %d: got %v, want %v", i, err, want)
			}
		})
	}
	close(start)
	wg.Wait()
	release()
	page, err := ReadChildActivity(t.Context(), cwd, binding, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Activities) != 4 || page.Result == nil || page.Result.Status != "failed" {
		t.Fatalf("recovered journal: %+v", page)
	}
	for i, a := range page.Activities {
		if a.Sequence != int64(i+1) {
			t.Fatalf("sequence %d at index %d", a.Sequence, i)
		}
	}
}
