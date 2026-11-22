package sessions

import (
	"context"
	"database/sql"
	"strings"

	"petal/internal/acpv2"
	"petal/internal/sessions/sessiondb"
)

type Message struct{ Role, Body, SourceID string }

// ProjectTranscript retains adapter IDs. V1 chunk IDs used by the proxy to
// assemble text are local to a turn; only tool and compaction IDs survive replay.
func ProjectTranscript(t *acpv2.Transcript, nativeIDs bool) []Message {
	var out []Message
	t.Visit(func(id string, e acpv2.Entry) {
		if !nativeIDs && e.Role != "tool" && e.Role != "compaction" {
			id = ""
		}
		out = append(out, Message{e.Role, e.Text, id})
	})
	return out
}

// The archive is ordered by first observation. Its row IDs never change.
// Live unkeyed messages always append. During replay, consume previously
// observed occurrences once each so repeated identical turns retain multiplicity.
func SaveTranscript(cwd, id string, messages []Message, replay bool) error {
	if len(messages) == 0 {
		return nil
	}
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
	// Bound index probes so a full replay still uses one archive scan.
	const maxReplayProbes = 16
	if replay && len(messages) <= maxReplayProbes {
		allKnown := true
		for _, m := range messages {
			if m.SourceID == "" {
				allKnown = false
				break
			}
			known, err := hasTranscriptMessage(tx, id, m)
			if err != nil {
				return err
			}
			if !known {
				allKnown = false
				break
			}
		}
		if allKnown {
			return tx.Commit()
		}
	}
	skip := make([]bool, len(messages))
	if replay {
		if err := matchReplay(tx, id, messages, skip); err != nil {
			return err
		}
	}
	for i, m := range messages {
		if skip[i] {
			continue
		}
		if !replay && m.SourceID != "" {
			exists, err := hasTranscriptMessage(tx, id, m)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
		}
		if _, err := tx.Exec("INSERT INTO messages(session_id,role,body,source_id) VALUES(?,?,?,NULLIF(?,''))", id, m.Role, m.Body, m.SourceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func hasTranscriptMessage(tx *sql.Tx, id string, m Message) (bool, error) {
	var exists bool
	err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM messages WHERE session_id=? AND role=? AND source_id=? AND body=?)", id, m.Role, m.SourceID, m.Body).Scan(&exists)
	return exists, err
}

// Reserve exact source matches before assigning anonymous legacy rows. A
// keyed and an unkeyed occurrence in one replay cannot claim the same row.
func matchReplay(tx *sql.Tx, id string, messages []Message, skip []bool) error {
	type textKey struct{ role, body string }
	counts := map[textKey]int{}
	legacy := map[textKey][]int64{}
	known := map[Message]bool{}
	rows, err := tx.Query("SELECT rowid,role,body,COALESCE(source_id,'') FROM messages WHERE session_id=? ORDER BY rowid", id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var row int64
		var m Message
		if err := rows.Scan(&row, &m.Role, &m.Body, &m.SourceID); err != nil {
			return err
		}
		k := textKey{m.Role, m.Body}
		counts[k]++
		if m.SourceID == "" {
			legacy[k] = append(legacy[k], row)
		} else {
			known[m] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	seen := map[Message]bool{}
	for i, m := range messages {
		if m.SourceID == "" {
			continue
		}
		if seen[m] {
			skip[i] = true
			continue
		}
		seen[m] = true
		k := textKey{m.Role, m.Body}
		if !known[m] {
			ids := legacy[k]
			if len(ids) == 0 {
				continue
			}
			if _, err := tx.Exec("UPDATE messages SET source_id=? WHERE rowid=?", m.SourceID, ids[0]); err != nil {
				return err
			}
			legacy[k] = ids[1:]
		}
		skip[i] = true
		counts[k]--
	}
	for i, m := range messages {
		k := textKey{m.Role, m.Body}
		if m.SourceID == "" && counts[k] > 0 {
			skip[i] = true
			counts[k]--
		}
	}
	return nil
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

// TranscriptHead identifies the last archived message at the start of a live
// session. A resume without adapter replay can use that prefix with its journal.
func TranscriptHead(ctx context.Context, cwd, id string) (int64, error) {
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var head int64
	err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid), 0) FROM messages WHERE session_id = ?", id).Scan(&head)
	return head, err
}
