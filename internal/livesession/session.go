//go:build linux || darwin

package livesession

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"petal/internal/acphost"
	"petal/internal/proxy"
	"petal/internal/routing"
)

const (
	receiptCount = 32
	receiptBytes = 4 << 20
	maxMetadata  = 512 << 10
	maxQuestions = 16
	maxQuestion  = 64 << 10
	// Reserve IDs for turns with no frontend prompt, within JSON safe integers.
	independentTurnBase = 1 << 52
)

type operation struct {
	number      uint64
	method      string
	fingerprint [32]byte
	dispatched  chan struct{}
	done        chan struct{}
	receipt     Receipt
	size        int
	replay      *setupReplay
}

type setupReplay struct {
	journal
	archive     setupArchiveV2
	foreground  string
	stateCursor uint64
}

type session struct {
	server    *Server
	options   OpenOptions
	ctx       context.Context
	stop      context.CancelFunc
	ready     chan struct{}
	callbacks chan struct{}

	// Preparation writes these fields before closing ready. The backend and
	// host have the same lifetime as ctx, which belongs to the daemon.
	host    *acphost.Host
	backend *proxy.Local
	config  routing.Config
	initErr error
	leases  map[string]*os.File

	// Guarded by mu.
	preparing            bool
	reservationIdleSince time.Time

	mu                  sync.Mutex
	info                Info
	controller          *peer
	changed             chan struct{}
	events              journal
	closed              bool
	closingDone         chan struct{}
	hello               json.RawMessage
	resumeMethod        string
	archiveHead         int64
	archiveCursor       uint64
	archiveV2           archiveV2
	settingsConfirmed   bool
	settingsRevision    uint64
	setup               json.RawMessage
	nextOp              uint64
	active              *operation
	steering            *operation
	turn                *operation
	pendingPrompt       *operation
	turnV2              uint64
	nextIndependentTurn uint64
	running             bool
	foreground          string
	canceling           bool
	cancelled           uint64
	receipts            []*operation
	receiptSize         int
	questionID          uint64
	pending             map[uint64]*pendingQuestion
	answered            map[uint64][32]byte
	answerOrder         []uint64
	listing             *operation
}

func newSession(server *Server, options OpenOptions) *session {
	ctx, stop := context.WithCancel(server.ctx)
	return &session{
		server: server, options: options, ctx: ctx, stop: stop, ready: make(chan struct{}), callbacks: make(chan struct{}, 32),
		info:                 Info{Handle: options.Handle, Cwd: options.Cwd, Model: options.Model, Account: options.Account, PinnedAccount: options.Account, ResumeID: options.ResumeID},
		reservationIdleSince: time.Now(), changed: make(chan struct{}), nextOp: 1, pending: make(map[uint64]*pendingQuestion), answered: make(map[uint64][32]byte),
	}
}

func (s *session) initialInfo() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

func (s *session) prepare() {
	defer close(s.ready)
	if s.server.resolve == nil {
		s.initErr = errors.New("daemon routing resolver is missing")
		return
	}
	config, err := s.server.resolve(s.options)
	if err != nil {
		s.initErr = err
		return
	}
	if err := s.ctx.Err(); err != nil {
		s.initErr = err
		return
	}
	if config.Model == "" {
		config.Model = config.DefaultModel()
	}
	if route := proxy.DecodeSessionRoute(s.options.ResumeID); s.options.ResumeID != "" &&
		(route == nil || route[1] != config.Model || route[2] == "") {
		s.initErr = errors.New("resume ID does not belong to the selected model")
		return
	}
	s.config = config
	host, err := acphost.Open(s.ctx, s.options.Cwd, s.approveHost)
	if err != nil {
		s.initErr = err
		return
	}
	s.host = host
	s.backend = proxy.NewLocal(s.ctx, config, s.options.Cwd, s.callback, s.update, func(id string) error {
		if err := s.leaseRoute(id); err != nil {
			return err
		}
		return nil
	})
	s.mu.Lock()
	s.info.Model = config.Model
	s.info.Account = config.PinnedAccount
	s.mu.Unlock()
}

func (s *session) publish(event Event) {
	s.events.append(event)
	s.notify()
}

func (s *session) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *session) attach(p *peer, opts AttachOptions) (Snapshot, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Snapshot{}, failure(Closed, "session is closed")
	}
	after := s.events.floor()
	if opts.After != nil {
		after = *opts.After
		if _, err := s.events.after(after); err != nil {
			return Snapshot{}, err
		}
	}
	if opts.Control {
		if s.controller != nil {
			return Snapshot{}, failure(Conflict, "another connection controls the session")
		}
		s.controller = p
		s.notify()
		s.info.LastAttached = time.Now()
	}
	snapshot := s.state(p)
	snapshot.After = after
	return snapshot, nil
}

