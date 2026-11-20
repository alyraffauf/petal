package sessions

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
	"petal/internal/paths"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS
var migrations, _ = fs.Sub(migrationFiles, "migrations")

func openStore(cwd string) (*sql.DB, error) {
	return openStoreContext(context.Background(), cwd)
}

func openStoreContext(ctx context.Context, cwd string) (*sql.DB, error) {
	dir, err := paths.XDG("STATE", filepath.Join("conversations", key(cwd)))
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Goose checks and applies migrations across multiple transactions. Serialize
	// initial opens from independent Petal processes during schema setup.
	lock, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "index.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithAllowOutofOrder(true))
	if err == nil {
		_, err = provider.Up(ctx)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
