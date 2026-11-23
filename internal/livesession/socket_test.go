//go:build linux || darwin

package livesession

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"petal/internal/localdaemon"
	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestLiveSessionDetachOverUnixSocket(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx, dial := startSocket(t, version, false)
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "source"), []byte("workspace contents"), 0600); err != nil {
				t.Fatal(err)
			}
			controller := dial()
			opts := OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture"}
			setup := openSession(t, ctx, controller, opts, version)
			sessionID := setup.Info.SessionID
			record, err := sessions.Lookup(cwd, sessionID)
			if err != nil || record.ID != sessionID {
				t.Fatalf("unprompted session record = %+v, %v", record, err)
			}
			observer := dial()
			if _, err := observer.Attach(ctx, AttachOptions{Handle: opts.Handle, Control: true}); !hasCode(err, Conflict) {
				t.Fatalf("second controller: %v", err)
			}
			observed, err := observer.Attach(ctx, AttachOptions{SessionID: sessionID})
			if err != nil || observed.Control {
				t.Fatalf("observer attachment = %+v, %v", observed, err)
			}
			if _, err := observer.Submit(ctx, Call{Operation: 3, Method: "session/prompt"}); !hasCode(err, ReadOnly) {
				t.Fatalf("observer prompt: %v", err)
			}
			retry, err := observer.Open(ctx, opts)
			if err != nil || retry.SessionID != sessionID {
				t.Fatalf("open retry = %+v, %v", retry, err)
			}
			if _, err := observer.Open(ctx, OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture", ResumeID: sessionID}); !hasCode(err, Conflict) {
				t.Fatalf("duplicate adapter owner: %v", err)
			}

			prompt := Call{Operation: 3, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"input"}]}`)}
			submit(t, ctx, controller, prompt)
			cursor := setup.Head
			before := advance(t, ctx, controller, &cursor, func(event Event) bool {
				return event.Kind == Update && strings.Contains(string(event.Params), "before ")
			})
			if err := controller.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			observerCursor := before.Sequence
			questionEvent := advance(t, ctx, observer, &observerCursor, func(event Event) bool { return event.Kind == QuestionOpened })
			question := questionEvent.Question
			if err := observer.Answer(ctx, Answer{QuestionID: question.ID, OptionID: "allow"}); !hasCode(err, ReadOnly) {
				t.Fatalf("observer answer: %v", err)
			}

			reconnected := dial()
			snapshot := takeControl(t, ctx, reconnected, AttachOptions{SessionID: sessionID, After: &cursor, Control: true})
			if !snapshot.Running || snapshot.SessionID != sessionID || snapshot.Turn != 3 ||
				len(snapshot.Pending) != 1 || snapshot.Pending[0].ID != question.ID {
				t.Fatalf("reattached state = %+v", snapshot)
			}
			for cursor < snapshot.Head {
				advance(t, ctx, reconnected, &cursor, func(Event) bool { return true })
			}
			seam := dial()
			start := setup.Head
			attach := AttachOptions{SessionID: sessionID, After: &start}
			replay, err := seam.Attach(ctx, attach)
			if err != nil {
				t.Fatal(err)
			}

			submit(t, ctx, reconnected, prompt)
			conflicting := prompt
			conflicting.Params = json.RawMessage(`{"prompt":[]}`)
			if _, err := reconnected.Submit(ctx, conflicting); !hasCode(err, Conflict) {
				t.Fatalf("conflicting retry: %v", err)
			}
			answer := Answer{QuestionID: question.ID, OptionID: "allow"}
			if err := reconnected.Answer(ctx, answer); err != nil {
				t.Fatal(err)
			}
			if err := reconnected.Answer(ctx, answer); err != nil {
				t.Fatalf("idempotent answer retry: %v", err)
			}
			if err := reconnected.Answer(ctx, Answer{QuestionID: answer.QuestionID, OptionID: "deny"}); !hasCode(err, Conflict) {
				t.Fatalf("conflicting answer retry: %v", err)
			}
			if version == 1 {
				for _, want := range []string{"Write", "Run"} {
					event := advance(t, ctx, reconnected, &cursor, func(event Event) bool { return event.Kind == QuestionOpened })
					if event.Question.Method != "host/approve" || !strings.Contains(string(event.Question.Params), want) {
						t.Fatalf("host approval = %+v", event.Question)
					}
					if err := reconnected.Answer(ctx, Answer{QuestionID: event.Question.ID, OptionID: "allow"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			after := advance(t, ctx, reconnected, &cursor, func(event Event) bool {
				return event.Kind == Update && strings.Contains(string(event.Params), "after")
			})
			again, err := reconnected.Next(ctx, after.Sequence-1)
			if err != nil || again.Sequence != after.Sequence || string(again.Params) != string(after.Params) {
				t.Fatalf("same-cursor replay = %+v, %v", again, err)
			}
			advance(t, ctx, reconnected, &cursor, func(event Event) bool {
				return version == 1 && event.Kind == CallFinished && event.Operation == 3 ||
					version == 2 && event.Kind == Update && strings.Contains(string(event.Params), `"state":"idle"`)
			})
			requireResult(t, ctx, reconnected, 3)
			// The adapter already emitted the live tail. A later read must
			// cross the original replay boundary exactly once and in order.
			retryAttach, err := seam.Attach(ctx, attach)
			if err != nil || retryAttach.Head != replay.Head {
				t.Fatalf("attach retry moved replay boundary: %+v, %v", retryAttach, err)
			}
			seamCursor := replay.After
			openings, closures := 0, 0
			advance(t, ctx, seam, &seamCursor, func(event Event) bool {
				if event.Kind == QuestionOpened && event.Question.ID == question.ID {
					openings++
				}
				if event.Kind == QuestionClosed && event.QuestionID == question.ID {
					closures++
				}
				return event.Sequence == after.Sequence
			})
			if openings != 1 || closures != 1 || questionEvent.Sequence > replay.Head || after.Sequence <= replay.Head {
				t.Fatalf("replay/live boundary %d: question %d (%d/%d), tail %d", replay.Head, questionEvent.Sequence, openings, closures, after.Sequence)
			}
			rows, err := sessions.Transcript(cwd, sessionID)
			if err != nil || len(rows) == 0 || rows[0].Body != "input" {
				t.Fatalf("archived transcript = %+v, %v", rows, err)
			}
			if version == 2 && (len(rows) != 2 || rows[1].Body != "before after") {
				t.Fatalf("archived v2 response = %+v", rows)
			}
			if version == 1 {
				if len(rows) != 4 || rows[1].Body != "before " || rows[2].Role != "tool" || rows[3].Body != "after" {
					t.Fatalf("archived v1 response = %+v", rows)
				}
				hits, err := sessions.Search(cwd, "", "workspace contents", 0, 10)
				if err != nil || len(hits) != 1 || hits[0].Session != sessionID || hits[0].Row != rows[2].Rowid {
					t.Fatalf("terminal output search = %+v, %v", hits, err)
				}
				written, err := os.ReadFile(filepath.Join(cwd, "written"))
				if err != nil || string(written) != "workspace contents" {
					t.Fatalf("host write = %q, %v", written, err)
				}
			}

			cancelPrompt := Call{Operation: 4, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"cancel"}]}`)}
			submit(t, ctx, reconnected, cancelPrompt)
			if err := reconnected.Cancel(ctx, 4); err != nil {
				t.Fatal(err)
			}
			if err := reconnected.Cancel(ctx, 4); err != nil {
				t.Fatalf("cancel retry: %v", err)
			}
			finished, adapterCancelled := false, false
			advance(t, ctx, reconnected, &cursor, func(event Event) bool {
				finished = finished || event.Kind == CallFinished && event.Operation == 4
				adapterCancelled = adapterCancelled || event.Kind == Update && strings.Contains(string(event.Params), "adapter-cancelled")
				return finished && adapterCancelled
			})
			requireResult(t, ctx, reconnected, 4)
			if err := reconnected.CloseSession(ctx); err != nil {
				t.Fatal(err)
			}
			if err := reconnected.CloseSession(ctx); err != nil {
				t.Fatalf("close retry: %v", err)
			}
			advance(t, ctx, reconnected, &cursor, func(event Event) bool { return event.Kind == SessionClosed })

			newController := dial()
			opts.Handle, opts.ResumeID = NewHandle(), sessionID
			for {
				_, err := newController.Open(ctx, opts)
				if err == nil {
					break
				}
				if !hasCode(err, Conflict) || ctx.Err() != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Millisecond)
			}
			resumed := openSession(t, ctx, newController, opts, version)
			if resumed.SessionID != sessionID {
				t.Fatalf("reopened ID = %q, want %q", resumed.SessionID, sessionID)
			}
		})
	}
}

