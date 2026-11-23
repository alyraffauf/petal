package routing

import (
	"bufio"
	"context"
	"encoding/json"
	"path/filepath"
)

func (a Account) openCodeDefaultLabel(ctx context.Context, cli, cwd string) string {
	directory, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	cmd := a.Command(ctx, []string{cli, "acp"})
	cmd.Dir = directory
	in, out, cleanup, err := startQuotaProcess(ctx, cmd)
	if err != nil {
		return ""
	}
	defer cleanup()
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 4096), 4<<20)
	request := func(id int, method string, params any) json.RawMessage {
		err := json.NewEncoder(in).Encode(map[string]any{"jsonrpc": "2.0", "method": method, "id": id, "params": params})
		if err != nil {
			return nil
		}
		for scan.Scan() {
			var reply struct {
				ID     int
				Result json.RawMessage
			}
			if json.Unmarshal(scan.Bytes(), &reply) == nil && reply.ID == id {
				return reply.Result
			}
		}
		return nil
	}
	if request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": struct{}{}}) == nil {
		return ""
	}
	raw := request(2, "session/new", map[string]any{"cwd": directory, "mcpServers": []string{}})
	type choice struct{ Value, Name string }
	var session struct {
		ConfigOptions []struct {
			ID, Category, CurrentValue string
			Options                    []struct {
				choice
				Options []choice
			}
		}
		Models struct {
			CurrentModelID  string
			AvailableModels []struct{ ModelID, Name string }
		}
	}
	if json.Unmarshal(raw, &session) != nil {
		return ""
	}
	for _, option := range session.ConfigOptions {
		if option.Category != "model" && option.ID != "model" || option.CurrentValue == "" {
			continue
		}
		for _, item := range option.Options {
			if item.Value == option.CurrentValue {
				return item.Name
			}
			for _, value := range item.Options {
				if value.Value == option.CurrentValue {
					return value.Name
				}
			}
		}
	}
	for _, model := range session.Models.AvailableModels {
		if model.ModelID == session.Models.CurrentModelID {
			return model.Name
		}
	}
	return ""
}
