package sessions

import (
	"context"
	"strings"

	"petal/internal/sessions/sessiondb"
)

type Message struct{ Role, Body string }

func SaveTranscript(cwd, id string, messages []Message, replace bool) error {
	db, err := openStore(cwd)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := sessiondb.New(tx)
	if replace {
		// A load commonly replays exactly what was indexed at turn completion.
		// Keep the FTS rows (and search result row IDs) for an identical replay.
		rows, err := q.ReadTranscript(ctx, id)
		if err != nil {
			return err
		}
		equal := len(rows) == len(messages)
		if equal {
			for i, row := range rows {
				if row.Role != messages[i].Role || row.Body != messages[i].Body {
					equal = false
					break
				}
			}
		}
		if equal {
			return nil
		}
		if err := q.ClearTranscript(ctx, id); err != nil {
			return err
		}
	}
	for _, m := range messages {
		if err := q.AppendMessage(ctx, sessiondb.AppendMessageParams{SessionID: id, Role: m.Role, Body: m.Body}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func Transcript(cwd, id string) ([]sessiondb.ReadTranscriptRow, error) {
	db, err := openStore(cwd)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return sessiondb.New(db).ReadTranscript(context.Background(), id)
}

type Hit struct {
	Row                                           int64
	Session, Title, Model, Account, Role, Snippet string
}

// Each word is literal. Double quotes keep a sequence of words together.
func matchExpression(query string) string {
	var terms []string
	for i, part := range strings.Split(query, `"`) {
		words := strings.Fields(part)
		if i%2 == 1 && len(words) > 0 {
			words = []string{strings.Join(words, " ")}
		}
		for _, word := range words {
			terms = append(terms, `"`+word+`"`)
		}
	}
	return strings.Join(terms, " AND ")
}

func Search(cwd, account, query string, offset, limit int) ([]Hit, error) {
	expr := matchExpression(query)
	if expr == "" {
		return nil, nil
	}
	db, err := openStore(cwd)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT m.rowid,s.id,s.title,s.model,s.account,m.role,
		snippet(message_search,0,char(1),char(2),'…',18)
		FROM message_search JOIN messages m ON m.rowid=message_search.rowid
		JOIN sessions s ON s.id=m.session_id WHERE message_search MATCH ?
		AND (?='' OR s.account=?) ORDER BY rank, m.rowid DESC LIMIT ? OFFSET ?`, expr, account, account, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Row, &h.Session, &h.Title, &h.Model, &h.Account, &h.Role, &h.Snippet); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func RecentTranscript(ctx context.Context, cwd, id string, limit int64) ([]sessiondb.TranscriptTailRow, error) {
	db, err := openStore(cwd)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return sessiondb.New(db).TranscriptTail(ctx, sessiondb.TranscriptTailParams{SessionID: id, Limit: limit})
}
