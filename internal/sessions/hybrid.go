package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"petal/internal/embeddings"
)

var embeddingClient = &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

var embeddingSlot = make(chan struct{}, 1)

// Backfill eight missing vectors per call. Compare the input again at write
// time because a transcript can change while Ollama is running.
const window = historyScope + `, recent AS (SELECT rowid AS ref FROM messages WHERE session_id IN (SELECT id FROM scope) ORDER BY rowid DESC LIMIT 256) `

func backfill(ctx context.Context, db *sql.DB, cfg embeddings.Settings, parent, query, digest, resolved string, queryVector []float32) ([]float32, error) {
	rows, err := db.QueryContext(ctx, window+`SELECT ref,substr(body,1,1024) FROM recent JOIN messages ON rowid=ref LEFT JOIN message_embeddings ON reference=ref AND digest=? WHERE reference IS NULL ORDER BY ref DESC LIMIT 8`, parent, digest)
	if err != nil {
		return nil, err
	}
	var ids []int64
	var inputs []string
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
	if queryVector != nil && len(inputs) == 0 {
		return queryVector, nil
	}
	batch := inputs
	if queryVector == nil {
		batch = append([]string{query}, inputs...)
	}
	vectors, model, err := cfg.Embed(ctx, embeddingClient, batch)
	if err != nil {
		return nil, errors.Join(embeddings.ErrUnavailable, err)
	}
	if queryVector == nil {
		queryVector, vectors = vectors[0], vectors[1:]
		current, err := cfg.CacheKey(ctx, embeddingClient)
		if err != nil || current != digest {
			return nil, embeddings.ErrUnavailable
		}
	} else if model != resolved || len(vectors[0]) != len(queryVector) {
		return nil, embeddings.ErrUnavailable
	}
	for i, id := range ids {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.LittleEndian, vectors[i])
		_, err = db.ExecContext(ctx, historyScope+`INSERT OR REPLACE INTO message_embeddings(reference,vector,digest) SELECT rowid,?,? FROM messages WHERE rowid=? AND session_id IN (SELECT id FROM scope) AND substr(body,1,1024)=?`, parent, buf.Bytes(), digest, id, inputs[i])
		if err != nil {
			return nil, err
		}
	}
	return queryVector, nil
}

func scoreHistory(ctx context.Context, db historyReader, parent, digest string, queryVector []float32, coverage *HistoryCoverage) ([]HistoryMatch, error) {
	rows, err := db.QueryContext(ctx, window+`SELECT ref,session_id,role,substr(body,1,512),vector FROM recent JOIN messages ON messages.rowid=ref LEFT JOIN message_embeddings ON reference=ref AND digest=? ORDER BY ref DESC`, parent, digest)
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
		coverage.Window++
		if len(blob) != len(queryVector)*4 {
			continue
		}
		coverage.Indexed++
		hit.Source = "semantic"
		var score float64
		// Vectors are normalized before storage and scoring.
		for i, q := range queryVector {
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

// Structured identifiers need lexical evidence even when nearby codes embed similarly.
var historyIdentifier = regexp.MustCompile(`[[:alnum:]]_[[:alnum:]]|[[:alpha:]][[:digit:]]|[[:digit:]][[:alpha:]]|[[:alnum:]]-[[:digit:]]|#[[:digit:]]|\b[[:digit:]]+\b`)

func optionalHistoryVector(ctx context.Context, db *sql.DB, cfg embeddings.Settings, parent, query string) ([]float32, string, error) {
	select {
	case embeddingSlot <- struct{}{}:
		defer func() { <-embeddingSlot }()
	default:
		return nil, "", nil
	}
	// Reserve the final tenth of the remaining budget for the scoped read.
	deadline, _ := ctx.Deadline()
	optional, stop := context.WithDeadline(ctx, deadline.Add(-time.Until(deadline)/10))
	defer stop()
	digest, err := cfg.CacheKey(optional, embeddingClient)
	if err != nil {
		return nil, "", ctx.Err()
	}
	// This handle belongs to this search. Yield immediately to an archive writer.
	if _, err = db.ExecContext(optional, "PRAGMA busy_timeout=0"); err != nil {
		if optional.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", err
	}
	var queryVector []float32
	var resolved string
	if cfg.Provider == "openai-compatible" {
		vectors, model, err := cfg.Embed(optional, embeddingClient, []string{query})
		if err != nil {
			return nil, "", ctx.Err()
		}
		queryVector, resolved = vectors[0], model
		digest += ":" + resolved + ":" + strconv.Itoa(len(queryVector))
	}
	vector, err := backfill(optional, db, cfg, parent, query, digest, resolved, queryVector)
	var sqliteErr *sqlite.Error
	busy := errors.As(err, &sqliteErr) && sqliteErr.Code()&255 == sqlite3.SQLITE_BUSY
	if busy || errors.Is(err, embeddings.ErrUnavailable) || optional.Err() != nil {
		return nil, "", ctx.Err()
	}
	return vector, digest, err
}

func SearchHybridHistory(ctx context.Context, cwd, parent, query string, cfg embeddings.Settings) (HistoryMatches, error) {
	return searchHybridHistory(ctx, cwd, parent, query, cfg, 10*time.Second)
}

func searchHybridHistory(caller context.Context, cwd, parent, query string, cfg embeddings.Settings, budget time.Duration) (HistoryMatches, error) {
	ctx, cancel := context.WithTimeout(caller, budget)
	defer cancel()
	var page HistoryMatches
	cfg, err := cfg.Resolve()
	if err != nil {
		return page, err
	}
	if cfg.Provider == "off" {
		return page, errors.New("hybrid history disabled in config")
	}
	if utf8.RuneCountInString(query) > 512 {
		return page, errors.New("invalid history query length")
	}
	expr := matchExpression(query)
	if expr == "" {
		return page, errors.New("invalid history query")
	}
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return page, err
	}
	defer db.Close()
	var digest string
	var vector []float32
	if !historyIdentifier.MatchString(query) {
		vector, digest, err = optionalHistoryVector(ctx, db, cfg, parent, query)
		if err != nil {
			return page, err
		}
	}
	if err := caller.Err(); err != nil {
		return page, err
	}
	// Optional cache writes yield immediately. Give the authoritative read
	// the remaining request budget to wait for a concurrent writer.
	deadline, _ := ctx.Deadline()
	waitMS := max(int64(0), time.Until(deadline).Milliseconds())
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout="+strconv.FormatInt(waitMS, 10)); err != nil {
		return page, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	page, err = searchHistory(ctx, tx, parent, expr, 0)
	if err != nil {
		return page, err
	}
	page.Retrieval = "fts"

	for i := range page.Hits {
		page.Hits[i].Source = "keyword"
	}
	if vector == nil {
		return page, ctx.Err()
	}
	var coverage HistoryCoverage
	semantic, err := scoreHistory(ctx, tx, parent, digest, vector, &coverage)
	if err != nil {
		return page, err
	}
	coverage.Partial = coverage.Indexed < coverage.Window
	page.Coverage = &coverage
	if coverage.Partial {
		page.Next = "Repeat this hybrid query to index up to eight more messages in the recent window."
	}
	page.NextBefore = 0
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
					return page, ctx.Err()
				}
			}
		}
	}
	return page, ctx.Err()
}
