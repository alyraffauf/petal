package sessions

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestListOrdersThreadsWithinProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	for i, id := range []string{"older", "newer"} {
		r := Record{ID: id, Account: "work", Model: "m", Opened: time.Unix(int64(i+1), 0)}
		if _, err := Create(project, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(project, []string{"m"})
	if err != nil || len(got) != 2 || got[0].ID != "newer" || got[1].ID != "older" {
		t.Fatalf("conversations: %+v, %v", got, err)
	}
	got, err = List(t.TempDir(), []string{"m"})
	if err != nil || len(got) != 0 {
		t.Fatalf("other project: %+v, %v", got, err)
	}
}

func TestConcurrentRecordEditsSurviveReopen(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state ?#"))
	project := t.TempDir()
	const workers = 10
	done := make(chan error, workers)
	for i := range workers {
		go func() {
			r, err := Create(project, Record{ID: "same", Model: "m"})
			if err == nil {
				_, err = Update(project, r.ID, func(current *Record) bool {
					current.Selections = append(current.Selections, Selection{ID: strconv.Itoa(i)})
					return true
				})
			}
			done <- err
		}()
	}
	for range workers {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	r, err := Lookup(project, "same")
	if err != nil || len(r.Selections) != workers {
		t.Fatalf("lost update: %+v, %v", r, err)
	}
}
