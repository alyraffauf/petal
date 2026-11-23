package sessions

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"petal/internal/embeddings"
)

// ProjectSearch keeps a query vector for repeated reads as the daemon indexes messages.
type ProjectSearch struct {
	cwd, account, digest, expression string
	cfg                              embeddings.Settings
	vector                           []float32
}

func PrepareProjectSearch(ctx context.Context, cwd, account, query string, settings embeddings.Settings) *ProjectSearch {
	cfg, err := settings.Resolve()
	if err != nil || cfg.Provider == "off" || strings.TrimSpace(query) == "" || strings.Contains(query, "\"") || historyIdentifier.MatchString(query) || utf8.RuneCountInString(query) > 512 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	vector, digest, _ := optionalHistoryVector(ctx, cfg, query)
	if len(vector) == 0 {
		return nil
	}
	return &ProjectSearch{cwd: cwd, account: account, cfg: cfg, vector: vector, digest: digest, expression: matchExpression(query)}
}

func (s *ProjectSearch) Matches(ctx context.Context) ([]Hit, HistoryCoverage, error) {
	var coverage HistoryCoverage
	db, err := openStoreContext(ctx, s.cwd)
	if err != nil {
		return nil, coverage, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, coverage, err
	}
	defer tx.Rollback()
	ranked, err := scoreHistory(ctx, tx, projectAccountScope, s.account, s.digest, s.expression, s.vector, &coverage)
	if err != nil {
		return nil, coverage, err
	}
	var hits []Hit
	for _, r := range ranked {
		h := Hit{Row: r.Reference, Session: r.Session, Role: r.Role, Snippet: r.Snippet, Semantic: true}
		err = tx.QueryRowContext(ctx, `SELECT title,model,account FROM sessions WHERE id=?`, r.Session).Scan(&h.Title, &h.Model, &h.Account)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, coverage, err
		}
		hits = append(hits, h)
	}
	return hits, coverage, nil
}

// Backfill indexes in short batches for frontends connected to legacy daemons.
// Closing the search cancels pending work.
func (s *ProjectSearch) Backfill(ctx context.Context, interval time.Duration) (bool, error) {
	until := time.Now().Add(interval)
	digest := s.digest
	for {
		select {
		case indexSlot <- struct{}{}:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		batch, cancel := context.WithTimeout(ctx, 10*time.Second)
		more, err := indexScopedBatch(batch, s.cwd, projectAccountScope, s.account, s.cfg, &digest, true)
		cancel()
		<-indexSlot
		if err != nil || !more {
			return false, err
		}
		if time.Now().After(until) {
			return true, nil
		}
	}
}
