package sessions

import (
	"context"
	"database/sql"
	"sync"
)

// Active children share one project connection until the last lease is released.
var childStores = struct {
	sync.Mutex
	projects map[string]*childStore
}{projects: make(map[string]*childStore)}

type childStore struct {
	db     *sql.DB
	refs   int
	writes chan childWrite
}

func retainChildStore(dir string) *childStore {
	childStores.Lock()
	defer childStores.Unlock()
	store := childStores.projects[dir]
	if store != nil {
		store.refs++
	}
	return store
}

func openChildStore(ctx context.Context, cwd, dir string) (*childStore, error) {
	if store := retainChildStore(dir); store != nil {
		return store, nil
	}
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return nil, err
	}
	childStores.Lock()
	defer childStores.Unlock()
	if store := childStores.projects[dir]; store != nil {
		db.Close()
		store.refs++
		return store, nil
	}
	store := &childStore{db: db, refs: 1, writes: make(chan childWrite)}
	go store.writeLoop()
	childStores.projects[dir] = store
	return store, nil
}

func (s *childStore) release(dir string) {
	childStores.Lock()
	s.refs--
	last := s.refs == 0
	if last {
		delete(childStores.projects, dir)
	}
	childStores.Unlock()
	if last {
		close(s.writes)
		s.db.Close()
	}
}

type childWrite struct {
	ctx  context.Context
	run  func(*sql.Tx) error
	done chan error
}

func (s *childStore) append(ctx context.Context, run func(*sql.Tx) error) error {
	w := childWrite{ctx, run, make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.writes <- w:
		return <-w.done
	}
}

func (s *childStore) writeLoop() {
	for first := range s.writes {
		batch := []childWrite{first}
	drain:
		for len(batch) < 32 {
			select {
			case w := <-s.writes:
				batch = append(batch, w)
			default:
				break drain
			}
		}
		if len(batch) > 1 && s.writeBatch(batch) {
			continue
		}
		for _, w := range batch {
			w.done <- runChildTransaction(w.ctx, s.db, w.run)
		}
	}
}

// Roll back statement failures and retry each caller independently. Once
// commit has been attempted, return its outcome to everyone without replay.
func (s *childStore) writeBatch(batch []childWrite) bool {
	// Each statement uses its caller's context. Keep the transaction alive
	// while deciding which callers can commit together.
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return false
	}
	for _, w := range batch {
		if err := w.run(tx); err != nil {
			tx.Rollback()
			return false
		}
	}
	for _, w := range batch {
		if w.ctx.Err() != nil {
			tx.Rollback()
			return false
		}
	}
	err = tx.Commit()
	for _, w := range batch {
		w.done <- err
	}
	return true
}
