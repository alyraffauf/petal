package agentmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"petal/internal/delegation"
	"petal/internal/embeddings"
	"petal/internal/sessions"
)

type Tools struct {
	Embeddings embeddings.Settings

	Catalog func(string) any

	Delegate func(context.Context, string, string, string, string) (any, error)

	Read func(context.Context, string, string, string, int64) (delegation.Page, error)

	Wait func(context.Context, string, string, string, int64, time.Duration) (delegation.Page, error)
}

func delegationToolList(enabled bool) json.RawMessage {
	semanticTool := ""
	if enabled {
		semanticTool = `{"name":"recall","description":"Find archived messages by meaning. Combines semantic and keyword hits. Repeat if coverage.partial; falls back to keywords if embeddings are unavailable.","inputSchema":{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":512}},"required":["query"],"additionalProperties":false}},`
	}
	return json.RawMessage(`{"tools":[
{"name":"search","description":"Find archived messages by keyword, identifier, or quoted phrase. Page older hits with next_before.","inputSchema":{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":512},"before":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false}},
` + semanticTool + `
{"name":"open","description":"Read original message text. Omit reference for latest. Page with next_offset; context adds neighboring messages.","inputSchema":{"type":"object","properties":{"reference":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0},"context":{"type":"integer","minimum":0,"maximum":8}},"additionalProperties":false}},
{"name":"models","description":"List child model IDs. Filter by name or provider.","inputSchema":{"type":"object","properties":{"search":{"type":"string"}},"additionalProperties":false}},
{"name":"delegate","description":"Start a child with a listed model ID and a self-contained task. Returns childId.","inputSchema":{"type":"object","properties":{"model":{"type":"string","minLength":1},"task":{"type":"string","minLength":1}},"required":["model","task"],"additionalProperties":false}},
{"name":"activity","description":"Read child events and final result immediately. Continue from nextAfter.","inputSchema":{"type":"object","properties":{"child_id":{"type":"string","minLength":1},"after":{"type":"integer","minimum":0,"default":0}},"required":["child_id"],"additionalProperties":false}},
{"name":"wait","description":"Wait for child activity or completion. Returns the same page as activity. Timeout is in seconds.","inputSchema":{"type":"object","properties":{"child_id":{"type":"string","minLength":1},"after":{"type":"integer","minimum":0,"default":0},"timeout":{"type":"integer","minimum":1,"maximum":60,"default":30}},"required":["child_id"],"additionalProperties":false}}
]}`)
}

