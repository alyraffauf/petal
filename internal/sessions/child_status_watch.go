package sessions

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ChildStatusWatch observes child bindings written by any process in the project.
type ChildStatusWatch struct {
	ctx    context.Context
	cancel context.CancelFunc
	files  *fsnotify.Watcher
	cwd    string
	first  bool
	active bool
}

func WatchChildStatuses(cwd string) (*ChildStatusWatch, error) {
	dir, err := storeDir(cwd)
	if err != nil {
		return nil, err
	}
	files, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := files.Add(filepath.Join(dir, storeFilename)); err != nil {
		files.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &ChildStatusWatch{ctx: ctx, cancel: cancel, files: files, cwd: cwd, first: true}, nil
}

func (w *ChildStatusWatch) Close() {
	w.cancel()
	w.files.Close()
}

// Next takes an initial snapshot after registration, then waits for writes.
// Only one caller may wait at a time.
func (w *ChildStatusWatch) Next() (map[ChildBinding]ChildStatus, error) {
	if !w.first {
		if err := w.waitForCommit(); err != nil {
			return nil, err
		}
	}
	w.first = false
	db, err := openStoreContext(w.ctx, w.cwd)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	statuses, err := childStatuses(w.ctx, db, w.cwd)
	w.active = false
	for _, status := range statuses {
		if status.State == "running" || status.State == "cancelling" {
			w.active = true
			break
		}
	}
	return statuses, err
}

func (w *ChildStatusWatch) waitForCommit() error {
	// A process exit releases its file lock without writing to SQLite.
	var leaseCheck <-chan time.Time
	if w.active {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		leaseCheck = timer.C
	}
	for {
		select {
		case <-leaseCheck:
			return nil
		case <-w.ctx.Done():
			return w.ctx.Err()
		case err, ok := <-w.files.Errors:
			if !ok {
				return context.Canceled
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				return nil
			}
			return err
		case event, ok := <-w.files.Events:
			if !ok {
				return context.Canceled
			}
			// The store uses a rollback journal. SQLite holds the exclusive
			// lock through commit, so the snapshot waits for the writer.
			if event.Has(fsnotify.Write) {
				return nil
			}
		}
	}
}
