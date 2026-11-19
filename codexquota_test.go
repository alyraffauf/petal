package main

import (
	"context"
	"os"
	"testing"
)

// This fixture speaks the Codex app-server handshake over stdio.
func TestInvalidCodexWindowExcludesAccount(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	script := t.TempDir() + "/codex"
	const adapter = `#!/bin/sh
read -r init
echo '{"id":1,"result":{}}'
read -r notification
read -r request
echo '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":101},"secondary":{"usedPercent":1}}}}'
`
	if err := os.WriteFile(script, []byte(adapter), 0700); err != nil {
		t.Fatal(err)
	}
	c := config{model: "m", Accounts: map[string]account{
		"invalid": {Provider: "codex", Env: map[string]string{"CODEX_PATH": script}, Models: map[string][]string{"m": {"agent"}}},
	}}
	if got, err := c.choose(context.Background()); err == nil {
		t.Fatalf("routed to %q despite invalid Codex quota", got)
	}
}
