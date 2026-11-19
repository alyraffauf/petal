package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileTierDrivesMeasuredRouting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	cli := filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(`#!/bin/sh
case "$CLAUDE_CONFIG_DIR" in
*/work) tier=max_5x ;;
*) tier=pro ;;
esac
printf '{"loggedIn":true,"authMethod":"claude.ai","planTier":"%s"}' "$tier"
`), 0700); err != nil {
		t.Fatal(err)
	}
	c := config{model: "m", Accounts: map[string]account{}}
	for name, value := range map[string]string{"work": "30", "personal": "90"} {
		c.Accounts[name] = account{Provider: "claude", Env: map[string]string{"CLAUDE_CODE_EXECUTABLE": cli, "CLAUDE_CONFIG_DIR": filepath.Join(dir, name)}, Models: map[string][]string{"m": {"agent"}}, Usage: []string{"/bin/sh", "-c", "printf '%s' \"$1\"", "quota", value}}
	}
	check := func(want string) {
		t.Helper()
		got, err := c.choose(context.Background())
		if err != nil || got != want {
			t.Fatalf("route = %q, %v; want %q", got, err, want)
		}
	}
	check("work")
	c.Tiers = map[string]map[string]float64{"claude": {"max_5x": 2}}
	check("personal")
	work := c.Accounts["work"]
	size := 4.0
	work.Capacity = &size
	c.Accounts["work"] = work
	check("work")
}
