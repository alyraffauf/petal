package paths

import (
	"os"
	"path/filepath"
)

func XDG(kind, name string) (string, error) {
	dir := os.Getenv("XDG_" + kind + "_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, map[string]string{"CONFIG": ".config", "STATE": ".local/state"}[kind])
	}
	return filepath.Join(dir, "petal", name), nil
}