func TestResumedSettingsGateAndV2CancellationSurviveReconnect(t *testing.T) {
	ctx, dial := startSocket(t, 2, false)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "resume-running"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	const id = `["fake","fixture","thread"]`
	opts := OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture", ResumeID: id}
	client := dial()
	resumed := openSession(t, ctx, client, opts, 2)
	if !resumed.SettingsPending || !resumed.Running || !resumed.TurnStarted || resumed.Turn == 0 || resumed.ActiveOperation != 0 {
		t.Fatalf("resumed state = %+v", resumed)
	}
	rows, err := sessions.Transcript(cwd, id)
	if err != nil || resumed.ArchiveCursor == 0 || resumed.ArchiveHead == 0 ||
		len(rows) != 1 || rows[0].Body != "resumed-history" {
		t.Fatalf("resume checkpoint must exclude active updates: %+v, rows %+v, %v", resumed, rows, err)
	}
	archiveCursor := resumed.ArchiveCursor
	tail := advance(t, ctx, client, &archiveCursor, func(event Event) bool {
		return strings.Contains(string(event.Params), "resumed-active")
	})
	if tail.Sequence > resumed.Head {
		t.Fatalf("active tail should be retained at the setup snapshot: %+v", tail)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	client = dial()
	reconnected := takeControl(t, ctx, client, AttachOptions{Handle: opts.Handle, Control: true})
	if !reconnected.SettingsPending || reconnected.Turn != resumed.Turn {
		t.Fatalf("reconnected state = %+v", reconnected)
	}
	prompt := Call{Operation: reconnected.NextOperation, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"cancel"}]}`)}
	if _, err := client.Submit(ctx, prompt); !hasCode(err, Conflict) {
		t.Fatalf("prompt bypassed required settings review: %v", err)
	}
	if err := client.ConfirmSettings(ctx, reconnected.SettingsRevision-1); !hasCode(err, Conflict) {
		t.Fatalf("confirmation accepted a stale revision: %v", err)
	}
	if err := client.Cancel(ctx, resumed.Turn); err != nil {
		t.Fatal(err)
	}
	cursor := reconnected.Head
	idle, cancelled := false, false
	advance(t, ctx, client, &cursor, func(event Event) bool {
		idle = idle || strings.Contains(string(event.Params), `"state":"idle"`)
		cancelled = cancelled || event.Kind == CancelFinished && event.Turn == resumed.Turn
		return idle && cancelled
	})
	if _, err := client.Submit(ctx, prompt); !hasCode(err, Conflict) {
		t.Fatalf("prompt bypassed settings review after idle: %v", err)
	}
	if err := client.ConfirmSettings(ctx, reconnected.SettingsRevision); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	client = dial()
	confirmed := takeControl(t, ctx, client, AttachOptions{Handle: opts.Handle, Control: true})
	if confirmed.SettingsPending {
		t.Fatalf("confirmation lost on reconnect: %+v", confirmed)
	}
	submit(t, ctx, client, prompt)
	cursor = confirmed.Head
	started := advance(t, ctx, client, &cursor, func(event Event) bool { return strings.Contains(string(event.Params), `"state":"running"`) })
	if started.Turn == 0 || started.Turn == resumed.Turn {
		t.Fatalf("new turn identity = %d", started.Turn)
	}
	if err := client.Cancel(ctx, resumed.Turn); !hasCode(err, NotFound) {
		t.Fatalf("cancel by previous turn ID: %v", err)
	}
	state, err := client.Snapshot(ctx)
	if err != nil || !state.Running || state.Turn != started.Turn {
		t.Fatalf("stale cancellation changed the current turn: %+v, %v", state, err)
	}
	if err := client.Cancel(ctx, started.Turn); err != nil {
		t.Fatal(err)
	}
	advance(t, ctx, client, &cursor, func(event Event) bool { return strings.Contains(string(event.Params), "adapter-cancelled") })
}

func TestLiveV2AdapterExitClearsRunningState(t *testing.T) {
	ctx, dial := startSocket(t, 2, false)
	client := dial()
	setup := openSession(t, ctx, client, OpenOptions{Handle: NewHandle(), Cwd: t.TempDir(), Model: "fixture"}, 2)
	submit(t, ctx, client, Call{Operation: 3, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"exit"}]}`)})
	cursor := setup.Head
	advance(t, ctx, client, &cursor, func(event Event) bool { return event.Kind == AdapterExited })
	snapshot, err := client.Snapshot(ctx)
	if err != nil || !snapshot.AdapterExited || snapshot.Running {
		t.Fatalf("adapter exit state = %+v, %v", snapshot, err)
	}
	if _, err := client.Submit(ctx, Call{Operation: 4, Method: "session/prompt"}); !hasCode(err, Closed) {
		t.Fatalf("new prompt on disconnected adapter: %v", err)
	}
}

