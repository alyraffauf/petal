package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"
)

func TestElicitationValidatesAnswerAndRestoresDraft(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	h := &harness{lifetime: ctx, events: make(chan tea.Msg, 1), permissions: make(chan struct{}, 1)}
	result := make(chan map[string]any, 1)
	go func() {
		response, err := h.callback(ctx, "elicitation/create", json.RawMessage(`{"sessionId":"x","mode":"form","message":"Choose count","requestedSchema":{"type":"object","properties":{"count":{"type":"integer","minimum":1}},"required":["count"]}}`))
		if err != nil {
			result <- nil
			return
		}
		result <- response.(map[string]any)
	}()
	var e event
	select {
	case msg := <-h.events:
		e = msg.(event)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	m := terminal{screen: screen{connection: connected, editor: newComposer()}, ask: e.ask, permissionDraft: testComposer("draft", 3)}
	m.editor.SetValue(`{"count":0}`)
	m.submit()
	if m.ask == nil {
		t.Fatal("invalid answer was submitted")
	}
	m.editor.SetValue(`{"count":2}`)
	m.submit()
	select {
	case r := <-result:
		if r["action"] != "accept" || m.editor.Value() != "draft" {
			t.Fatalf("response %v, draft %q", r, m.editor.Value())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestAttachmentUsesNegotiatedBlock(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "note.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := attachment(cwd, "note.txt", acp.PromptCapabilities{})
	if err != nil || ref.content[0]["type"] != "resource_link" || ref.content[0]["uri"] != "file://"+filepath.Join(cwd, "note.txt") {
		t.Fatalf("baseline file reference: %+v, %v", ref, err)
	}
	p, err := attachment(cwd, "note.txt", acp.PromptCapabilities{EmbeddedContext: true})
	if err != nil || p.content[0]["resource"].(map[string]any)["text"] != "hello" {
		t.Fatalf("attachment %+v: %v", p, err)
	}
}

func TestURLCompletionLeavesOtherRequestsAndDraftAlone(t *testing.T) {
	m := terminal{root: bloom.New(), screen: screen{connection: connected, editor: testComposer("draft", 5)}}
	a := &urlRequest{id: "a", answer: make(chan string, 1)}
	b := &urlRequest{id: "b", answer: make(chan string, 1)}
	m.Update(a)
	m.Update(b)
	if m.ask != nil || m.showURL {
		t.Fatal("sign-in took focus from composer")
	}
	m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyF6})
	m.Update(urlCompleted("unrelated"))
	m.Update(urlCompleted("b"))
	m.Update(urlCompleted("b"))
	if !m.showURL || len(m.urls) != 1 || m.urls[0] != a || len(b.answer) != 1 || <-b.answer != "accept" {
		t.Fatal("completion changed the wrong request")
	}
	m.Update(urlCompleted("a"))
	if m.showURL || len(m.urls) != 0 || m.editor.Value() != "draftx" || len(a.answer) != 1 {
		t.Fatalf("completion left review open or lost draft: %q", m.editor.Value())
	}
}
