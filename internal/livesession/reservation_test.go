//go:build linux || darwin

package livesession

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"petal/internal/routing"
)

func TestAbandonedOpenReservationsReleaseAllSlots(t *testing.T) {
	server := NewServer(t.Context(), nil)
	server.reservationGrace = time.Second
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	clients, options := fillReservations(t, ctx, server, cwd)
	client := clients[0]
	if retry, err := client.Open(ctx, options[0]); err != nil || retry.Handle != options[0].Handle {
		t.Fatalf("open retry = %+v, %v", retry, err)
	}
	changed := options[0]
	changed.Model = "different"
	if _, err := client.Open(ctx, changed); !hasCode(err, Conflict) {
		t.Fatalf("open retry with changed options = %v", err)
	}

	awaitReservationCount(t, ctx, client, 0)
	for _, opts := range options {
		if _, err := client.Open(ctx, opts); !hasCode(err, Closed) {
			t.Fatalf("expired reservation retry = %v", err)
		}
	}
	fillReservations(t, ctx, server, cwd)
}

func TestFailedStartupReleasesAllSlots(t *testing.T) {
	server := NewServer(t.Context(), func(OpenOptions) (routing.Config, error) {
		return routing.Config{}, errors.New("fixture failed to load settings")
	})
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	clients, options := fillReservations(t, ctx, server, cwd)
	var starts sync.WaitGroup
	for i, client := range clients {
		starts.Go(func() {
			if _, err := client.Attach(ctx, AttachOptions{Handle: options[i].Handle, Control: true}); err != nil {
				t.Error(err)
				return
			}
			if _, err := client.Submit(ctx, Call{Operation: 1, Method: "initialize"}); err != nil {
				t.Error(err)
				return
			}
			receipt, err := client.WaitResult(ctx, 1)
			if err != nil || receipt.Err() == nil || receipt.Err().Error() != "fixture failed to load settings" {
				t.Errorf("initialize failure receipt = %+v, %v", receipt, err)
			}
		})
	}
	starts.Wait()
	awaitReservationCount(t, ctx, clients[0], 0)
	// Keep the original controllers connected. The failed reservations must
	// let a second full batch open even while their receipts remain readable.
	fillReservations(t, ctx, server, cwd)
}

func TestReservationExpiryPreservesDetachedWorkingSession(t *testing.T) {
	ctx, dial := startSocketWithGrace(t, 1, false, 250*time.Millisecond)
	cwd := t.TempDir()
	controller := dial()
	opts := OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture"}
	working := openSession(t, ctx, controller, opts, 1)
	controller.Close()

	observer := dial()
	abandoned := OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture"}
	if _, err := observer.Open(ctx, abandoned); err != nil {
		t.Fatal(err)
	}
	awaitReservationCount(t, ctx, observer, 1)
	reconnected := takeControl(t, ctx, observer, AttachOptions{Cwd: cwd, SessionID: working.SessionID, Control: true})
	if reconnected.Handle != working.Handle || len(reconnected.Setup) == 0 {
		t.Fatalf("reattached working session = %+v", reconnected)
	}
}

func fillReservations(t *testing.T, ctx context.Context, server *Server, cwd string) ([]*Client, []OpenOptions) {
	t.Helper()
	clients := make([]*Client, maxSessions)
	options := make([]OpenOptions, maxSessions)
	for i := range clients {
		local, remote := net.Pipe()
		go server.Handler()(ctx, remote)
		client, err := Connect(ctx, local)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		clients[i] = client
		options[i] = OpenOptions{Cwd: cwd, Handle: NewHandle()}
	}
	var opens sync.WaitGroup
	for i, client := range clients {
		opens.Go(func() {
			for {
				_, err := client.Open(ctx, options[i])
				if err == nil {
					return
				}
				if !hasCode(err, Limit) || ctx.Err() != nil {
					t.Error(err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
	opens.Wait()
	if _, err := clients[0].Open(ctx, OpenOptions{Cwd: cwd, Handle: NewHandle()}); !hasCode(err, Limit) {
		t.Fatalf("seventeenth reservation = %v", err)
	}
	return clients, options
}

func awaitReservationCount(t *testing.T, ctx context.Context, client *Client, count int) {
	t.Helper()
	for {
		current, err := client.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(current) == count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wanted %d live sessions, got %v", count, current)
		case <-time.After(time.Millisecond):
		}
	}
}