func hasCode(err error, want Code) bool {
	var protocolErr *Error
	return errors.As(err, &protocolErr) && protocolErr.Code == want
}

func startSocket(t *testing.T, version int, delegation bool) (context.Context, func() *Client) {
	t.Helper()
	return startSocketWithGrace(t, version, delegation, OpenReservationGrace)
}

func startSocketWithGrace(t *testing.T, version int, delegation bool, grace time.Duration) (context.Context, func() *Client) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := os.MkdirTemp("/tmp", "petal-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	endpoint, err := localdaemon.Discover(root)
	if err != nil {
		// A Nix user namespace can have an unmapped owner for /. It
		// cannot provide a trusted Unix socket path. CI runs outside it too.
		var stat unix.Stat_t
		if runtime.GOOS == "linux" && os.Getenv("NIX_BUILD_TOP") != "" &&
			unix.Stat("/", &stat) == nil && stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
			t.Skip("daemon requires a trusted runtime path outside the Nix sandbox")
		}
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture := func(model string) routing.Account {
		return routing.Account{
			Models: map[string][]string{model: {executable, "-test.run=^TestLiveAdapterFixture$"}},
			Env:    map[string]string{"PETAL_LIVE_FIXTURE": strconv.Itoa(version), "PETAL_LIVE_MODEL": model},
		}
	}
	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	server := NewServer(ctx, func(opts OpenOptions) (routing.Config, error) {
		return routing.Config{
			Model: opts.Model, PinnedAccount: opts.Account, ACPv2: version == 2, Delegation: delegation,
			Accounts: map[string]routing.Account{"fake": fixture("fixture"), "child": fixture("child")},
		}, nil
	})
	server.reservationGrace = grace
	done := make(chan error, 1)
	go func() { done <- endpoint.Serve(ctx, server.Handler()) }()
	t.Cleanup(func() {
		stop()
		server.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	dial := func() *Client {
		t.Helper()
		for {
			conn, err := endpoint.Dial(ctx)
			if err == nil {
				client, err := Connect(ctx, conn)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = client.Close() })
				return client
			}
			if ctx.Err() != nil || !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
	}
	return ctx, dial
}

func advance(t *testing.T, ctx context.Context, client *Client, cursor *uint64, until func(Event) bool) Event {
	t.Helper()
	for {
		event, err := client.Next(ctx, *cursor)
		if err != nil || event.Sequence != *cursor+1 {
			t.Fatalf("after %d: event %+v, %v", *cursor, event, err)
		}
		*cursor = event.Sequence
		if until(event) {
			return event
		}
	}
}

func submit(t *testing.T, ctx context.Context, client *Client, call Call) {
	t.Helper()
	if number, err := client.Submit(ctx, call); err != nil || number != call.Operation {
		t.Fatalf("submit %s = %d, %v", call.Method, number, err)
	}
}

func requireResult(t *testing.T, ctx context.Context, client *Client, number uint64) json.RawMessage {
	t.Helper()
	receipt, err := client.Result(ctx, number)
	if err != nil || !receipt.Done || receipt.Error != nil || receipt.ACPError != nil {
		t.Fatalf("call %d receipt = %+v, %v", number, receipt, err)
	}
	return receipt.Result
}

func takeControl(t *testing.T, ctx context.Context, client *Client, opts AttachOptions) Snapshot {
	t.Helper()
	for {
		snapshot, err := client.Attach(ctx, opts)
		if err == nil {
			return snapshot
		}
		if !hasCode(err, Conflict) || ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
}

func openSession(t *testing.T, ctx context.Context, client *Client, opts OpenOptions, version int) Snapshot {
	t.Helper()
	info, err := client.Open(ctx, opts)
	if err != nil || info.Handle != opts.Handle {
		t.Fatalf("open = %+v, %v", info, err)
	}
	attached := takeControl(t, ctx, client, AttachOptions{Handle: opts.Handle, Control: true})
	cursor := attached.Head
	for index, method := range []string{"initialize", "session/open"} {
		op := uint64(index + 1)
		submit(t, ctx, client, Call{Operation: op, Method: method, Params: json.RawMessage(`{"cwd":"/ignored"}`)})
		receipt, err := client.WaitResult(ctx, op)
		if err != nil || !receipt.Done || receipt.Err() != nil {
			t.Fatalf("call %d wait = %+v, %v", op, receipt, err)
		}
		advance(t, ctx, client, &cursor, func(event Event) bool { return event.Kind == CallFinished && event.Operation == op })
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil || int(snapshot.ACPVersion) != version || snapshot.SessionID != `["fake","fixture","thread"]` {
		t.Fatalf("setup snapshot = %+v, %v", snapshot, err)
	}
	return snapshot
}

func TestSteeringAcrossReceiptEviction(t *testing.T) {
	ctx, dial := startSocket(t, 1, false)
	client := dial()
	opts := OpenOptions{Handle: NewHandle(), Cwd: t.TempDir(), Model: "fixture"}
	setup := openSession(t, ctx, client, opts, 1)
	if err := sessions.SaveTranscript(opts.Cwd, setup.SessionID, []sessions.Message{{Role: "assistant", Body: "prior turn"}}, false); err != nil {
		t.Fatal(err)
	}
	priorHead, err := sessions.TranscriptHead(ctx, opts.Cwd, setup.SessionID)
	if err != nil || priorHead == 0 {
		t.Fatalf("prior archive boundary = %d, %v", priorHead, err)
	}
	prompt := Call{Operation: 3, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"hold for steering"}]}`)}
	submit(t, ctx, client, prompt)
	cursor := setup.Head
	started := advance(t, ctx, client, &cursor, func(e Event) bool { return e.Kind == CallStarted && e.Operation == prompt.Operation })
	if started.ArchiveHead == nil || *started.ArchiveHead != priorHead {
		t.Fatalf("prompt replay boundary = %v, want %d", started.ArchiveHead, priorHead)
	}
	advance(t, ctx, client, &cursor, func(e Event) bool { return strings.Contains(string(e.Params), "ready for steering") })
	for i := range receiptCount + 2 {
		text := "defer input"
		if i == 0 {
			text = "accepted input"
		}
		call := Call{Operation: uint64(4 + i), Method: "_session/steering", Params: bytesOf(map[string]any{"prompt": []map[string]string{{"type": "text", "text": text}}})}
		submit(t, ctx, client, call)
		if receipt, err := client.WaitResult(ctx, call.Operation); err != nil || receipt.Err() != nil {
			t.Fatalf("steering: %+v, %v", receipt, err)
		}
	}
	submit(t, ctx, client, prompt)
	if err := client.Cancel(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if receipt, err := client.WaitResult(ctx, 3); err != nil || receipt.Err() != nil {
		t.Fatalf("prompt: %+v, %v", receipt, err)
	}
	count := 0
	advance(t, ctx, client, &cursor, func(e Event) bool {
		if e.Kind == CallFinished && e.Method == "_session/steering" && len(e.Params) > 0 {
			if !strings.Contains(string(e.Params), "accepted input") {
				t.Fatalf("unexpected replay: %s", e.Params)
			}
			count++
		}
		return e.Kind == CallFinished && e.Operation == 3
	})
	rows, err := sessions.Transcript(opts.Cwd, setup.SessionID)
	if count != 1 || err != nil || len(rows) != 4 || rows[1].Rowid <= priorHead || rows[3].Role != "user" || rows[3].Body != "accepted input" {
		t.Fatalf("replayed %d accepted inputs, archive %+v, error %v", count, rows, err)
	}
}

func TestLiveV2ReattachesAfterPromptReplyBeforeRunning(t *testing.T) {
	ctx, dial := startSocket(t, 2, false)
	client := dial()
	setup := openSession(t, ctx, client, OpenOptions{Handle: NewHandle(), Cwd: t.TempDir(), Model: "fixture"}, 2)
	prompt := Call{Operation: setup.NextOperation, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"reply-before-running"}]}`)}
	submit(t, ctx, client, prompt)
	waitForResult(t, ctx, client, prompt.Operation)
	cursor := setup.Head
	idle := func(event Event) bool {
		return event.Kind == Update && strings.Contains(string(event.Params), `"state":"idle"`)
	}
	advance(t, ctx, client, &cursor, idle)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	reconnected := dial()
	snapshot := takeControl(t, ctx, reconnected, AttachOptions{Handle: setup.Handle, Control: true})
	if !snapshot.AwaitingStart || snapshot.Running || snapshot.Turn != prompt.Operation || snapshot.ActiveOperation != 0 {
		t.Fatalf("acknowledged prompt state = %+v", snapshot)
	}
	followUp := Call{Operation: snapshot.NextOperation, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"list overlap"}]}`)}
	if _, err := reconnected.Submit(ctx, followUp); !hasCode(err, Busy) {
		t.Fatalf("follow-up while awaiting start: %v", err)
	}
	// Release the adapter after observing the reattached state and checking
	// admission. The adapter emits running and idle before the list response.
	submit(t, ctx, reconnected, Call{Operation: followUp.Operation, Method: "session/list"})
	waitForResult(t, ctx, reconnected, followUp.Operation)
	advance(t, ctx, reconnected, &cursor, idle)
	followUp.Operation++
	submit(t, ctx, reconnected, followUp)
	waitForResult(t, ctx, reconnected, followUp.Operation)
}

func TestLivePromptWhileSessionListIsPending(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx, dial := startSocket(t, version, false)
			client := dial()
			cwd := t.TempDir()
			setup := openSession(t, ctx, client, OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture"}, version)
			list := setup.NextOperation
			submit(t, ctx, client, Call{Operation: list, Method: "session/list"})
			cursor := setup.Head
			advance(t, ctx, client, &cursor, func(event Event) bool {
				return event.Kind == Update && strings.Contains(string(event.Params), "list waiting for prompt")
			})
			receipt, err := client.Result(ctx, list)
			if err != nil || receipt.Done {
				t.Fatalf("list must still be pending: %+v, %v", receipt, err)
			}
			prompt := Call{Operation: list + 1, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"list overlap"}]}`)}
			submit(t, ctx, client, prompt)
			waitForResult(t, ctx, client, prompt.Operation)
			waitForResult(t, ctx, client, list)
			if version == 2 {
				advance(t, ctx, client, &cursor, func(event Event) bool {
					return event.Kind == Update && strings.Contains(string(event.Params), `"state":"idle"`)
				})
			}
			rows, err := sessions.Transcript(cwd, setup.SessionID)
			if err != nil || len(rows) != 2 || rows[0].Role != "user" || rows[0].Body != "list overlap" || rows[1].Body != "prompt during list" {
				t.Fatalf("transcript after overlap = %+v, %v", rows, err)
			}
		})
	}
}

