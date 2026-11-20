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

func TestTranscriptSearchFollowsSessionLifecycle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	for _, account := range []string{"work", "personal"} {
		if _, err := Create(project, Record{ID: account, Account: account, Model: "codex"}); err != nil {
			t.Fatal(err)
		}
		if err := SaveTranscript(project, account, []Message{{"user", "Measure cache miss frequency"}, {"agent", "Cache hits outnumber misses"}}, false); err != nil {
			t.Fatal(err)
		}
	}
	search := func(cwd, account, query string, offset int) []Hit {
		t.Helper()
		hits, err := Search(cwd, account, query, offset, 1)
		if err != nil {
			t.Fatal(err)
		}
		return hits
	}
	first := search(project, "work", `"cache miss"`, 0)
	if len(first) != 1 || first[0].Role != "user" || first[0].Account != "work" {
		t.Fatal(first)
	}
	rows, err := Transcript(project, first[0].Session)
	if err != nil || len(rows) != 2 || rows[0].Rowid != first[0].Row {
		t.Fatal(rows, err)
	}
	if hits := search(project, "", `"cache miss"`, 1); len(hits) != 1 {
		t.Fatal(hits)
	}
	if hits := search(t.TempDir(), "", "cache", 0); len(hits) != 0 {
		t.Fatal(hits)
	}
	if err := SaveTranscript(project, "work", []Message{{"tool", "Rotated credentials"}}, true); err != nil {
		t.Fatal(err)
	}
	if hits := search(project, "work", "cache", 0); len(hits) != 0 {
		t.Fatal(hits)
	}
	if hits := search(project, "work", "credentials", 0); len(hits) != 1 || hits[0].Role != "tool" {
		t.Fatal(hits)
	}
	if err := Forget(project, "codex", "work"); err != nil {
		t.Fatal(err)
	}
	if hits := search(project, "", "credentials", 0); len(hits) != 0 {
		t.Fatal(hits)
	}
}

func TestUpdateRejectsTimestampOutsideStorageRange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	original := Record{ID: "thread", Model: "m", Opened: time.Unix(10, 0)}
	if _, err := Create(project, original); err != nil {
		t.Fatal(err)
	}
	// This valid RFC3339 time rolls over to the year 10000 in UTC.
	stamp, err := time.Parse(time.RFC3339, "9999-12-31T23:30:00-01:00")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update(project, original.ID, func(record *Record) bool {
		record.Updated = stamp
		return true
	}); err == nil {
		t.Error("saved a timestamp that the store cannot read")
	}
	got, err := Lookup(project, original.ID)
	if err != nil || got.ID != original.ID || !got.Opened.Equal(original.Opened) || !got.Updated.IsZero() {
		t.Fatalf("failed edit changed the stored record: %+v, %v", got, err)
	}
}
