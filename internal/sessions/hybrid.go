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

	"petal/internal/embeddings"
)

var embeddingClient = &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

var indexSlot = make(chan struct{}, 1)

var embeddingSlot = make(chan struct{}, 1)

const scopedMessages = historyScope + ` SELECT messages.rowid AS ref,substr(body,1,1024) FROM messages LEFT JOIN message_embeddings ON reference=messages.rowid AND digest=? WHERE session_id IN (SELECT id FROM scope) AND reference IS NULL ORDER BY messages.rowid LIMIT 8`

func embedHistory(ctx context.Context, cfg embeddings.Settings, digest string, inputs []string) ([][]float32, string, error) {
	vectors, model, err := cfg.Embed(ctx, embeddingClient, inputs)
	if err != nil {
		return nil, "", err
	}
	if cfg.Provider == "openai-compatible" {
		return vectors, digest + ":" + model + ":" + strconv.Itoa(len(vectors[0])), nil
	}
	current, err := cfg.CacheKey(ctx, embeddingClient)
	if err != nil || current != digest {
		return nil, "", embeddings.ErrUnavailable
	}
	return vectors, digest, nil
}

func indexHistoryBatch(ctx context.Context, cwd, parent string, cfg embeddings.Settings, lastDigest *string) (bool, error) {
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var hasMessages bool
	if err := db.QueryRowContext(ctx, historyScope+` SELECT EXISTS(SELECT 1 FROM messages WHERE session_id IN (SELECT id FROM scope))`, parent).Scan(&hasMessages); err != nil || !hasMessages {
		return false, err
	}
	digest, err := cfg.CacheKey(ctx, embeddingClient)
	if err != nil {
		return false, err
	}
	lookup := digest
	if cfg.Provider == "openai-compatible" {
		lookup = *lastDigest
	}
	rows, err := db.QueryContext(ctx, scopedMessages, parent, lookup)
	if err != nil {
		return false, err
	}
	var ids []int64
	var inputs []string
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
		inputs = append(inputs, body)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(inputs) == 0 {
		return false, nil
	}
	vectors, digest, err := embedHistory(ctx, cfg, digest, inputs)
	if err != nil {
		return false, err
	}
	if cfg.Provider == "openai-compatible" {
		*lastDigest = digest
	}
	for i, id := range ids {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.LittleEndian, vectors[i])
		_, err = db.ExecContext(ctx, historyScope+`INSERT OR REPLACE INTO message_embeddings(reference,vector,digest) SELECT rowid,?,? FROM messages WHERE rowid=? AND session_id IN (SELECT id FROM scope) AND substr(body,1,1024)=?`, parent, buf.Bytes(), digest, id, inputs[i])
		if err != nil {
			return false, err
		}
	}
	return len(inputs) == 8, nil
}

// IndexHistory follows the lifetime of a bound history endpoint. A full batch
// schedules another pass; idle scopes and unavailable providers retry slowly.
func IndexHistory(ctx context.Context, cwd, parent string, settings embeddings.Settings) {
	cfg, err := settings.Resolve()
	if err != nil || cfg.Provider == "off" {
		return
	}
	var digest string
	refresh := time.Now().Add(time.Hour)
	for ctx.Err() == nil {
		if time.Now().After(refresh) {
			digest = ""
			refresh = time.Now().Add(time.Hour)
		}
		select {
		case indexSlot <- struct{}{}:
		case <-ctx.Done():
			return
		}
		batch, cancel := context.WithTimeout(ctx, 10*time.Second)
		more, err := indexHistoryBatch(batch, cwd, parent, cfg, &digest)
		<-indexSlot
		cancel()
		delay := 30 * time.Second
		if more && err == nil {
			delay = 250 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func scoreHistory(ctx context.Context, db historyReader, parent, digest string, queryVector []float32, coverage *HistoryCoverage) ([]HistoryMatch, error) {
	rows, err := db.QueryContext(ctx, historyScope+` SELECT messages.rowid,session_id,role,substr(body,1,512),vector FROM messages LEFT JOIN message_embeddings ON reference=messages.rowid AND digest=? WHERE session_id IN (SELECT id FROM scope) ORDER BY messages.rowid DESC`, parent, digest)
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
			pos := sort.Search(len(ranked), func(i int) bool { return ranked[i].score < score })
			if pos < 10 {
				if len(ranked) < 10 {
					ranked = append(ranked, scored{})
				}
				copy(ranked[pos+1:], ranked[pos:])
				ranked[pos] = scored{hit, score}
			}
		}
	}
	var hits []HistoryMatch
	for _, r := range ranked {
		hits = append(hits, r.hit)
	}
	return hits, rows.Err()
}

// Structured identifiers need lexical evidence even when nearby codes embed similarly.
var historyIdentifier = regexp.MustCompile(`[[:alnum:]]_[[:alnum:]]|[[:alpha:]][[:digit:]]|[[:digit:]][[:alpha:]]|[[:alnum:]]-[[:digit:]]|#[[:digit:]]|\b[[:digit:]]+\b`)

func optionalHistoryVector(ctx context.Context, cfg embeddings.Settings, query string) ([]float32, string, error) {
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
	vectors, digest, err := embedHistory(optional, cfg, digest, []string{query})
	if err != nil {
		return nil, "", ctx.Err()
	}
	return vectors[0], digest, nil
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
		vector, digest, err = optionalHistoryVector(ctx, cfg, query)
		if err != nil {
			return page, err
		}
	}
	if err := caller.Err(); err != nil {
		return page, err
	}
	// Give the authoritative read
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
		page.Next = "Background indexing is incomplete. Retry later for additional semantic matches."
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
