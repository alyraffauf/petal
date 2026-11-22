package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"petal/internal/sessions/sessiondb"
	"sync"
	"syscall"
)

var (
	ErrChildNotFound     = errors.New("child binding not found")
	ErrChildAlreadyBound = errors.New("child already bound to a different parent")
	ErrChildFinished     = errors.New("child already finished")
)

// ChildBinding identifies a direct parent-child relationship within a project.
type ChildBinding struct {
	ParentID string
	ChildID  string
}

type ChildActivity struct {
	Sequence int64           `json:"sequence"`
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data,omitempty"`
}

type ChildResult struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
}

type ChildActivityPage struct {
	Status     string          `json:"status"`
	Activities []ChildActivity `json:"activities"`
	NextAfter  int64           `json:"nextAfter"`
	HasMore    bool            `json:"hasMore"`
	Result     *ChildResult    `json:"result"`
}

// BindChild attaches two existing sessions. Repeating the same binding succeeds.
func BindChild(ctx context.Context, cwd string, binding ChildBinding) error {
	if binding.ParentID == "" || binding.ChildID == "" || binding.ParentID == binding.ChildID {
		return errors.New("child binding requires distinct, non-empty session IDs")
	}
	return childTransaction(ctx, cwd, func(tx *sql.Tx) error {
		var parent string
		err := tx.QueryRowContext(ctx, "SELECT parent_id FROM child_bindings WHERE child_id = ?", binding.ChildID).Scan(&parent)
		switch {
		case err == nil && parent == binding.ParentID:
			return nil
		case err == nil:
			return ErrChildAlreadyBound
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE id IN (?, ?)", binding.ChildID, binding.ParentID).Scan(&count); err != nil {
			return err
		}
		if count != 2 {
			return ErrChildNotFound
		}
		var cycle bool
		err = tx.QueryRowContext(ctx, `
			WITH RECURSIVE ancestors(id) AS (
				VALUES (?)
				UNION
				SELECT COALESCE(b.parent_id, s.parent)
				FROM ancestors a JOIN sessions s ON s.id = a.id
				LEFT JOIN child_bindings b ON b.child_id = s.id
			)
			SELECT EXISTS(SELECT 1 FROM ancestors WHERE id = ?)`, binding.ParentID, binding.ChildID).Scan(&cycle)
		if err != nil {
			return err
		}
		if cycle {
			return errors.New("child binding would create a cycle")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO child_bindings (parent_id, child_id) VALUES (?, ?)", binding.ParentID, binding.ChildID)
		return err
	})
}

// StartChild creates the history row and journal together. The lease is held
// until the terminal result is committed, or released by the OS on exit.
func StartChild(ctx context.Context, cwd string, binding ChildBinding, record Record, task string) (func(), error) {
	dir, err := storeDir(cwd)
	if err != nil {
		return nil, err
	}
	lease, err := os.CreateTemp(dir, "child-")
	if err != nil {
		return nil, err
	}
	release := func() { _ = os.Remove(lease.Name()); _ = lease.Close() }
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX); err != nil {
		release()
		return nil, err
	}
	store, err := openChildStore(ctx, cwd, dir)
	if err != nil {
		release()
		return nil, err
	}
	err = runChildTransaction(ctx, store.db, func(tx *sql.Tx) error {
		q := sessiondb.New(tx)
		if _, err := q.GetSession(ctx, binding.ChildID); !errors.Is(err, sql.ErrNoRows) {
			if err != nil {
				return err
			}
			return errors.New("adapter reused an existing child session ID")
		}
		if err := put(ctx, q, record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO child_bindings (child_id,parent_id,lease) VALUES (?,?,?)", binding.ChildID, binding.ParentID, filepath.Base(lease.Name()))
		if err != nil {
			return err
		}
		return q.AppendMessage(ctx, sessiondb.AppendMessageParams{SessionID: record.ID, Role: "user", Body: task})
	})
	if err != nil {
		store.release(dir)
		release()
		return nil, err
	}
	return sync.OnceFunc(func() {
		release()
		store.release(dir)
	}), nil
}

func recoverInterruptedChild(ctx context.Context, tx *sql.Tx, cwd string, binding ChildBinding) error {
	var name sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT lease FROM child_bindings WHERE child_id=? AND parent_id=? AND result_status IS NULL", binding.ChildID, binding.ParentID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || !name.Valid {
		return err
	}
	dir, err := storeDir(cwd)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, filepath.Base(name.String))
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE child_bindings SET result_status='failed', result_data=?, lease=NULL WHERE child_id=? AND parent_id=?`, []byte(`{"error":"child runtime exited before recording its result"}`), binding.ChildID, binding.ParentID)
	if err == nil {
		_ = os.Remove(path)
	}
	return err
}

// AppendChildActivity assigns the next sequence while holding a write transaction.
// The store ignores activity.Sequence. Each successful call adds one row.
func AppendChildActivity(ctx context.Context, cwd string, binding ChildBinding, activity ChildActivity) (int64, error) {
	if activity.Kind == "" || (len(activity.Data) > 0 && !json.Valid(activity.Data)) {
		return 0, errors.New("child activity requires a kind and optional valid JSON")
	}
	var sequence int64
	write := func(tx *sql.Tx) error {
		result, err := childResult(ctx, tx, binding)
		if err != nil {
			return err
		}
		if result != nil {
			return ErrChildFinished
		}
		return tx.QueryRowContext(ctx, `
			INSERT INTO child_activities (child_id, sequence, kind, data)
			SELECT ?, COALESCE(MAX(sequence), 0) + 1, ?, ? FROM child_activities WHERE child_id = ?
			RETURNING sequence`, binding.ChildID, activity.Kind, []byte(activity.Data), binding.ChildID).Scan(&sequence)
	}
	dir, err := storeDir(cwd)
	if err != nil {
		return 0, err
	}
	if store := retainChildStore(dir); store != nil {
		defer store.release(dir)
		err = store.append(ctx, write)
	} else {
		err = childTransaction(ctx, cwd, write)
	}

	if err != nil {
		return 0, err
	}
	return sequence, nil
}

// FinishChild sets an immutable result. A byte-for-byte repeat succeeds.
func FinishChild(ctx context.Context, cwd string, binding ChildBinding, result ChildResult) error {
	if result.Status == "" || (len(result.Data) > 0 && !json.Valid(result.Data)) {
		return errors.New("child result requires a status and optional valid JSON")
	}
	return childTransaction(ctx, cwd, func(tx *sql.Tx) error {
		saved, err := childResult(ctx, tx, binding)
		if err != nil {
			return err
		}
		if saved != nil {
			if saved.Status == result.Status && bytes.Equal(saved.Data, result.Data) {
				return nil
			}
			return ErrChildFinished
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE child_bindings SET result_status = ?, result_data = ?
			WHERE parent_id = ? AND child_id = ?`, result.Status, []byte(result.Data), binding.ParentID, binding.ChildID)
		return err
	})
}

