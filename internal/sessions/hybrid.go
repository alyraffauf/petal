package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"time"
)

var embeddingClient = &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
var errEmbeddingProvider = errors.New("local embedding provider unavailable")
var embeddingSlot = make(chan struct{}, 1)

func embedHistory(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": "all-minilm:latest", "input": inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:11434/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := embeddingClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errEmbeddingProvider
	}
	var reply struct{ Embeddings [][]float32 }
	if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&reply); err != nil {
		return nil, err
	}
	if len(reply.Embeddings) != len(inputs) {
		return nil, errors.New("incomplete embeddings")
	}
	for _, v := range reply.Embeddings {
		if len(v) != 384 {
			return nil, errors.New("unexpected embedding dimensions")
		}
	}
	return reply.Embeddings, nil
}

// Backfill eight missing vectors per call. Compare the input again at write
// time because a transcript can change while Ollama is running.
func historyVectors(ctx context.Context, db *sql.DB, parent, query string) ([]HistoryMatch, error) {
	const window = historyScope + `, recent AS (SELECT rowid AS ref FROM messages WHERE session_id IN (SELECT id FROM scope) ORDER BY rowid DESC LIMIT 256) `
	rows, err := db.QueryContext(ctx, window+`SELECT ref,substr(body,1,1024) FROM recent JOIN messages ON rowid=ref LEFT JOIN message_embeddings ON reference=ref WHERE reference IS NULL ORDER BY ref DESC LIMIT 8`, parent)
	if err != nil {
		return nil, err
	}
	var ids []int64
	inputs := []string{query}
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		inputs = append(inputs, body)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	vectors, err := embedHistory(ctx, inputs)
	if err != nil {
		return nil, errors.Join(errEmbeddingProvider, err)
	}
	for i, id := range ids {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.LittleEndian, vectors[i+1])
		_, err = db.ExecContext(ctx, historyScope+`INSERT OR IGNORE INTO message_embeddings SELECT rowid,? FROM messages WHERE rowid=? AND session_id IN (SELECT id FROM scope) AND substr(body,1,1024)=?`, parent, buf.Bytes(), id, inputs[i+1])
		if err != nil {
			return nil, err
		}
	}
	rows, err = db.QueryContext(ctx, window+`SELECT ref,session_id,role,substr(body,1,512),vector FROM recent JOIN messages ON messages.rowid=ref JOIN message_embeddings ON reference=ref ORDER BY ref DESC`, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		hit   HistoryMatch
		score float64
	}
	var ranked []scored
	for rows.Next() {
		var hit HistoryMatch
		var blob []byte
		if err := rows.Scan(&hit.Reference, &hit.Session, &hit.Role, &hit.Snippet, &blob); err != nil {
			return nil, err
		}
		if len(blob) != 384*4 {
			continue
		}
		var score float64
		// Ollama /api/embed returns unit vectors.
		for i, q := range vectors[0] {
			score += float64(q) * float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[4*i:])))
		}
		if score >= 0.4 {
			ranked = append(ranked, scored{hit, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	var hits []HistoryMatch
	for _, r := range ranked {
		hits = append(hits, r.hit)
	}
	return hits, rows.Err()
}

func SearchHybridHistory(ctx context.Context, cwd, parent, query string) (HistoryMatches, error) {
	page, err := SearchHistory(ctx, cwd, parent, query, 0)
	if err != nil {
		return page, err
	}
	if os.Getenv("PETAL_HISTORY_EMBEDDINGS") != "1" {
		return page, errors.New("enable hybrid history with PETAL_HISTORY_EMBEDDINGS=1")
	}
	page.Retrieval = "fts"
	page.NextBefore = 0
	select {
	case embeddingSlot <- struct{}{}:
		defer func() { <-embeddingSlot }()
	default:
		return page, nil
	}
	timed, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	db, err := openStoreContext(timed, cwd)
	if err != nil {
		return page, err
	}
	defer db.Close()
	semantic, err := historyVectors(timed, db, parent, query)
	if errors.Is(err, errEmbeddingProvider) || errors.Is(err, context.DeadlineExceeded) {
		return page, ctx.Err()
	}
	if err != nil {
		return page, err
	}
	page.Retrieval = "hybrid"
	seen := map[int64]bool{}
	lexical := page.Hits
	page.Hits = []HistoryMatch{}
	for i := 0; i < len(lexical) || i < len(semantic); i++ {
		for _, list := range [][]HistoryMatch{lexical, semantic} {
			if i < len(list) && !seen[list[i].Reference] {
				hit := list[i]
				seen[hit.Reference] = true
				page.Hits = append(page.Hits, hit)
				if len(page.Hits) == 10 {
					return page, nil
				}
			}
		}
	}
	return page, nil
}
