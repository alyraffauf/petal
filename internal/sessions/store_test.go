package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestListKeepsOlderThreadsAndLegacySession(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	project := t.TempDir()
	legacyDir := filepath.Join(state, "petal", "sessions")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	const legacy = `["work","m","legacy"]`
	if err := os.WriteFile(filepath.Join(legacyDir, key(project+"\x00m")), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"older", "newer"} {
		r := Record{ID: id, Account: "work", Model: "m", Opened: time.Unix(int64(i+1), 0)}
		if _, err := Create(project, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(project, []string{"m"})
	if err != nil || len(got) != 3 || got[0].ID != legacy || got[1].ID != "newer" || got[2].ID != "older" {
		t.Fatalf("conversations: %+v, %v", got, err)
	}
	got, err = List(t.TempDir(), []string{"m"})
	if err != nil || len(got) != 0 {
		t.Fatalf("other project: %+v, %v", got, err)
	}
}

func TestImportRetriesAndDoesNotResurrectForgottenSessions(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	project := t.TempDir()
	dir := filepath.Join(state, "petal", "conversations", key(project))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	record := Record{ID: `["work","m","first"]`, Model: "m", Title: "Saved", Account: "work", Parent: "root", Updated: time.Now().UTC(), Selections: []Selection{{ID: "mode", Value: "read"}}}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "a.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "b.json")
	if err = os.WriteFile(corrupt, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = List(project, []string{"m"}); err == nil {
		t.Fatal("incomplete import reported success")
	}
	record.Title = "Repaired during retry"
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "a.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(corrupt); err != nil {
		t.Fatal(err)
	}
	got, err := Latest(project, "m")
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("imported %#v, want %#v (%v)", got, record, err)
	}
	if err = Forget(project, record.Model, record.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := List(project, []string{"m"}); err != nil || len(got) != 0 {
		t.Fatalf("forgotten record reappeared: %v %v", got, err)
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
