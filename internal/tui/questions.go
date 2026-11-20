package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type option struct{ OptionID, Name string }
type permission struct {
	sessionID, title string
	options          []option
	reply            chan int
	form             *jsonschema.Schema
	answer           chan map[string]any
}

func (q *permission) cancel() {
	if q.form != nil {
		q.answer <- nil
	} else {
		q.reply <- -1
	}
}

type dismissedQuestion struct{ q *permission }

func (h *harness) question(ctx context.Context, title string, q *permission) (any, error) {
	select {
	case h.permissions <- struct{}{}:
		defer func() { <-h.permissions }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var b strings.Builder
	b.WriteString(title)
	for i, opt := range q.options {
		fmt.Fprintf(&b, "\n\n%d. %s", i+1, opt.Name)
	}
	select {
	case h.events <- event{role: "system", text: b.String(), ask: q}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case choice := <-q.reply:
		if choice >= 0 && choice < len(q.options) {
			return q.options[choice].OptionID, nil
		}
		return nil, nil
	case answer := <-q.answer:
		if answer != nil {
			return answer, nil
		}
		return nil, nil
	case <-ctx.Done():
		select {
		case h.events <- dismissedQuestion{q}:
		case <-h.lifetime.Done():
		}
		return nil, ctx.Err()
	}
}
func (h *harness) choose(ctx context.Context, title string, options []option) (string, error) {
	result, err := h.question(ctx, title, &permission{options: options, reply: make(chan int, 1)})
	id, _ := result.(string)
	return id, err
}

type inlineSchemaOnly struct{}

func (inlineSchemaOnly) Load(string) (any, error) {
	return nil, fmt.Errorf("elicitation schemas must be self-contained")
}

func (h *harness) elicit(ctx context.Context, raw json.RawMessage) (any, *acp.RequestError) {
	var p struct {
		Mode, Message, URL, ElicitationID string
		RequestedSchema                   map[string]any
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, acp.NewInvalidParams(err.Error())
	}
	switch p.Mode {
	case "url":
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || clean(p.URL) != p.URL {
			return nil, acp.NewInvalidParams("expected a web URL")
		}
		if p.ElicitationID == "" {
			return nil, acp.NewInvalidParams("missing elicitationId")
		}
		action := h.requestURL(ctx, &urlRequest{id: p.ElicitationID, message: p.Message, url: p.URL, answer: make(chan string, 1)})
		return map[string]string{"action": action}, nil
	case "form":
		if p.RequestedSchema == nil {
			return nil, acp.NewInvalidParams("missing form schema")
		}
		const loc = "urn:petal:elicitation"
		compiler := jsonschema.NewCompiler()
		compiler.UseLoader(inlineSchemaOnly{})
		if err := compiler.AddResource(loc, p.RequestedSchema); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		schema, err := compiler.Compile(loc)
		if err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		desc, _ := json.MarshalIndent(p.RequestedSchema, "", "  ")
		answer, err := h.question(ctx, p.Message+"\n\nEnter a JSON object matching this form, or Escape to cancel.\n\n```json\n"+string(desc)+"\n```", &permission{form: schema, answer: make(chan map[string]any, 1)})
		if err != nil || answer == nil {
			return map[string]string{"action": "cancel"}, nil
		}
		return map[string]any{"action": "accept", "content": answer}, nil
	default:
		return nil, acp.NewInvalidParams("unsupported elicitation mode")
	}
}
