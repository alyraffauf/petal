package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"

	"petal/internal/livesession"
	"petal/internal/sessions"
)

type daemonOpened struct {
	snapshot livesession.Snapshot
	history  []entry
	gap      bool
	partial  bool
}
type daemonReplayStart struct{ prompt []entry }
type daemonReplayFinish struct{ active bool }
type daemonNotice struct{ text string }
type daemonDisconnected struct{ err error }

func (h *harness) readDaemon(ctx context.Context, snapshot livesession.Snapshot, fresh bool) {
	for {
		err := h.readDaemonSnapshot(ctx, snapshot, fresh)
		if !daemonError(err, livesession.Gap) {
			h.emitV2(ctx, daemonDisconnected{err})
			return
		}
		snapshot, err = h.daemon.client.Snapshot(ctx)
		if err != nil {
			h.emitV2(ctx, daemonDisconnected{err})
			return
		}
		// Rebuild from the archive and the retained ACP v2 suffix.
		snapshot.After = snapshot.Head
		if snapshot.ACPVersion == 2 {
			snapshot.After = snapshot.Floor
		}
		h.emitV2(ctx, daemonReplayReset{})
		fresh = false
	}
}

type daemonReplayReset struct{}

func (h *harness) readDaemonSnapshot(ctx context.Context, snapshot livesession.Snapshot, fresh bool) error {
	readCtx, stop := context.WithCancel(ctx)
	defer stop()
	d := h.daemon
	cursor := snapshot.After
	gap := cursor > 0
	var archive []entry
	if gap && snapshot.ACPVersion == 2 {
		var err error
		archive, err = h.readArchiveHistory(snapshot.ArchiveHead)
		if err != nil {
			return err
		}
	} else if gap {
		archive = h.archiveHistory(-1)
	} else if snapshot.ACPVersion == 1 && snapshot.ArchiveHead != 0 {
		archive = h.archiveHistory(snapshot.ArchiveHead)
	}
	var active bool
	for cursor < snapshot.Head {
		e, err := d.client.Next(ctx, cursor)
		if err != nil {
			return err
		}
		cursor = e.Sequence
		switch {
		case snapshot.ACPVersion == 1 && e.Kind == livesession.CallStarted && e.Method == "session/prompt":
			if active || !gap {
				h.emitV2(ctx, daemonReplayFinish{})
			}
			active = !gap || e.Operation == snapshot.Turn && snapshot.Running
			if active {
				prompt := replayedPrompt(e.Params)
				if gap && len(prompt) == 1 && len(archive) > 0 && archive[len(archive)-1] == prompt[0] {
					archive = archive[:len(archive)-1]
				}
				h.emitV2(ctx, daemonReplayStart{prompt})
			}
		case snapshot.ACPVersion == 1 && active && e.Kind == livesession.CallFinished && e.Method == "_session/steering":
			for _, row := range replayedPrompt(e.Params) {
				h.emitV2(ctx, event{role: row.Role, text: row.Text})
			}
		case snapshot.ACPVersion == 1 && e.Kind == livesession.CallFinished && e.Method == "session/prompt":
			if active {
				h.emitV2(ctx, daemonReplayFinish{})
			}
			active = false
		case e.Kind == livesession.Update:
			if snapshot.ACPVersion == 2 {
				if !gap || e.Sequence > snapshot.ArchiveCursor || !h.archivedV2Update(e) {
					h.applyDaemonUpdate(ctx, e, true, fresh)
				}
			} else if !gap || active || !transcriptUpdate(e) {
				h.applyDaemonUpdate(ctx, e, true, fresh)
			} else if e.Method == "terminal/output" {
				h.rememberTerminal(e.Params)
			}
		}
	}
	if snapshot.ACPVersion == 1 {
		h.emitV2(ctx, daemonReplayFinish{active: active && snapshot.Running})
	}
	h.emitV2(ctx, daemonOpened{snapshot: snapshot, history: archive, gap: gap, partial: gap && snapshot.ACPVersion == 2 && snapshot.Floor > snapshot.ArchiveCursor})
	questions := daemonQuestions{h: h, active: make(map[uint64]context.CancelFunc)}
	for _, question := range snapshot.Pending {
		questions.open(readCtx, question)
	}
	for {
		e, err := d.client.Next(ctx, cursor)
		if err != nil {
			return err
		}
		cursor = e.Sequence
		questions.apply(readCtx, e)
		switch e.Kind {
		case livesession.Update:
			h.applyDaemonUpdate(ctx, e, false, false)
		case livesession.CallFinished:
			if e.Method == "session/prompt" {
				h.finishDaemonPrompt(ctx, e)
			} else if e.Method == "_session/steering" && e.Operation < snapshot.NextOperation {
				for _, row := range replayedPrompt(e.Params) {
					h.emitV2(ctx, event{role: row.Role, text: row.Text})
				}
			}
		case livesession.CancelFinished:
			var err error
			if e.Outcome != nil {
				err = e.Outcome.Err()
			}
			if err != nil || h.v2.Load() {
				h.emitV2(ctx, daemonCancelFinished{turn: e.Turn, err: err})
			}
		case livesession.AdapterExited, livesession.SessionClosed:
			if e.Kind == livesession.AdapterExited {
				h.discardFailedSession(ctx)
			}
			return io.EOF
		}
	}
}

