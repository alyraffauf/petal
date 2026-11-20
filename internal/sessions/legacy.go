package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"petal/internal/paths"
	"petal/internal/sessions/sessiondb"
)

// The oldest format keyed a raw ID by hash(project, model). Read its embedded
// model to recover that key. A later JSON record for the same ID takes precedence.
func importLegacy(ctx context.Context, tx *sql.Tx, cwd, dir string) error {
	q := sessiondb.New(tx)
	legacy, err := paths.XDG("STATE", "sessions")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(legacy)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || len(entry.Name()) != 64 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(legacy, entry.Name()))
		if err != nil {
			return err
		}
		var parts []string
		if json.Unmarshal(data, &parts) != nil || len(parts) != 3 || entry.Name() != key(cwd+"\x00"+parts[1]) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err = put(ctx, q, Record{ID: string(data), Account: parts[0], Model: parts[1], Opened: info.ModTime()}); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		var r Record
		if err = json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("import session %s: %w", entry.Name(), err)
		}
		if r.ID == "" {
			return fmt.Errorf("import session %s: missing ID", entry.Name())
		}
		if err = put(ctx, q, r); err != nil {
			return err
		}
	}
	return nil
}
