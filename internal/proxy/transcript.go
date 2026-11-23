package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"petal/internal/acphost"
	"petal/internal/acpv2"
	"petal/internal/sessions"
)

// One recorder belongs to one editor-facing route. Embedded TUI and delegated
// children already record their own transcripts.
type transcriptRecorder struct {
	active sync.Map
	host   *acphost.Host
}
type recordedSession struct {
	toolTerminals                   map[string][]terminalReference
	children                        map[string]*nativeChild
	mail                            map[string]*acpv2.Transcript
	dirtyMail                       []string
	mailPending                     map[string]bool
	nativeReplay                    []sessions.Message
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
	for i := range msgs {
		if msgs[i].Role == "tool" {
			for _, ref := range s.toolTerminals[msgs[i].SourceID] {
				msgs[i].Body += acphost.FormatTerminalOutput(ref.view())
			}
		}
	}
	msgs = append(msgs, s.nativeReplay...)
	msgs = append(msgs, s.mailRows()...)
	if len(msgs) > 0 || replace {
		if err := s.store(msgs, replace); err != nil {
			return err
		}
	}
	s.nativeReplay = nil
	s.clearDirtyMail()
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
			if r.host != nil {
				s.trackToolTerminals(r.host, u)
			}
			s.last = ""
		default:
			return
		}
	}
	changed, _ := s.projection.ApplyFields(u, version)
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

// unsaved reports whether a session holds observed history that is not yet in
// the archive, such as an ACP v2 turn that has not reported idle.
func (r *transcriptRecorder) unsaved(key string) bool {
	value, ok := r.active.Load(key)
	if !ok {
		return false
	}
	s := value.(*recordedSession)
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.replay && (len(s.projection.Entries()) > 0 || len(s.nativeReplay) > 0)
}

// flush checkpoints the last observed updates during daemon shutdown,
// including an ACP v2 turn that did not report idle before closing.
func (r *transcriptRecorder) flush() {
	r.active.Range(func(_, value any) bool {
		session := value.(*recordedSession)
		session.mu.Lock()
		defer session.mu.Unlock()
		if !session.replay {
			if err := session.save(false); err != nil {
				slog.Error("save ACP transcript on close", "error", err)
			}
		}
		return true
	})
}

// Stage replay separately until the provider confirms the load. A partial
// v2 replay has no durable cursor base and must not overwrite local history.
func (p *proxy) beforeTranscript(method, name, native, id string, params Object) (func(error) error, error) {
	r := p.recorder
	noop := func(err error) error { return err }
	if r == nil || p.local != nil && p.version == 2 {
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
			if s.full && (p.version == 2 && s.resumed || p.version == 1 && (len(s.projection.Entries()) > 0 || len(s.nativeReplay) > 0 || len(s.dirtyMail) > 0)) {
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
			if err == nil && method == "session/close" {
				// An ACP v2 turn is held in memory until idle. Keep it when the
				// session closes first, as a model switch does.
				s.mu.Lock()
				if !s.replay {
					if saveErr := s.save(false); saveErr != nil {
						slog.Error("save ACP transcript on close", "error", saveErr)
					}
				}
				s.mu.Unlock()
			}
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
			if err := s.storePrompt(params); err != nil {
				return nil, err
			}
			return func(err error) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				saveErr := s.save(false)
				if saveErr == nil {
					s.toolTerminals = nil
				}
				if err != nil {
					return err
				}
				return saveErr
			}, nil
		}
	}
	return noop, nil
}

func SteeringInjected(raw json.RawMessage) bool {
	var reply struct{ Outcome string }
	return json.Unmarshal(raw, &reply) == nil && reply.Outcome == "injected"
}

func (s *recordedSession) storePrompt(params Object) error {
	var prompt acpv2.Transcript
	data, _ := json.Marshal(Object{"sessionUpdate": encoded("user_message"), "messageId": encoded("prompt"), "content": params["prompt"]})
	_, _ = prompt.Apply(data)
	for _, e := range prompt.Entries() {
		if err := s.store([]sessions.Message{{Role: "user", Body: e.Text}}, false); err != nil {
			return fmt.Errorf("record ACP prompt: %w", err)
		}
	}
	return nil
}

func (r *transcriptRecorder) recordSteering(key string, params Object) {
	if value, ok := r.active.Load(key); ok {
		s := value.(*recordedSession)
		s.mu.Lock()
		defer s.mu.Unlock()
		err := s.save(false)
		if err == nil {
			err = s.storePrompt(params)
		}
		if err != nil {
			slog.Error("record accepted steering input", "error", err)
		}
	}
}
