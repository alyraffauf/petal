package routing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexDiscoversModelsWithoutCache(t *testing.T) {
	home := t.TempDir()
	cli := filepath.Join(home, "codex")
	if err := os.WriteFile(cli, []byte(`#!/bin/sh
[ "$1 $2" = "debug models" ] && [ -d "$CODEX_HOME" ] || exit 1
echo '{"models":[{"slug":"fresh","display_name":"Fresh model","visibility":"list"},{"slug":"hidden","visibility":"hide"}]}'
`), 0700); err != nil {
		t.Fatal(err)
	}
	a := discoverAccount("codex", cli, "codex-acp", Account{Env: map[string]string{"CODEX_HOME": home}})
	if !a.SelectModel["fresh"] || a.ModelLabels["fresh"] != "Fresh model" || len(a.Models["hidden"]) > 0 {
		t.Fatal(a.Models)
	}
}
