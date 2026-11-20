package sessions

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestChildCancellationSurvivesWaiterCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()

	for _, id := range []string{"parent", "other-parent"} {
		if _, err := Create(cwd, Record{ID: id, Model: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	binding := ChildBinding{ParentID: "parent", ChildID: "child"}
	release, err := StartChild(ctx, cwd, binding, Record{ID: binding.ChildID, Model: "test"}, "test cancellation")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	wrongParent := ChildBinding{ParentID: "other-parent", ChildID: binding.ChildID}
	if result, err := CancelChild(ctx, cwd, wrongParent); result != nil || !errors.Is(err, ErrChildNotFound) {
		t.Fatalf("wrong parent returned result %+v, error %v", result, err)
	}

	// Keep the owner inactive while the requester waits for a result.
	wait, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		terminal, err := CancelChild(wait, cwd, binding)
		if terminal != nil {
			err = errors.New("reported a terminal result while the owner was still running")
		}
		result <- err
	}()
	if err := WaitForChildCancellation(ctx, cwd, binding); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("unconfirmed cancellation returned %v", err)
	}
	if err := WaitForChildCancellation(ctx, cwd, binding); err != nil {
		t.Fatalf("request did not survive its waiter: %v", err)
	}
	if err := FinishChild(ctx, cwd, binding, ChildResult{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	terminal, err := CancelChild(ctx, cwd, binding)
	if err != nil || terminal == nil || terminal.Status != "completed" {
		t.Fatalf("retry changed the owner's outcome: %+v, %v", terminal, err)
	}
}
