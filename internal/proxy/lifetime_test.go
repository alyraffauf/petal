package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"petal/internal/routing"
)

func TestExitedAdapterWithInheritedOutput(t *testing.T) {
	// The adapter's helper keeps stdout open after the final initialize reply.
	lifetimeDir := t.TempDir()
	script := `read -r request
(while [ -d "$1" ]; do sleep 0.1; done) &
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'`
	p := proxy{embedded: true, lifetime: t.Context(), Config: routing.Config{
		Model: "test", Accounts: map[string]routing.Account{"local": {Models: map[string][]string{"test": {"sh", "-c", script, "adapter", lifetimeDir}}}},
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	b, err := p.startBackend(ctx, "local", json.RawMessage(`{"protocolVersion":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	select {
	case <-b.conn.Done():
	case <-ctx.Done():
		t.Fatal("exited adapter left the connection waiting for its helper")
	}
}
