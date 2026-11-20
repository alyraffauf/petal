// Package sessions records the ACP conversations Petal can reopen in a project.
package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"petal/internal/sessions/sessiondb"
)

// Selection is an option the user explicitly applied to this session.
type Selection struct {
	ID, Value  string
	LegacyMode bool `json:",omitempty"`
}

type Record struct {
	DelegatedParent   string      `json:"-"`
	DelegationStatus  string      `json:"-"`
	UnreadChildResult bool        `json:"-"`
	Parent            string      `json:",omitempty"`
	Selections        []Selection `json:",omitempty"`
	ID                string
	Title             string
	Model             string
	Account           string
	Opened            time.Time
	Updated           time.Time `json:",omitzero"`
}

func (r Record) LastActivity() time.Time {
	if !r.Updated.IsZero() {
		return r.Updated
	}
	return r.Opened
}

func key(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

// Fixed-width UTC timestamps preserve chronological order in a SQLite TEXT index.
const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func timestamp(t time.Time) (string, error) {
	if t.IsZero() {
		return "", nil
	}
	utc := t.UTC()
	if year := utc.Year(); year < 0 || year > 9999 {
		return "", fmt.Errorf("session timestamp year %d is outside the storage range", year)
	}
	return utc.Format(timestampLayout), nil
}
func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(timestampLayout, value)
}
func decode(row sessiondb.Session) (Record, error) {
	r := Record{ID: row.ID, Parent: row.Parent, Title: row.Title, Model: row.Model, Account: row.Account}
	var err error
	if r.Opened, err = parseTime(row.Opened); err != nil {
		return Record{}, err
	}
	if r.Updated, err = parseTime(row.Updated); err != nil {
		return Record{}, err
	}
	if err = json.Unmarshal([]byte(row.Selections), &r.Selections); err != nil {
		return Record{}, err
	}
	return r, nil
}

func put(ctx context.Context, q *sessiondb.Queries, r Record) error {
	opened, err := timestamp(r.Opened)
	if err != nil {
		return err
	}
	updated, err := timestamp(r.Updated)
	if err != nil {
		return err
	}
	selections, err := json.Marshal(r.Selections)
	if err != nil {
		return err
	}
	return q.PutSession(ctx, sessiondb.PutSessionParams{ID: r.ID, Parent: r.Parent, Title: r.Title, Model: r.Model, Account: r.Account, Opened: opened, Updated: updated, Selections: string(selections)})
}

func Lookup(cwd, id string) (Record, error) {
	db, err := openStore(cwd)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	row, err := sessiondb.New(db).GetSession(context.Background(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, err
	}
	return decode(row)
}

// Create keeps the first record for an ID, including when two clients see it at once.
func Create(cwd string, record Record) (Record, error) {
	return editRecord(cwd, record.ID, func(saved *Record) bool {
		if saved.ID != "" {
			return false
		}
		*saved = record
		return true
	}, true)
}

// Update applies edit to the current record while holding a SQLite write transaction.
func Update(cwd, id string, edit func(*Record) bool) (Record, error) {
	return editRecord(cwd, id, edit, false)
}

func editRecord(cwd, id string, edit func(*Record) bool, allowCreate bool) (Record, error) {
	db, err := openStore(cwd)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	q := sessiondb.New(tx)
	row, err := q.GetSession(ctx, id)
	var record Record
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if !allowCreate {
			return Record{}, os.ErrNotExist
		}
	case err != nil:
		return Record{}, err
	default:
		record, err = decode(row)
		if err != nil {
			return Record{}, err
		}
	}
	if edit(&record) {
		if record.ID != id || id == "" {
			return Record{}, errors.New("session edit requires an unchanged, non-empty ID")
		}
		if err := put(ctx, q, record); err != nil {
			return Record{}, err
		}
	}
	return record, tx.Commit()
}

func List(cwd string, models []string) ([]Record, error) {
	if len(models) == 0 {
		return nil, nil
	}
	db, err := openStore(cwd)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := sessiondb.New(db).ListSessions(context.Background(), models)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(rows))
	for _, row := range rows {
		r, err := decode(row)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if len(records) == 0 {
		return records, nil
	}
	if err := loadChildBindings(db, records); err != nil {
		return nil, err
	}
	return records, nil
}

func Latest(cwd, model string) (Record, error) {
	db, err := openStore(cwd)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	row, err := sessiondb.New(db).LatestSession(context.Background(), model)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, os.ErrNotExist
	}
	if err != nil {
		return Record{}, err
	}
	return decode(row)
}

func Forget(cwd, _, id string) error {
	db, err := openStore(cwd)
	if err != nil {
		return err
	}
	defer db.Close()
	return sessiondb.New(db).DeleteSession(context.Background(), id)
}
