package sessions

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"petal/internal/paths"
)

// ProjectThread identifies a saved thread even when two projects use the same adapter ID.
type ProjectThread struct {
	Project, Directory, ID, Parent, Title, Model, Account string
}

func ThreadInProject(cwd string, r Record) ProjectThread {
	return ProjectThread{Project: key(cwd), Directory: cwd, ID: r.ID, Parent: r.Parent, Title: r.Title, Model: r.Model, Account: r.Account}
}

func OtherProjectThreads(cwd string) ([]ProjectThread, error) {
	root, err := paths.XDG("STATE", "conversations")
	if err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []ProjectThread
	var failures []error
	for _, dir := range dirs {
		hash := dir.Name()
		if !dir.IsDir() || len(hash) != 64 || hash == key(cwd) {
			continue
		}
		if _, err := hex.DecodeString(hash); err != nil {
			continue
		}
		threads, err := readProjectThreads(filepath.Join(root, hash, storeFilename), hash)
		if err != nil {
			failures = append(failures, fmt.Errorf("read project %.8s: %w", hash, err))
			continue
		}
		result = append(result, threads...)
	}
	return result, errors.Join(failures...)
}

func readProjectThreads(path, hash string) ([]ProjectThread, error) {
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(500)"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var hasDirectory bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='project_directory')").Scan(&hasDirectory); err != nil {
		return nil, err
	}
	var cwd string
	if hasDirectory {
		err = db.QueryRow("SELECT cwd FROM project_directory WHERE id=1").Scan(&cwd)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if key(cwd) != hash || !filepath.IsAbs(cwd) {
			cwd = ""
		}
	}
	rows, err := db.Query("SELECT id, parent, title, model, account FROM sessions ORDER BY COALESCE(NULLIF(updated,''),opened) DESC, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ProjectThread
	for rows.Next() {
		r := ProjectThread{Project: hash, Directory: cwd}
		if err := rows.Scan(&r.ID, &r.Parent, &r.Title, &r.Model, &r.Account); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
