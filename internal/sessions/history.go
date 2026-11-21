package sessions

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"unicode/utf8"

	"petal/internal/sessions/sessiondb"
)

// Message references are stable within the endpoint's project archive.
type HistoryPassage struct {
	Reference  int64             `json:"reference"`
	Session    string            `json:"session"`
	Role       string            `json:"role"`
	Text       string            `json:"text"`
	Offset     int               `json:"offset"`
	NextOffset int               `json:"next_offset,omitempty"`
	Before     []HistoryNeighbor `json:"before,omitempty"`
	After      []HistoryNeighbor `json:"after,omitempty"`
	Previous   int64             `json:"previous,omitempty"`
	Next       int64             `json:"next,omitempty"`
}

type HistoryNeighbor struct {
	Reference int64  `json:"reference"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Follow only durable delegation links, starting at the authenticated session.
const historyScope = `WITH RECURSIVE scope(id) AS (
	SELECT ? UNION SELECT b.child_id FROM child_bindings b JOIN scope ON b.parent_id=scope.id
) `

func ReadHistory(ctx context.Context, cwd, parent string, reference int64, offset int) (HistoryPassage, error) {
	return ReadHistoryWithContext(ctx, cwd, parent, reference, offset, 0)
}

func ReadHistoryWithContext(ctx context.Context, cwd, parent string, reference int64, offset, count int) (HistoryPassage, error) {
	var passage HistoryPassage
	if count < 0 || count > 8 {
		return passage, errors.New("context must be between 0 and 8")
	}
	if reference < 0 || offset < 0 || offset > 1<<31-1 || (reference == 0 && offset != 0) {
		return passage, errors.New("invalid transcript reference or offset")
	}
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return passage, err
	}
	defer db.Close()
	var reader sessiondb.DBTX = db
	if count > 0 {
		// Keep the scope check, navigation references, and neighbors on one snapshot.
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return passage, err
		}
		defer tx.Rollback()
		reader = tx
	}
	// A zero reference opens the latest message in the caller's thread.
	const query = historyScope + `SELECT m.rowid,m.session_id,m.role,coalesce(substr(CAST(m.body AS BLOB),?,4097), ''),
		coalesce((SELECT max(rowid) FROM messages WHERE session_id=m.session_id AND rowid<m.rowid),0),
		coalesce((SELECT min(rowid) FROM messages WHERE session_id=m.session_id AND rowid>m.rowid),0)
		FROM messages m WHERE m.rowid=coalesce(nullif(?,0),(SELECT max(rowid) FROM messages WHERE session_id=?))
		AND m.session_id IN (SELECT id FROM scope)`
	err = reader.QueryRowContext(ctx, query, parent, int64(offset)+1, reference, parent).Scan(&passage.Reference, &passage.Session, &passage.Role, &passage.Text, &passage.Previous, &passage.Next)
	if errors.Is(err, sql.ErrNoRows) {
		return passage, errors.New("no saved message at this reference")
	}
	if err != nil {
		return passage, err
	}
	passage.Offset = offset
	if len(passage.Text) > 0 && !utf8.RuneStart(passage.Text[0]) {
		return HistoryPassage{}, errors.New("offset is inside a UTF-8 character")
	}
	if len(passage.Text) > 4096 {
		end := 4096
		for !utf8.RuneStart(passage.Text[end]) {
			end--
		}
		passage.Text = passage.Text[:end]
		passage.NextOffset = offset + end
	}
	if count == 0 || passage.NextOffset != 0 {
		return passage, nil
	}
	remaining := 4096 - len(passage.Text)
	// Spend the shared budget on preceding messages first, nearest first on each side.
	for _, before := range []bool{true, false} {
		if remaining == 0 {
			break
		}
		order := "rowid > ? ORDER BY rowid ASC"
		dest := &passage.After
		if before {
			order = "rowid < ? ORDER BY rowid DESC"
			dest = &passage.Before
		}
		rows, err := reader.QueryContext(ctx, `SELECT rowid,role,coalesce(substr(CAST(body AS BLOB),1,?), '') FROM messages WHERE session_id=? AND `+order+" LIMIT ?", remaining+1, passage.Session, passage.Reference, count)
		if err != nil {
			return passage, err
		}
		for remaining > 0 && rows.Next() {
			var n HistoryNeighbor
			if err := rows.Scan(&n.Reference, &n.Role, &n.Text); err != nil {
				rows.Close()
				return passage, err
			}
			if len(n.Text) > remaining {
				end := remaining
				for end > 0 && !utf8.RuneStart(n.Text[end]) {
					end--
				}
				n.Text = n.Text[:end]
				n.Truncated = true
			}
			*dest = append(*dest, n)
			remaining -= len(n.Text)

		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return passage, err
		}
	}
	slices.Reverse(passage.Before)
	return passage, nil
}

type HistoryMatch struct {
	Reference int64  `json:"reference"`
	Session   string `json:"session"`
	Role      string `json:"role"`
	Snippet   string `json:"snippet"`
}

type HistoryMatches struct {
	Hits       []HistoryMatch `json:"hits"`
	NextBefore int64          `json:"next_before,omitempty"`
}

func SearchHistory(ctx context.Context, cwd, parent, query string, before int64) (HistoryMatches, error) {
	page := HistoryMatches{Hits: []HistoryMatch{}}
	if utf8.RuneCountInString(query) > 512 || before < 0 {
		return page, errors.New("invalid history query length or cursor")
	}
	expr := matchExpression(query)
	if expr == "" {
		return page, errors.New("enter a word or quoted phrase")
	}
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return page, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, historyScope+`SELECT m.rowid,m.session_id,m.role,CASE WHEN length(snippet(message_search,0,'','','…',18)) <= 512
		THEN snippet(message_search,0,'','','…',18)
		ELSE substr(snippet(message_search,0,'','','…',1),1,512) END
	FROM message_search JOIN messages m ON m.rowid=message_search.rowid
	WHERE message_search MATCH ? AND m.session_id IN (SELECT id FROM scope)
	AND (?=0 OR m.rowid < ?) ORDER BY m.rowid DESC LIMIT 11`, parent, expr, before, before)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Hits) == 10 {
			page.NextBefore = page.Hits[len(page.Hits)-1].Reference
			break
		}
		var hit HistoryMatch
		if err := rows.Scan(&hit.Reference, &hit.Session, &hit.Role, &hit.Snippet); err != nil {
			return page, err
		}
		page.Hits = append(page.Hits, hit)
	}
	return page, rows.Err()
}
