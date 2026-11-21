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
	mode := `"enum":["fts"]`
	description := ""
	constraint := ""
	if enabled {
		mode = `"enum":["fts","hybrid"]`
		description = " By default Petal tries hybrid search. Hybrid mixes keyword and semantic hits without pagination. Repeat partial searches to extend coverage."
		constraint = `,"not":{"required":["mode","before"],"properties":{"mode":{"const":"hybrid"}}}`
	}
	return json.RawMessage(`{"tools":[
{"name":"petal_search_history","description":"Find original sources for questions about earlier work, including exact tool output from completed delegated children and work before compaction or restart. Search saved user, assistant, and tool messages in this session and its descendants. Use a distinctive keyword, identifier, or tool name from the earlier task. Quote adjacent words for a phrase. For an original result or verbatim quote, open the matching reference with petal_read_transcript. Keyword matches are newest first. Pass next_before as before for older keyword matches.` + description + `","inputSchema":{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":512},"mode":{"type":"string",` + mode + `},"before":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false` + constraint + `}},
{"name":"petal_read_transcript","description":"Open an original archived message or tool result by numeric reference, including sources from completed delegated children. Use a reference from petal_search_history or one supplied by the user. Cite the returned reference when asked for a source. With no arguments, open your latest saved message. Pass previous or next as reference to move between messages. For long text, reuse reference with next_offset as offset. Set context (0–8) for neighboring messages on each side within the shared 4096-byte text budget.","inputSchema":{"type":"object","properties":{"reference":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0},"context":{"type":"integer","minimum":0,"maximum":8}},"additionalProperties":false}},
{"name":"petal_capabilities","description":"Discover configured child models before delegating. Search by model ID, label or provider if a target is missing. IDs respect the current account pin. Quota is checked at routing time when needed.","inputSchema":{"type":"object","properties":{"search":{"type":"string"}},"additionalProperties":false}},
{"name":"petal_delegate","description":"Use a model ID from petal_capabilities. Start an independent child in your working directory. Its only prompt is task, so include context, edit scope and expected output. Keep childId and follow next.","inputSchema":{"type":"object","properties":{"model":{"type":"string","minLength":1},"task":{"type":"string","minLength":1}},"required":["model","task"],"additionalProperties":false}},
{"name":"petal_read","description":"Read the activity journal for a delegated run you are following, including its terminal result. Use the full childId returned by petal_delegate. Start at 0 and pass nextAfter as after on subsequent calls. Follow next. For historical questions that ask for original sources, use petal_search_history.","inputSchema":{"type":"object","properties":{"child_id":{"type":"string","minLength":1},"after":{"type":"integer","minimum":0,"default":0}},"required":["child_id"],"additionalProperties":false}},
{"name":"petal_wait","description":"Wait while following a delegated run for activity, a permission request, completion or timeout (default 30 seconds). Use the full childId returned by petal_delegate and follow next. A running child may return an empty page.","inputSchema":{"type":"object","properties":{"child_id":{"type":"string","minLength":1},"after":{"type":"integer","minimum":0,"default":0},"timeout":{"type":"integer","minimum":1,"maximum":60,"default":30}},"required":["child_id"],"additionalProperties":false}}
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
	case "petal_search_history":
		var args struct {
			Mode   string
			Query  string
			Before *int64
		}
		if err := decode(&args); err != nil {
			return nil, "", err
		}
		if (args.Mode == "hybrid" || (args.Mode == "" && tools.Embeddings.Provider != "off")) && args.Before == nil {
			value, err := sessions.SearchHybridHistory(ctx, endpoint.cwd, record.ID, args.Query, tools.Embeddings)
			return value, "", err
		}
		if args.Mode != "" && args.Mode != "fts" {
			return nil, "", errors.New("invalid search mode or hybrid cursor")
		}
		var before int64
		if args.Before != nil {
			before = *args.Before
			if before < 1 {
				return nil, "", errors.New("before must be positive")
			}
		}
		value, err := sessions.SearchHistory(ctx, endpoint.cwd, record.ID, args.Query, before)
		return value, "", err
	case "petal_read_transcript":
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
	case "petal_capabilities":
		var args struct{ Search string }
		if len(arguments) > 0 {
			if err := decode(&args); err != nil {
				return nil, "", err
			}
		}
		return tools.Catalog(args.Search), "", nil
	case "petal_delegate":
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
	case "petal_read":
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
	case "petal_wait":
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
	next := "Call petal_wait with the same child_id and after set to nextAfter when you need more activity."
	switch {
	case page.HasMore:
		next = "Call petal_read with after set to nextAfter to read the rest of the journal."
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
