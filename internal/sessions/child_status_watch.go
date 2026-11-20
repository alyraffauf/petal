package sessions

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// ChildStatusWatch observes child bindings written by any process in the project.
type ChildStatusWatch struct {
	ctx    context.Context
	cancel context.CancelFunc
	files  *fsnotify.Watcher
	cwd    string
	first  bool
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
	return &ChildStatusWatch{ctx, cancel, files, cwd, true}, nil
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
	return childStatuses(w.ctx, db)
}

func (w *ChildStatusWatch) waitForCommit() error {
	for {
		select {
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
