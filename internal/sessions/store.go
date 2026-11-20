// Package sessions records the ACP conversations Petal can reopen in a project.
package sessions

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"petal/internal/paths"
)

// Selection is an option the user explicitly applied to this session.
type Selection struct {
	ID, Value  string
	LegacyMode bool `json:",omitempty"`
}

type Record struct {
	Parent     string      `json:",omitempty"`
	Selections []Selection `json:",omitempty"`
	ID         string
	Title      string
	Model      string
	Account    string
	Opened     time.Time
	Updated    time.Time `json:",omitzero"`
}

func (r Record) LastActivity() time.Time {
	if !r.Updated.IsZero() {
		return r.Updated
	}
	return r.Opened
}

func key(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func recordPath(cwd, id string) (string, error) {
	return paths.XDG("STATE", filepath.Join("conversations", key(cwd), key(id)+".json"))
}

func Lookup(cwd, id string) (Record, error) {
	path, err := recordPath(cwd, id)
	if err != nil {
		return Record{}, err
	}
	return readRecord(path)
}

func readRecord(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, err
	}
	var record Record
	err = json.Unmarshal(data, &record)
	return record, err
}

// Create records a session the first time Petal sees it.
func Create(cwd string, record Record) (saved Record, err error) {
	err = lockRecord(cwd, record.ID, func(path string) error {
		var err error
		saved, err = readRecord(path)
		if err != nil || saved.ID != "" {
			return err
		}
		saved = record
		return writeRecord(path, saved)
	})
	return saved, err
}

// Update applies an edit to the latest record and writes it when edit reports a change.
func Update(cwd, id string, edit func(*Record) bool) (record Record, err error) {
	err = lockRecord(cwd, id, func(path string) error {
		var err error
		record, err = readRecord(path)
		if err != nil {
			return err
		}
		if record.ID == "" {
			return os.ErrNotExist
		}
		if !edit(&record) {
			return nil
		}
		return writeRecord(path, record)
	})
	return record, err
}

func lockRecord(cwd, id string, edit func(string) error) error {
	path, err := recordPath(cwd, id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// Keep the lock file in place so every process locks the same inode.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if err == nil {
			return edit(path)
		}
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func writeRecord(path string, r Record) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(r)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func List(cwd string, models []string) ([]Record, error) {
	dir, err := paths.XDG("STATE", filepath.Join("conversations", key(cwd)))
	if err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var result []Record
	seen := make(map[string]bool)
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("read saved conversation %s: %w", file.Name(), err)
		}
		if slices.Contains(models, r.Model) {
			result = append(result, r)
			seen[r.ID] = true
		}
	}
	// Older Petal releases saved a single raw ID per directory and model.
	for _, model := range models {
		path, err := paths.XDG("STATE", filepath.Join("sessions", key(cwd+"\x00"+model)))
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var parts []string
		id := string(data)
		if seen[id] || json.Unmarshal(data, &parts) != nil || len(parts) != 3 || parts[1] != model {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		result = append(result, Record{ID: id, Account: parts[0], Model: model, Opened: info.ModTime()})
	}
	slices.SortStableFunc(result, func(a, b Record) int { return b.LastActivity().Compare(a.LastActivity()) })
	return result, nil
}

// Forget removes local resume information after the adapter confirms deletion.
func Forget(cwd, model, id string) error {
	return lockRecord(cwd, id, func(path string) error {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		legacy, err := paths.XDG("STATE", filepath.Join("sessions", key(cwd+"\x00"+model)))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(legacy)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if string(data) == id {
			return os.Remove(legacy)
		}
		return nil
	})
}