func (s *session) state(p *peer) Snapshot {
	snapshot := Snapshot{
		Info: s.info, Floor: s.events.floor(), Head: s.events.head, After: s.events.head,
		Control: s.controller == p, NextOperation: s.nextOp, Foreground: s.foreground, Running: s.running, Canceling: s.canceling,
		SettingsPending: s.settingsPending(), SettingsRevision: s.settingsRevision, ArchiveCursor: s.archiveCursor,
		Closed: s.closed, Hello: s.hello, Setup: s.setup, SetupMethod: s.resumeMethod, ArchiveHead: s.archiveHead,
		AwaitingStart: s.pendingPrompt != nil,
	}
	if s.active != nil {
		snapshot.ActiveOperation = s.active.number
		snapshot.ActiveMethod = s.active.method
	}
	if s.steering != nil {
		snapshot.SteeringOperation = s.steering.number
	}
	switch {
	case s.info.ACPVersion == 2:
		snapshot.Turn = s.currentV2Turn()
		snapshot.TurnStarted = s.running
	case s.turn != nil:
		snapshot.Turn = s.turn.number
	}
	// Question IDs are monotonic, but a long-running question can survive
	// arbitrarily many other questions. Sort only the pending entries.
	for _, question := range s.pending {
		snapshot.Pending = append(snapshot.Pending, question.question)
	}
	sortQuestions(snapshot.Pending)
	return snapshot
}

func (s *session) snapshot(p *peer) (Snapshot, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state(p), nil
}

func (s *session) claim(p *peer) (Snapshot, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Snapshot{}, failure(Closed, "session is closed")
	}
	if s.controller != nil && s.controller != p {
		return Snapshot{}, failure(Conflict, "another connection controls the session")
	}
	s.controller = p
	s.notify()
	s.info.LastAttached = time.Now()
	return s.state(p), nil
}

func (s *session) detach(p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.controller == p {
		s.controller = nil
		s.reservationIdleSince = time.Now()
		s.notify()
	}
}

func (s *session) requireControl(p *peer) *Error {
	if s.closed {
		return failure(Closed, "session is closed")
	}
	if s.controller != p {
		return failure(ReadOnly, "connection does not control the session")
	}
	return nil
}

func (s *session) next(ctx context.Context, after uint64) (Event, *Error) {
	return poll(ctx, s, func() (Event, bool, *Error) {
		event, err := s.events.after(after)
		return event, event.Sequence != 0, err
	})
}

func poll[T any](ctx context.Context, s *session, read func() (T, bool, *Error)) (T, *Error) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		s.mu.Lock()
		value, found, err := read()
		changed, closed := s.changed, s.closed
		s.mu.Unlock()
		if found || err != nil {
			return value, err
		}
		if closed {
			return value, failure(Closed, "session is closed")
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return value, failure(Closed, "connection is closed")
		case <-timer.C:
			return value, nil
		}
	}
}

func (s *session) closeBy(p *peer) *Error {
	s.mu.Lock()
	if s.closed && s.controller == p {
		s.mu.Unlock()
		return nil
	}
	if err := s.requireControl(p); err != nil {
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.publish(Event{Kind: SessionClosed})
	if s.info.SessionID != "" && !s.info.AdapterExited && s.active == nil && s.listing == nil && s.steering == nil && s.pendingPrompt == nil && !s.running && !s.canceling {
		done := make(chan struct{})
		s.closingDone = done
		go s.closeAdapter(s.info.SessionID, done)
	} else {
		s.stop()
	}
	s.mu.Unlock()
	return nil
}

// Await the daemon stop or the end of an abandoned, incomplete reservation.
func (s *session) awaitRetirement() {
	for {
		s.mu.Lock()
		if s.info.SessionID != "" || s.closed {
			s.mu.Unlock()
			<-s.ctx.Done()
			return
		}
		changed := s.changed
		var timer *time.Timer
		var expired <-chan time.Time
		if s.controller == nil && s.active == nil {
			remaining := time.Until(s.reservationIdleSince.Add(s.server.reservationGrace))
			if remaining <= 0 {
				s.closed = true
				s.publish(Event{Kind: SessionClosed})
				s.stop()
				s.mu.Unlock()
				return
			}
			timer = time.NewTimer(remaining)
			expired = timer.C
		}
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
		case <-changed:
		case <-expired:
		}
		if timer != nil {
			timer.Stop()
		}
		if s.ctx.Err() != nil {
			return
		}
	}
}

func (s *session) retire() {
	s.awaitRetirement()
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.publish(Event{Kind: SessionClosed})
	}
	active, steering, listing, closing, preparing := s.active, s.steering, s.listing, s.closingDone, s.preparing
	s.mu.Unlock()
	if preparing {
		<-s.ready
	}
	if closing != nil {
		<-closing
	}
	if s.host != nil {
		s.host.Close()
	}
	// Hold the durable route until in-flight operations have checkpointed and
	// the previous adapter processes have exited.
	if active != nil {
		<-active.done
	}
	if steering != nil {
		<-steering.done
	}
	if listing != nil {
		<-listing.done
	}
	if s.backend != nil {
		s.backend.Wait()
	}
	if s.info.ACPVersion == 2 && len(s.setup) != 0 && !s.archiveV2.deleted {
		s.checkpointV2(s.events.head)
	}
	s.server.remove(s)
}