// ConsumeChildResult records delivery of an immutable terminal result to its parent.
func ConsumeChildResult(ctx context.Context, cwd string, binding ChildBinding) error {
	return childTransaction(ctx, cwd, func(tx *sql.Tx) error {
		update, err := tx.ExecContext(ctx, `
			UPDATE child_bindings SET result_consumed = 1
			WHERE parent_id = ? AND child_id = ? AND result_status IS NOT NULL`, binding.ParentID, binding.ChildID)
		if err != nil {
			return err
		}
		count, err := update.RowsAffected()
		if err == nil && count == 0 {
			return ErrChildNotFound
		}
		return err
	})
}

// ReadChildActivity returns activity strictly after the cursor in sequence order.
// The result and page come from the same snapshot. A zero limit defaults to 100.
func ReadChildActivity(ctx context.Context, cwd string, binding ChildBinding, after int64, limit int) (ChildActivityPage, error) {
	if after < 0 || limit < 0 {
		return ChildActivityPage{}, errors.New("child activity cursor and limit must be non-negative")
	}
	if limit == 0 {
		limit = 100
	}
	limit = min(limit, 500)
	page := ChildActivityPage{NextAfter: after, Activities: []ChildActivity{}}
	err := childTransaction(ctx, cwd, func(tx *sql.Tx) error {
		var err error
		if err := recoverInterruptedChild(ctx, tx, cwd, binding); err != nil {
			return err
		}
		page.Result, err = childResult(ctx, tx, binding)
		if err != nil {
			return err
		}
		page.Status = "running"
		if page.Result != nil {
			page.Status = page.Result.Status
		} else {
			err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT kind FROM child_activities WHERE child_id=? AND kind IN ('needs_input','running') ORDER BY sequence DESC LIMIT 1),'running')`, binding.ChildID).Scan(&page.Status)
			if err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT sequence, kind, data FROM child_activities
			WHERE child_id = ? AND sequence > ? ORDER BY sequence LIMIT ?`, binding.ChildID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(page.Activities) == limit {
				page.HasMore = true
				break
			}
			var activity ChildActivity
			var data []byte
			if err := rows.Scan(&activity.Sequence, &activity.Kind, &data); err != nil {
				return err
			}
			activity.Data = data
			page.Activities = append(page.Activities, activity)
			page.NextAfter = activity.Sequence
		}
		return rows.Err()
	})
	if err != nil {
		return ChildActivityPage{}, err
	}
	return page, nil
}