func waitForResult(t *testing.T, ctx context.Context, client *Client, operation uint64) {
	t.Helper()
	receipt, err := client.WaitResult(ctx, operation)
	if err != nil || !receipt.Done || receipt.Err() != nil {
		t.Fatalf("call %d receipt = %+v, %v", operation, receipt, err)
	}
}

func TestResumeForkWhileParentIsAttached(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx, dial := startSocket(t, version, false)
			cwd := t.TempDir()
			parent := dial()
			root := openSession(t, ctx, parent, OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture"}, version)
			submit(t, ctx, parent, Call{Operation: 3, Method: "session/fork"})
			result, err := parent.WaitResult(ctx, 3)
			if err != nil || result.Err() != nil {
				t.Fatalf("fork: %+v, %v", result, err)
			}
			var fork struct{ SessionID string }
			if err := json.Unmarshal(result.Outcome.Result, &fork); err != nil || fork.SessionID == "" {
				t.Fatalf("fork ID: %s, %v", result.Outcome.Result, err)
			}
			child := dial()
			opts := OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture", ResumeID: fork.SessionID}
			if _, err := child.Open(ctx, opts); err != nil {
				t.Fatal(err)
			}
			takeControl(t, ctx, child, AttachOptions{Handle: opts.Handle, Control: true})
			for i, method := range []string{"initialize", "session/open"} {
				op := uint64(i + 1)
				submit(t, ctx, child, Call{Operation: op, Method: method})
				res, err := child.WaitResult(ctx, op)
				if err != nil || res.Err() != nil {
					t.Fatalf("%s: %+v, %v", method, res, err)
				}
			}
			snap, err := child.Snapshot(ctx)
			if err != nil || snap.SessionID != fork.SessionID {
				t.Fatalf("resumed: %+v, %v", snap, err)
			}
			snap, err = parent.Snapshot(ctx)
			if err != nil || snap.SessionID != root.SessionID || snap.Closed {
				t.Fatalf("parent: %+v, %v", snap, err)
			}
		})
	}
}
