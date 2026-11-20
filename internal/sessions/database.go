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

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
	"petal/internal/paths"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS
var migrations, _ = fs.Sub(migrationFiles, "migrations")

func openStore(cwd string) (*sql.DB, error) {
	dir, err := paths.XDG("STATE", filepath.Join("conversations", key(cwd)))
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Goose checks and applies migrations across multiple transactions. Serialize
	// initial opens from independent Petal processes until the import commits.
	lock, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
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
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)&_txlock=immediate"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithGoMigrations(
		goose.NewGoMigration(2, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
			return importLegacy(ctx, tx, cwd, dir)
		}}, nil)))
	if err == nil {
		_, err = provider.Up(context.Background())
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