func childResult(ctx context.Context, tx *sql.Tx, binding ChildBinding) (*ChildResult, error) {
	var status sql.NullString
	var data []byte
	err := tx.QueryRowContext(ctx, `
		SELECT result_status, result_data FROM child_bindings
		WHERE parent_id = ? AND child_id = ?`, binding.ParentID, binding.ChildID).Scan(&status, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChildNotFound
	}
	if err != nil || !status.Valid {
		return nil, err
	}
	return &ChildResult{Status: status.String, Data: data}, nil
}

func childTransaction(ctx context.Context, cwd string, run func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return err
	}
	defer db.Close()
	return runChildTransaction(ctx, db, run)
}

func runChildTransaction(ctx context.Context, db *sql.DB, run func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := run(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// loadChildBindings projects ownership and status for the session picker without
// reading the activity or result payloads.
func loadChildBindings(db *sql.DB, cwd string, records []Record) error {
	byID := make(map[string]*Record, len(records))
	for i := range records {
		byID[records[i].ID] = &records[i]
	}
	statuses, err := childStatuses(context.Background(), db, cwd)
	for binding, status := range statuses {
		if record := byID[binding.ChildID]; record != nil {
			record.DelegatedParent = binding.ParentID
			record.DelegationStatus = status.State
			record.UnreadChildResult = status.Unread
		}
	}
	return err
}

type ChildStatus struct {
	State  string
	Unread bool
}

func childStatuses(ctx context.Context, db *sql.DB, cwd string) (map[ChildBinding]ChildStatus, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	leases, err := tx.QueryContext(ctx, "SELECT parent_id, child_id FROM child_bindings WHERE lease IS NOT NULL AND result_status IS NULL")
	if err != nil {
		return nil, err
	}
	var active []ChildBinding
	for leases.Next() {
		var binding ChildBinding
		if err := leases.Scan(&binding.ParentID, &binding.ChildID); err != nil {
			leases.Close()
			return nil, err
		}
		active = append(active, binding)
	}
	leases.Close()
	if err := leases.Err(); err != nil {
		return nil, err
	}
	for _, binding := range active {
		if err := recoverInterruptedChild(ctx, tx, cwd, binding); err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT child_id, parent_id, COALESCE(result_status,
			CASE WHEN cancel_requested = 1 THEN 'cancelling' WHEN lease IS NOT NULL THEN 'running' ELSE '' END), result_status IS NOT NULL AND result_consumed = 0
		FROM child_bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	statuses := make(map[ChildBinding]ChildStatus)
	for rows.Next() {
		var binding ChildBinding
		var status ChildStatus
		if err := rows.Scan(&binding.ChildID, &binding.ParentID, &status.State, &status.Unread); err != nil {
			return nil, err
		}
		statuses[binding] = status
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return statuses, tx.Commit()
}
