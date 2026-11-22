package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"petal/internal/routing"
)

// Terminal authentication launches the configured adapter with its advertised
// arguments and profile environment. The client appends the flag to "petal acp".
type terminalLogin struct {
	Account string
	Model   string
	Args    []string
}

func LoginCommand(ctx context.Context, c routing.Config, token string) (*exec.Cmd, error) {
	var login terminalLogin
	if json.Unmarshal([]byte(token), &login) != nil || login.Account == "" || login.Model == "" {
		return nil, fmt.Errorf("invalid terminal authentication arguments")
	}
	a := c.Accounts[login.Account]
	argv := a.Models[login.Model]
	if len(argv) == 0 || (c.PinnedAccount != "" && c.PinnedAccount != login.Account) {
		return nil, fmt.Errorf("authentication account or model is unavailable")
	}
	cmd := a.Command(ctx, append(append([]string(nil), argv...), login.Args...))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// Adapters may ask for MCP sign-in after session/new has already returned.
// Track the first prompt per native session so a sibling chat cannot enable
// interactive startup sign-in for a new or reopened session.
func (b *backend) trackSessionStartup(method string, params Object) {
	switch method {
	case "session/prompt":
		b.promptedSessions.Store(stringField(params, "sessionId"), struct{}{})
	case "session/load", "session/resume":
		b.promptedSessions.Delete(stringField(params, "sessionId"))
	}
}

func (b *backend) forgetClosedSession(method string, params Object) {
	if method == "session/close" || method == "session/delete" {
		b.promptedSessions.Delete(stringField(params, "sessionId"))
	}
}

func (b *backend) startupURL(raw json.RawMessage) bool {
	var args Object
	if json.Unmarshal(raw, &args) != nil || stringField(args, "mode") != "url" || !hasString(args, "sessionId") || args["requestId"] != nil {
		return false
	}
	_, prompted := b.promptedSessions.Load(stringField(args, "sessionId"))
	return !prompted
}
