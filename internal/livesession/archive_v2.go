//go:build linux || darwin

package livesession

import (
	"context"
	"encoding/json"
	"log/slog"

	"petal/internal/acpv2"
	"petal/internal/sessions"
)

type archiveV2 struct {
	projection acpv2.Transcript
	setup      []archiveBatch
	deleted    bool
}

type archiveBatch struct {
	messages []sessions.Message
	replay   bool
	cursor   uint64
}

type setupArchiveV2 struct {
	replay, live acpv2.Transcript
	cursor       uint64
	completed    []sessions.Message
	idleCursor   uint64
}

func (a *setupArchiveV2) observe(raw json.RawMessage, kind, state string, sequence uint64) {
	if kind == "replay_complete" && a.cursor == 0 {
		a.cursor = sequence
		return
	}
	if a.cursor == 0 {
		projectV2(&a.replay, raw)
		return
	}
	if kind == "state_update" && state == "idle" {
		a.completed = append(a.completed, sessions.ProjectTranscript(&a.live, true)...)
		a.live.Reset()
		a.idleCursor = sequence
		return
	}
	projectV2(&a.live, raw)
}

func projectV2(projection *acpv2.Transcript, raw json.RawMessage) string {
	var update struct {
		Update map[string]json.RawMessage
	}
	if json.Unmarshal(raw, &update) != nil {
		return ""
	}
	changed, _ := projection.ApplyFields(update.Update, 2)
	var id string
	if changed {
		_ = json.Unmarshal(update.Update["compactionId"], &id)
	}
	return id
}

func (s *session) observeV2(raw json.RawMessage, kind, state string, sequence uint64) {
	if kind == "state_update" && state == "idle" {
		s.checkpointV2(sequence)
		return
	}
	id := projectV2(&s.archiveV2.projection, raw)
	if id != "" && len(s.archiveV2.setup) == 0 && (kind == "compaction_update" || kind == "compaction_summary_chunk") {
		if entry, ok := s.archiveV2.projection.FinishedCompaction(id); ok {
			message := sessions.Message{Role: entry.Role, Body: entry.Text, SourceID: id}
			if err := sessions.SaveTranscript(s.info.Cwd, s.info.SessionID, []sessions.Message{message}, false); err != nil {
				slog.Error("save ACP compaction", "error", err)
			}
		}
	}
}

func (s *session) commitReplayV2(staged *setupArchiveV2, base uint64) {
	s.archiveV2.projection = staged.live
	s.archiveV2.setup = append(s.archiveV2.setup, archiveBatch{
		messages: sessions.ProjectTranscript(&staged.replay, true),
		replay:   true, cursor: base + staged.cursor,
	})
	if staged.idleCursor != 0 {
		s.archiveV2.setup = append(s.archiveV2.setup, archiveBatch{
			messages: staged.completed, cursor: base + staged.idleCursor,
		})
	}
	s.checkpointSetupV2()
}

// Retry failed setup writes before advancing the cursor to a live checkpoint.
func (s *session) checkpointSetupV2() bool {
	for len(s.archiveV2.setup) != 0 {
		batch := s.archiveV2.setup[0]
		if !s.saveCheckpointV2(batch.messages, batch.replay, batch.cursor) {
			return false
		}
		s.archiveV2.setup[0] = archiveBatch{}
		s.archiveV2.setup = s.archiveV2.setup[1:]
	}
	return true
}

func (s *session) checkpointV2(sequence uint64) {
	if s.checkpointSetupV2() && s.saveCheckpointV2(sessions.ProjectTranscript(&s.archiveV2.projection, true), false, sequence) {
		s.archiveV2.projection.Reset()
	}
}

func (s *session) saveCheckpointV2(messages []sessions.Message, replay bool, sequence uint64) bool {
	id, cwd := s.info.SessionID, s.info.Cwd
	if err := sessions.SaveTranscript(cwd, id, messages, replay); err != nil {
		slog.Error("save ACP transcript", "error", err)
		return false
	}
	head, err := sessions.TranscriptHead(context.Background(), cwd, id)
	if err != nil {
		slog.Error("read ACP transcript checkpoint", "error", err)
		return false
	}
	s.archiveHead, s.archiveCursor = head, sequence
	return true
}
