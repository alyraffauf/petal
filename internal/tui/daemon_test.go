package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"bloomfx.dev/bloom"

	"petal/internal/livesession"
	"petal/internal/routing"
)

func TestDaemonV2ReattachKeepsAcknowledgedPromptBusy(t *testing.T) {
	h := &harness{daemon: &daemonSession{}}
	h.v2.Store(true)
	m := &terminal{root: bloom.New(), h: h, ctx: t.Context()}
	m.outbox = []promptMessage{{text: "follow-up"}}

	m.Update(daemonOpened{snapshot: livesession.Snapshot{
		Foreground: "idle", AwaitingStart: true, Turn: 3,
	}})
	m.Update(updateV2{SessionUpdate: "state_update", State: "idle"})
	if !m.Busy || m.Status != "starting" || m.deliver() != nil || len(m.outbox) != 1 {
		t.Fatalf("waiting to start: busy=%t, status=%q, queued=%d", m.Busy, m.Status, len(m.outbox))
	}
	m.Update(updateV2{SessionUpdate: "state_update", State: "running"})
	if !m.Busy || m.Status != "working" || len(m.outbox) != 1 {
		t.Fatalf("running: busy=%t, status=%q, queued=%d", m.Busy, m.Status, len(m.outbox))
	}
	m.Update(updateV2{SessionUpdate: "state_update", State: "idle"})
	if len(m.outbox) != 0 || m.Status != "starting" {
		t.Fatalf("follow-up did not start after idle: status=%q, queued=%d", m.Status, len(m.outbox))
	}
}

func TestDaemonSetupPollDoesNotOccupyLiveReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	snapshot := livesession.Snapshot{ActiveOperation: 2, ActiveMethod: "session/open", Head: 5}
	opened := livesession.Snapshot{Setup: json.RawMessage(`{}`), Head: 6}
	setupPolling := make(chan struct{})
	observerClosed := make(chan struct{})
	controller := connectTestDaemon(t, ctx, func(peer daemonTestPeer) {
		id := peer.read("wait_result")
		select {
		case <-setupPolling:
		case <-ctx.Done():
			return
		}
		peer.reply(id, livesession.Receipt{Done: true})
		peer.reply(peer.read("snapshot"), opened)
		peer.reply(peer.read("next"), livesession.Event{Sequence: 7})
	})
	h := &harness{daemon: &daemonSession{
		client: controller,
		handle: "test-session",
		dial: func(ctx context.Context) (*livesession.Client, error) {
			return connectTestDaemon(t, ctx, func(peer daemonTestPeer) {
				peer.reply(peer.read("attach"), snapshot)
				peer.read("next")
				close(setupPolling)
				// Never answer this poll. Setup must close its observer.
				if err := peer.in.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
					t.Errorf("setup observer: want EOF, got %v", err)
				}
				close(observerClosed)
			}), nil
		},
	}}
	if err := h.setupDaemon(ctx, routing.Config{}, snapshot); err != nil {
		t.Fatal(err)
	}
	if event, err := controller.Next(ctx, opened.Head); err != nil || event.Sequence != 7 {
		t.Fatalf("first live event = %+v, %v", event, err)
	}
	select {
	case <-observerClosed:
	case <-ctx.Done():
		t.Fatal("setup observer was not closed")
	}
}

type daemonTestPeer struct {
	t   *testing.T
	in  *json.Decoder
	out *json.Encoder
}

func (p daemonTestPeer) read(method string) uint64 {
	var request struct {
		ID     uint64
		Method string
	}
	if err := p.in.Decode(&request); err != nil || request.Method != method {
		p.t.Errorf("expected %s, read %+v, %v", method, request, err)
	}
	return request.ID
}

func (p daemonTestPeer) reply(id uint64, result any) {
	if err := p.out.Encode(map[string]any{"id": id, "result": result}); err != nil {
		p.t.Error(err)
	}
}

func connectTestDaemon(t *testing.T, ctx context.Context, serve func(daemonTestPeer)) *livesession.Client {
	t.Helper()
	front, back := net.Pipe()
	done := make(chan struct{})
	t.Cleanup(func() {
		front.Close()
		back.Close()
		<-done
	})
	go func() {
		defer close(done)
		peer := daemonTestPeer{t, json.NewDecoder(back), json.NewEncoder(back)}
		defer back.Close()
		peer.reply(peer.read("hello"), map[string]any{"version": livesession.ProtocolVersion, "instance": "test-daemon"})
		serve(peer)
	}()
	client, err := livesession.Connect(ctx, front)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestCancelAcknowledgedPromptBeforeRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client := connectTestDaemon(t, ctx, func(p daemonTestPeer) {
		var req struct {
			ID     uint64
			Method string
			Params uint64
		}
		if err := p.in.Decode(&req); err != nil || req.Method != "cancel" || req.Params != 7 {
			t.Errorf("cancel request = %+v, %v", req, err)
			return
		}
		p.reply(req.ID, true)
	})
	h := &harness{daemon: &daemonSession{client: client}}
	h.v2.Store(true)
	m := &terminal{root: bloom.New(), h: h, ctx: ctx, screen: screen{connection: connected}}
	c := m.conversationV2()
	c.foreground.beginPrompt()
	c.promptWrite = &promptWriteV2{operation: 7, ready: make(chan struct{})}
	close(c.promptWrite.ready)
	m.Update(promptV2Result{})
	result := m.cancelForegroundV2()().(cancelV2Result)
	if result.err != nil {
		t.Fatal(result.err)
	}
}
