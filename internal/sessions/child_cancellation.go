package sessions

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrChildNotRunning = errors.New("child has no running delegation")

const childCancelPoll = 250 * time.Millisecond

// CancelChild records a request for the owning runtime and waits for a terminal
// result. An expired context stops the wait but leaves the request in place.
func CancelChild(ctx context.Context, cwd string, binding ChildBinding) (*ChildResult, error) {
	var result *ChildResult
	err := childTransaction(ctx, cwd, func(tx *sql.Tx) error {
		if err := recoverInterruptedChild(ctx, tx, cwd, binding); err != nil {
			return err
		}
		var err error
		result, err = childResult(ctx, tx, binding)
		if err != nil || result != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `
			UPDATE child_bindings SET cancel_requested = 1
			WHERE parent_id = ? AND child_id = ? AND lease IS NOT NULL`, binding.ParentID, binding.ChildID)
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err == nil && count == 0 {
			return ErrChildNotRunning
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if result != nil {
		return result, nil
	}
	tick := time.NewTicker(childCancelPoll)
	defer tick.Stop()
	for {
		err := childTransaction(ctx, cwd, func(tx *sql.Tx) error {
			if err := recoverInterruptedChild(ctx, tx, cwd, binding); err != nil {
				return err
			}
			var err error
			result, err = childResult(ctx, tx, binding)
			return err
		})
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// WaitForChildCancellation watches the binding owned by this runtime.
func WaitForChildCancellation(ctx context.Context, cwd string, binding ChildBinding) error {
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return err
	}
	defer db.Close()
	tick := time.NewTicker(childCancelPoll)
	defer tick.Stop()
	for {
		var requested bool
		err := db.QueryRowContext(ctx, `
			SELECT cancel_requested FROM child_bindings
			WHERE parent_id = ? AND child_id = ? AND result_status IS NULL AND lease IS NOT NULL`,
			binding.ParentID, binding.ChildID).Scan(&requested)
		if err != nil || requested {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
