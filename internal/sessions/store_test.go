package sessions

import (
	"os"
	"path/filepath"
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
