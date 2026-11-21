package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"petal/internal/acpv2"
	"petal/internal/sessions"
)

// One recorder belongs to one editor-facing route. Embedded TUI and delegated
// children already record their own transcripts.
type transcriptRecorder struct{ active sync.Map }
type recordedSession struct {
	mu                              sync.Mutex
	cwd, id                         string
	version                         uint16
	projection                      acpv2.Transcript
	last                            string
	seq                             int
	replay, full, complete, resumed bool
}

func (r *transcriptRecorder) open(key, id, cwd, account, model string) (*recordedSession, error) {
	_, err := sessions.Create(cwd, sessions.Record{ID: id, Account: account, Model: model, Opened: time.Now()})
	if err != nil {
		return nil, err
	}
	s := &recordedSession{cwd: cwd, id: id}
	r.active.Store(key, s)
	return s, nil
}

func (s *recordedSession) save(replace bool) error {
	msgs := sessions.ProjectTranscript(&s.projection, s.version == 2)
	if len(msgs) > 0 || replace {
		if err := sessions.SaveTranscript(s.cwd, s.id, msgs, replace); err != nil {
			return err
		}
	}
	s.projection.Reset()
	s.last = ""
	s.seq = 0
	return nil
}

func (r *transcriptRecorder) observe(key string, version uint16, u Object) {
	value, ok := r.active.Load(key)
	if !ok {
		return
	}
	s := value.(*recordedSession)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = version
	kind := stringField(u, "sessionUpdate")
	if kind == "replay_complete" {
		s.complete = true
		return
	}
	if s.replay && s.complete && kind == "state_update" && stringField(u, "state") != "" {
		s.resumed = true
	}
	if version == 2 && kind == "state_update" && stringField(u, "state") == "idle" && !s.replay {
		if err := s.save(false); err != nil {
			slog.Error("save ACP transcript", "error", err)
		}
		return
	}
	if version == 1 {
		switch kind {
		case "agent_message_chunk", "agent_thought_chunk", "user_message_chunk":
			// The outbound v1 prompt is already durable.
			if kind == "user_message_chunk" && !s.replay {
				return
			}
			if kind != s.last {
				s.seq++
				s.last = kind
			}
			u["messageId"] = encoded(strconv.Itoa(s.seq))
		case "compaction_update", "compaction_summary_chunk":
			s.last = ""
		case "tool_call", "tool_call_update":
			normalizeV1Tool(u)
			u["sessionUpdate"] = encoded("tool_call_update")
			s.last = ""
		default:
			return
		}
	}
	raw, _ := json.Marshal(u)
	changed, _ := s.projection.Apply(raw)
	// Manual compaction can complete between turns, with no subsequent
	// prompt response or idle update to trigger a checkpoint.
	if changed && !s.replay && kind == "compaction_update" && stringField(u, "status") != "in_progress" {
		id := stringField(u, "compactionId")
		if entry, ok := s.projection.Lookup("compaction", id); ok {
			message := sessions.Message{Role: entry.Role, Body: entry.Text, SourceID: id}
			if err := sessions.SaveTranscript(s.cwd, s.id, []sessions.Message{message}, false); err != nil {
				slog.Error("save ACP compaction", "error", err)
			}
		}
	}
}

// V1 diffs carry the old and new file text directly. The v2 projection
// expects structured changes, so turn these into ordinary text blocks.
func normalizeV1Tool(u Object) {
	var blocks []Object
	if json.Unmarshal(u["content"], &blocks) != nil || len(blocks) == 0 {
		return
	}
	changed := false
	for i, part := range blocks {
		if stringField(part, "type") != "diff" {
			continue
		}
		body := stringField(part, "path") + "\nBefore:\n" + stringField(part, "oldText") + "\nAfter:\n" + stringField(part, "newText")
		content, _ := json.Marshal(Object{"type": encoded("text"), "text": encoded(body)})
		blocks[i] = Object{"type": encoded("content"), "content": content}
		changed = true
	}
	if changed {
		u["content"], _ = json.Marshal(blocks)
	}
}

// Stage replay separately until the provider confirms the load. A partial
// v2 replay has no durable cursor base and must not overwrite local history.
func (p *proxy) beforeTranscript(method, name, native, id string, params Object) (func(error) error, error) {
	r := p.recorder
	noop := func(err error) error { return err }
	if r == nil {
		return noop, nil
	}
	key := p.publicID(name, native)
	if method == "session/load" || method == "session/resume" {
		s := &recordedSession{cwd: stringField(params, "cwd"), id: id, replay: true, full: method == "session/load" || stringField(params.member("replayFrom"), "type") == "start"}
		prior, existed := r.active.Swap(key, s)
		return func(err error) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if err != nil {
				if existed {
					r.active.CompareAndSwap(key, s, prior)
				} else {
					r.active.CompareAndDelete(key, s)
				}
				return err
			}
			_, err = sessions.Create(s.cwd, sessions.Record{ID: id, Account: name, Model: p.Model, Opened: time.Now()})
			if err != nil {
				return err
			}
			if s.full && (p.version == 2 && s.resumed || p.version == 1 && len(s.projection.Entries()) > 0) {
				err = s.save(true)
			}
			s.projection.Reset()
			s.replay = false
			s.last = ""
			return err
		}, nil
	}
	value, ok := r.active.Load(key)
	if !ok {
		return noop, nil
	}
	s := value.(*recordedSession)
	switch method {
	case "session/delete", "session/close":
		return func(err error) error {
			if err == nil {
				r.active.CompareAndDelete(key, s)
				if method == "session/delete" {
					return sessions.Forget(s.cwd, "", s.id)
				}
			}
			return err
		}, nil
	case "session/prompt":
		if p.version == 1 {
			var prompt acpv2.Transcript
			data, _ := json.Marshal(Object{"sessionUpdate": encoded("user_message"), "messageId": encoded("prompt"), "content": params["prompt"]})
			_, _ = prompt.Apply(data)
			for _, e := range prompt.Entries() {
				if err := sessions.SaveTranscript(s.cwd, s.id, []sessions.Message{{Role: "user", Body: e.Text}}, false); err != nil {
					return nil, fmt.Errorf("record ACP prompt: %w", err)
				}
			}
			return func(err error) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				saveErr := s.save(false)
				if err != nil {
					return err
				}
				return saveErr
			}, nil
		}
	}
	return noop, nil
}