type daemonCancelFinished struct {
	turn uint64
	err  error
}

func (h *harness) finishDaemonPrompt(ctx context.Context, e livesession.Event) {
	outcome := e.Outcome
	if outcome == nil {
		receipt, err := h.daemon.client.Result(ctx, e.Operation)
		if err != nil {
			outcome = &livesession.Outcome{Error: &livesession.Error{Message: err.Error()}}
		} else {
			outcome = &receipt.Outcome
		}
	}
	h.emitV2(ctx, h.promptResult(outcome.Result, outcome.Err()))
}

func (h *harness) promptResult(raw json.RawMessage, err error) tea.Msg {
	if h.v2.Load() {
		var result struct{ MessageID *string }
		if err == nil && (json.Unmarshal(raw, &result) != nil || result.MessageID == nil) {
			err = errors.New("ACP v2 prompt response requires a messageId")
		}
		return promptV2Result{err}
	}
	var result acp.PromptResponse
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return finished{err, string(result.StopReason)}
}

func (h *harness) archiveHistory(head int64) []entry {
	history, _ := h.readArchiveHistory(head)
	return history
}

func (h *harness) readArchiveHistory(head int64) ([]entry, error) {
	if head == 0 {
		return nil, nil
	}
	rows, err := sessions.Transcript(h.cwd, h.id)
	if err != nil {
		return nil, err
	}
	history := make([]entry, 0, len(rows))
	for _, row := range rows {
		if head >= 0 && row.Rowid > head {
			break
		}
		history = append(history, entry{row.Role, row.Body})
	}
	return history, nil
}

func replayedPrompt(params json.RawMessage) []entry {
	var input struct{ Prompt []json.RawMessage }
	_ = json.Unmarshal(params, &input)
	var text []string
	for _, block := range input.Prompt {
		if value := contentText(block); value != "" {
			text = append(text, value)
		}
	}
	if len(text) == 0 {
		return nil
	}
	return []entry{{"user", strings.Join(text, "\n")}}
}

func transcriptUpdate(e livesession.Event) bool {
	if e.Method != "session/update" {
		return false
	}
	switch daemonUpdateKind(e.Params) {
	case "available_commands_update", "config_option_update", "current_mode_update", "usage_update",
		"session_info_update", "notice", "subagent_update", "session_message", "session_message_chunk", "plan", "plan_update", "plan_removed":
		return false
	}
	return true
}

func daemonUpdateKind(raw json.RawMessage) string {
	var update struct {
		Update struct{ SessionUpdate string }
	}
	_ = json.Unmarshal(raw, &update)
	return update.Update.SessionUpdate
}

func (h *harness) archivedV2Update(e livesession.Event) bool {
	if e.Method != "session/update" || !isV2TranscriptUpdate(daemonUpdateKind(e.Params)) {
		return false
	}
	var params struct{ SessionID string }
	return json.Unmarshal(e.Params, &params) == nil && params.SessionID == h.id
}

func (h *harness) applyDaemonUpdate(ctx context.Context, e livesession.Event, replay, fresh bool) {
	switch e.Method {
	case "session/update":
		kind := daemonUpdateKind(e.Params)
		if replay && (kind == "state_update" || kind == "replay_complete" || (kind == "session_info_update" && !fresh) ||
			kind == "config_option_update" || kind == "current_mode_update") {
			return
		}
		if h.v2.Load() && kind == "state_update" {
			_, _ = h.updateV2(ctx, e.Params, e.Turn)
			return
		}
		_, _ = h.callback(ctx, e.Method, e.Params)
	case "terminal/output":
		h.rememberTerminal(e.Params)
	}
}

func (h *harness) rememberTerminal(raw json.RawMessage) {
	var released struct {
		livesession.TerminalRef
		acp.TerminalOutputResponse
	}
	if json.Unmarshal(raw, &released) == nil {
		h.daemon.outputs.Store(released.TerminalRef, released.TerminalOutputResponse)
	}
}

func (h *harness) terminalView(sessionID, terminalID string) func() acp.TerminalOutputResponse {
	if h.host != nil {
		return h.host.OutputView(sessionID, terminalID)
	}
	if h.daemon == nil {
		return nil
	}
	ref := livesession.TerminalRef{SessionID: sessionID, TerminalID: terminalID}
	return func() acp.TerminalOutputResponse {
		if output, ok := h.daemon.outputs.Load(ref); ok {
			return output.(acp.TerminalOutputResponse)
		}
		ctx, cancel := context.WithTimeout(h.lifetime, 250*time.Millisecond)
		defer cancel()
		output, _ := h.daemon.client.TerminalOutput(ctx, ref)
		return output
	}
}
