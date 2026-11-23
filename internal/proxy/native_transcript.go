package proxy

import (
	"log/slog"
	"strconv"

	"petal/internal/acpv2"
	"petal/internal/sessions"
)

type nativeChild struct {
	title, last string
	sequence    int
	output      acpv2.Transcript
}

// A local ACP v1 backend serves one root conversation. Its native children
// publish on their own session IDs, but their searchable output belongs to the root.
func (p *proxy) observeNativeTranscript(account, native string, u Object) bool {
	kind := stringField(u, "sessionUpdate")
	root, found := p.recorder.active.Load(p.publicID(account, native))
	if found && kind != "subagent_update" && kind != "session_message" && kind != "session_message_chunk" {
		return false
	}
	if !found {
		p.recorder.active.Range(func(_, value any) bool {
			candidate := value.(*recordedSession)
			if candidate.record.Account != account {
				return true
			}
			root = value
			return false
		})
	}
	if root == nil {
		return false
	}
	s := root.(*recordedSession)
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []sessions.Message
	switch {
	case kind == "subagent_update":
		id := stringField(u, "sessionId")
		if id == "" {
			return true
		}
		child := s.nativeChild(id)
		if _, ok := u["title"]; ok {
			child.title = stringField(u, "title")
		}
		if stringField(u.member("state"), "state") == "idle" {
			title := child.title
			if title == "" {
				title = id
			}
			for _, entry := range child.output.Entries() {
				rows = append(rows, sessions.Message{Role: "subagent", Body: "**" + title + "** · " + entry.Role + "\n\n" + entry.Text})
			}
			child.output.Reset()
			child.last = ""
		}
	case !found:
		if kind == "user_message_chunk" || kind == "agent_message_chunk" {
			child := s.nativeChild(native)
			if stringField(u, "messageId") == "" {
				if child.last != kind {
					child.sequence++
					child.last = kind
				}
				u["messageId"] = encoded(strconv.Itoa(child.sequence))
			}
			_, _ = child.output.ApplyFields(u, 1)
		}
	case kind == "session_message" || kind == "session_message_chunk":
		if len(u["content"]) == 0 {
			return true
		}
		projected := Object{"sessionUpdate": encoded("agent_message"), "messageId": u["messageId"], "content": u["content"]}
		if kind == "session_message_chunk" {
			projected["sessionUpdate"] = encoded("agent_message_chunk")
		}
		id := stringField(u, "messageId")
		if id == "" {
			return true
		}
		if s.mail == nil {
			s.mail = make(map[string]*acpv2.Transcript)
		}
		if s.mail[id] == nil {
			s.mail[id] = acpv2.NewTranscript()
		}
		if changed, _ := s.mail[id].ApplyFields(projected, 1); changed && !s.mailPending[id] {
			if s.mailPending == nil {
				s.mailPending = make(map[string]bool)
			}
			s.mailPending[id] = true
			s.dirtyMail = append(s.dirtyMail, id)
		}
		if kind == "session_message" && !s.replay {
			rows = s.mailRows()
		}
	}
	if s.replay {
		s.nativeReplay = append(s.nativeReplay, rows...)
	} else if err := s.store(rows, false); err != nil {
		slog.Error("archive native agent output", "error", err)
	} else if kind == "session_message" && found {
		s.clearDirtyMail()
	}
	return true
}

func (s *recordedSession) nativeChild(id string) *nativeChild {
	if s.children == nil {
		s.children = make(map[string]*nativeChild)
	}
	child := s.children[id]
	if child == nil {
		child = &nativeChild{}
		s.children[id] = child
	}
	return child
}

func (s *recordedSession) mailRows() []sessions.Message {
	var rows []sessions.Message
	for _, id := range s.dirtyMail {
		s.mail[id].Visit(func(id string, entry acpv2.Entry) {
			rows = append(rows, sessions.Message{Role: "message", Body: entry.Text, SourceID: id})
		})
	}
	return rows
}

func (s *recordedSession) clearDirtyMail() { s.dirtyMail = nil; clear(s.mailPending) }
