package sessions

import (
	"context"
	"database/sql"
	"errors"
	"unicode/utf8"
)

// Message references are stable within the endpoint's project archive.
type HistoryPassage struct {
	Reference  int64  `json:"reference"`
	Session    string `json:"session"`
	Role       string `json:"role"`
	Text       string `json:"text"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"next_offset,omitempty"`
	Previous   int64  `json:"previous,omitempty"`
	Next       int64  `json:"next,omitempty"`
}

func ReadHistory(ctx context.Context, cwd, parent string, reference int64, offset int) (HistoryPassage, error) {
	var passage HistoryPassage
	if reference < 0 || offset < 0 || offset > 1<<31-1 || (reference == 0 && offset != 0) {
		return passage, errors.New("invalid transcript reference or offset")
	}
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return passage, err
	}
	defer db.Close()
	// A zero reference opens the latest message in the caller's thread.
	const query = `SELECT m.rowid,m.session_id,m.role,substr(m.body,?,4097),
		coalesce((SELECT max(rowid) FROM messages WHERE session_id=m.session_id AND rowid<m.rowid),0),
		coalesce((SELECT min(rowid) FROM messages WHERE session_id=m.session_id AND rowid>m.rowid),0)
		FROM messages m WHERE m.rowid=coalesce(nullif(?,0),(SELECT max(rowid) FROM messages WHERE session_id=?))
		AND m.session_id=?`
	err = db.QueryRowContext(ctx, query, int64(offset)+1, reference, parent, parent).Scan(&passage.Reference, &passage.Session, &passage.Role, &passage.Text, &passage.Previous, &passage.Next)
	if errors.Is(err, sql.ErrNoRows) {
		return passage, errors.New("no saved message at this reference")
	}
	if err != nil {
		return passage, err
	}
	passage.Offset = offset
	if utf8.RuneCountInString(passage.Text) > 4096 {
		_, size := utf8.DecodeLastRuneInString(passage.Text)
		passage.Text = passage.Text[:len(passage.Text)-size]
		passage.NextOffset = offset + 4096
	}
	return passage, nil
}
