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
	record                          sessions.Record
	version                         uint16
	projection                      acpv2.Transcript
	last                            string
	seq                             int
	replay, full, complete, resumed bool
}

func (r *transcriptRecorder) open(key, id, cwd, account, model string) *recordedSession {
	s := &recordedSession{cwd: cwd, id: id, record: sessions.Record{ID: id, Account: account, Model: model, Opened: time.Now()}}
	r.active.Store(key, s)
	return s
}

func (s *recordedSession) store(messages []sessions.Message, replay bool) error {
	if len(messages) == 0 {
		return nil
	}
	if _, err := sessions.Create(s.cwd, s.record); err != nil {
		return err
	}
	return sessions.SaveTranscript(s.cwd, s.id, messages, replay)
}

func (s *recordedSession) save(replace bool) error {
	msgs := sessions.ProjectTranscript(&s.projection, s.version == 2)
	if len(msgs) > 0 || replace {
		if err := s.store(msgs, replace); err != nil {
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
			acpv2.NormalizeV1Tool(u)
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
	if changed && !s.replay && (kind == "compaction_update" || kind == "compaction_summary_chunk") {
		id := stringField(u, "compactionId")
		if entry, ok := s.projection.FinishedCompaction(id); ok {
			message := sessions.Message{Role: entry.Role, Body: entry.Text, SourceID: id}
			if err := s.store([]sessions.Message{message}, false); err != nil {
				slog.Error("save ACP compaction", "error", err)
			}
		}
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
		s := &recordedSession{cwd: stringField(params, "cwd"), id: id, record: sessions.Record{ID: id, Account: name, Model: p.Model, Opened: time.Now()}, replay: true, full: method == "session/load" || stringField(params.member("replayFrom"), "type") == "start"}
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
				if err := s.store([]sessions.Message{{Role: "user", Body: e.Text}}, false); err != nil {
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
