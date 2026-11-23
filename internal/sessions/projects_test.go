package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOtherProjectThreadsSkipsDeletedDirectories(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	kept, deleted, current := filepath.Join(root, "kept"), filepath.Join(root, "deleted"), filepath.Join(root, "current")
	for _, dir := range []string{kept, deleted, current} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := Create(dir, Record{ID: dir, Model: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	threads, err := OtherProjectThreads(current)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].Directory != kept || threads[0].ID != kept {
		t.Fatalf("threads = %#v, want only the thread in %s", threads, kept)
	}
}