type mcpText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpText `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func (endpoint server) call(ctx context.Context, tools Tools, raw json.RawMessage) (mcpToolResult, string) {
	var call struct {
		Name      string
		Arguments json.RawMessage
	}
	var value any
	var child string
	err := json.Unmarshal(raw, &call)
	if err == nil {
		if record, ok := endpoint.bound(); !ok {
			err = errors.New("Petal has not confirmed the parent session yet")
		} else if err = ctx.Err(); err == nil {
			value, child, err = endpoint.invoke(ctx, tools, record, call.Name, call.Arguments)
		}
	}
	if err == nil {
		var encoded []byte
		encoded, err = json.Marshal(value)
		if err == nil {
			return mcpToolResult{Content: []mcpText{{Type: "text", Text: string(encoded)}}}, child
		}
	}
	return mcpToolResult{Content: []mcpText{{Type: "text", Text: err.Error()}}, IsError: true}, ""
}

func (endpoint server) invoke(ctx context.Context, tools Tools, record sessions.Record, name string, arguments json.RawMessage) (any, string, error) {
	decode := func(dest any) error {
		decoder := json.NewDecoder(bytes.NewReader(arguments))
		decoder.DisallowUnknownFields()
		return decoder.Decode(dest)
	}
	switch name {
	// Older sessions can retain the original tool catalog across a resume.
	case "petal_search_history":
		var args struct {
			Query  string
			Mode   string
			Before *int64
		}
		if err := decode(&args); err != nil {
			return nil, "", err
		}
		if args.Before == nil && (args.Mode == "hybrid" || (args.Mode == "" && tools.Embeddings.Provider != "off")) {
			value, err := sessions.SearchHybridHistory(ctx, endpoint.cwd, record.ID, args.Query, tools.Embeddings)
			return value, "", err
		}
		if args.Mode != "" && args.Mode != "fts" {
			return nil, "", errors.New("invalid search mode or hybrid cursor")
		}
		return endpoint.keywordHistory(ctx, record.ID, args.Query, args.Before)
	case "search":
		var args struct {
			Query  string
			Before *int64
		}
		if err := decode(&args); err != nil {
			return nil, "", err
		}
		return endpoint.keywordHistory(ctx, record.ID, args.Query, args.Before)
	case "recall", "petal_search_history_by_meaning":
		var args struct{ Query string }
		if err := decode(&args); err != nil {
			return nil, "", err
		}
		value, err := sessions.SearchHybridHistory(ctx, endpoint.cwd, record.ID, args.Query, tools.Embeddings)
		return value, "", err
	case "open", "petal_read_transcript":
		var args struct {
			Reference int64
			Offset    int
			Context   json.RawMessage
		}
		if len(arguments) > 0 {
			if err := decode(&args); err != nil {
				return nil, "", err
			}
		}
		count := 0
		if args.Context != nil {
			// JSON Schema integers include decimal and exponent spellings.
			n, ok := new(big.Rat).SetString(string(args.Context))
			if !ok || !n.IsInt() || !n.Num().IsInt64() || n.Sign() < 0 || n.Num().Int64() > 8 {
				return nil, "", errors.New("context must be an integer between 0 and 8")
			}
			count = int(n.Num().Int64())
		}
		value, err := sessions.ReadHistoryWithContext(ctx, endpoint.cwd, record.ID, args.Reference, args.Offset, count)
		return value, "", err
	case "models", "petal_capabilities":
		var args struct{ Search string }
		if len(arguments) > 0 {
			if err := decode(&args); err != nil {
				return nil, "", err
			}
		}
		return tools.Catalog(args.Search), "", nil
	case "delegate", "petal_delegate":
		var args struct{ Model, Task string }
		if err := decode(&args); err != nil {
			return nil, "", err
		}
		if strings.TrimSpace(args.Model) == "" || strings.TrimSpace(args.Task) == "" {
			return nil, "", errors.New("model and task are required")
		}
		if _, err := sessions.Create(endpoint.cwd, record); err != nil {
			return nil, "", err
		}
		value, err := tools.Delegate(ctx, record.ID, endpoint.cwd, args.Model, args.Task)
		return value, "", err
	case "activity", "petal_read":
		var args struct {
			ChildID string `json:"child_id"`
			After   int64
		}
		if err := decode(&args); err != nil {
			return nil, "", err
		}
		if args.ChildID == "" || args.After < 0 {
			return nil, "", errors.New("child_id is required and after must be nonnegative")
		}
		page, err := tools.Read(ctx, record.ID, endpoint.cwd, args.ChildID, args.After)
		return childDelivery(args.ChildID, page, err)
	case "wait", "petal_wait":
		args := struct {
			ChildID string `json:"child_id"`
			After   int64
			Timeout int64
		}{Timeout: 30}
		if err := decode(&args); err != nil {
			return nil, "", err
		}
		if args.ChildID == "" || args.After < 0 || args.Timeout < 1 || args.Timeout > 60 {
			return nil, "", errors.New("child_id is required, after must be nonnegative, and timeout must be 1 to 60 seconds")
		}
		page, err := tools.Wait(ctx, record.ID, endpoint.cwd, args.ChildID, args.After, time.Duration(args.Timeout)*time.Second)
		return childDelivery(args.ChildID, page, err)
	default:
		return nil, "", fmt.Errorf("unknown tool %q", name)
	}
}

func (endpoint server) keywordHistory(ctx context.Context, parent, query string, cursor *int64) (any, string, error) {
	var before int64
	if cursor != nil {
		before = *cursor
		if before < 1 {
			return nil, "", errors.New("before must be positive")
		}
	}
	page, err := sessions.SearchHistory(ctx, endpoint.cwd, parent, query, before)
	return page, "", err
}

func childDelivery(child string, page delegation.Page, err error) (any, string, error) {
	if err != nil {
		return nil, "", err
	}
	if page.Result == nil {
		child = ""
	}
	return childPageResponse(page), child, nil
}
func childPageResponse(page delegation.Page) any {
	next := "For more events, wait with after=nextAfter."
	switch {
	case page.HasMore:
		next = "More events: activity with after=nextAfter."
	case page.Status == "completed":
		next = "Use result.data.answer as the child's final reply."
	case page.Result != nil:
		next = "The child stopped. Inspect result.status and result.data before deciding whether to retry."
	case page.Status == "needs_input":
		next = "A child permission request needs the user's decision. Surface it to the user."
	}
	return struct {
		delegation.Page
		Next string `json:"next"`
	}{page, next}
}
