//go:build linux || darwin

package livesession

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"petal/internal/localdaemon"
	"petal/internal/proxy"
	"petal/internal/routing"
	"petal/internal/sessions"
)

const (
	maxSessions    = 16
	maxConnections = 64
	maxClosedKeys  = 1024
)

// Resolver loads daemon-side routing for the requested model and account.
// It can be called concurrently. The daemon's configuration source is
// chosen by its entry point, never by a wire request.
type Resolver func(OpenOptions) (routing.Config, error)

type Server struct {
	instance         string
	ctx              context.Context
	stop             context.CancelFunc
	workers          sync.WaitGroup
	resolve          Resolver
	clients          chan struct{}
	mu               sync.Mutex
	live             map[string]*session
	byID             map[workspaceRoute]*session
	closed           map[string]time.Time
	reservationGrace time.Duration
}

func NewServer(ctx context.Context, resolve Resolver) *Server {
	ctx, stop := context.WithCancel(ctx)
	return &Server{
		instance: rand.Text(), ctx: ctx, stop: stop, resolve: resolve, clients: make(chan struct{}, maxConnections),
		live: make(map[string]*session), byID: make(map[workspaceRoute]*session),
		closed: make(map[string]time.Time), reservationGrace: OpenReservationGrace,
	}
}

// Close stops all live sessions and waits for their adapters and archives.
// The endpoint Serve context still controls the listener.
func (s *Server) Close() {
	s.mu.Lock()
	s.stop()
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *Server) Handler() localdaemon.Handler { return s.serve }

type workspaceRoute struct{ cwd, route string }

func routeKey(id string) string {
	parts := proxy.DecodeSessionRoute(id)
	if parts == nil {
		return id
	}
	raw, _ := json.Marshal(parts)
	return string(raw)
}

func (s *Server) open(opts OpenOptions) (Info, *Error) {
	if !filepath.IsAbs(opts.Cwd) || len(opts.Cwd) > 4096 || len(opts.Model) > 256 ||
		len(opts.Account) > 256 || len(opts.ResumeID) > 4096 {
		return Info{}, failure(Invalid, "expected an absolute workspace directory and valid routing options")
	}
	opts.Cwd = filepath.Clean(opts.Cwd)
	expiry, valid := handleExpiry(opts.Handle)
	if !valid {
		return Info{}, failure(Invalid, "invalid open key")
	}

	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return Info{}, failure(Closed, "daemon is shutting down")
	}
	if live := s.live[opts.Handle]; live != nil {
		s.mu.Unlock()
		live.mu.Lock()
		defer live.mu.Unlock()
		if live.options != opts {
			return Info{}, failure(Conflict, "open key is already bound to different options")
		}
		if live.closed {
			return Info{}, failure(Closed, "session is closed")
		}
		return live.info, nil
	}
	if _, closed := s.closed[opts.Handle]; closed {
		s.mu.Unlock()
		return Info{}, failure(Closed, "session is closed")
	}
	now := time.Now()
	if !expiry.After(now) || expiry.After(now.Add(handleLifetime+time.Minute)) {
		s.mu.Unlock()
		return Info{}, failure(Expired, "open key is outside its retry window")
	}
	for key, expires := range s.closed {
		if !expires.After(now) {
			delete(s.closed, key)
		}
	}
	if len(s.live) >= maxSessions || len(s.live)+len(s.closed) >= maxClosedKeys {
		s.mu.Unlock()
		return Info{}, failure(Limit, "daemon live-session or open-key limit reached")
	}
	// Reserve both identities before publishing a resumable handle.
	live := newSession(s, opts)
	if opts.ResumeID != "" {
		if err := live.leaseRoute(opts.ResumeID); err != nil {
			live.stop()
			s.mu.Unlock()
			return Info{}, err
		}
		s.byID[workspaceRoute{opts.Cwd, routeKey(opts.ResumeID)}] = live
	}
	s.live[opts.Handle] = live
	s.workers.Go(live.retire)
	s.mu.Unlock()
	return live.initialInfo(), nil
}

// leaseRoute is called during reservation or by the single setup/fork
// operation. Retirement waits for that operation before releasing leases.
func (live *session) leaseRoute(id string) *Error {
	parts := proxy.DecodeSessionRoute(id)
	if parts == nil || parts[2] == "" || (live.options.Model != "" && live.options.Model != parts[1]) {
		return failure(Invalid, "durable session ID does not belong to the selected model")
	}
	route := routeKey(id)
	if live.leases[route] != nil {
		return nil
	}
	// A fork can also install a recorder, so bound the number of routes
	// retained by one live session until its backend flushes.
	if len(live.leases) >= 64 {
		return failure(Limit, "live session durable route limit reached")
	}
	lease, err := sessions.LeaseRoute(live.options.Cwd, route)
	if errors.Is(err, sessions.ErrRouteLeased) {
		return failure(Conflict, err.Error())
	}
	if err != nil {
		return failure(Internal, fmt.Sprintf("reserve durable session: %v", err))
	}
	if live.leases == nil {
		live.leases = make(map[string]*os.File)
	}
	live.leases[route] = lease
	return nil
}

func (s *Server) register(live *session, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[workspaceRoute{live.options.Cwd, routeKey(id)}] = live
}

func (s *Server) remove(live *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lease := range live.leases {
		_ = lease.Close()
	}
	delete(s.live, live.options.Handle)
	for key, owner := range s.byID {
		if owner == live {
			delete(s.byID, key)
		}
	}
	if expires, _ := handleExpiry(live.options.Handle); expires.After(time.Now()) {
		s.closed[live.options.Handle] = expires
	}
}

func (s *Server) lookup(opts AttachOptions) (*session, *Error) {
	if (opts.Handle == "") == (opts.SessionID == "") {
		return nil, failure(Invalid, "specify exactly one handle or durable session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var live *session
	if opts.Handle != "" {
		live = s.live[opts.Handle]
		if _, ok := s.closed[opts.Handle]; ok {
			return nil, failure(Closed, "session is closed")
		}
	} else {
		route := routeKey(opts.SessionID)
		if opts.Cwd != "" {
			live = s.byID[workspaceRoute{filepath.Clean(opts.Cwd), route}]
		} else {
			for key, candidate := range s.byID {
				if key.route != route {
					continue
				}
				if live != nil && live != candidate {
					return nil, failure(Conflict, "session ID exists in multiple workspaces")
				}
				live = candidate
			}
		}
	}
	if live == nil {
		return nil, failure(NotFound, "session is not live")
	}
	return live, nil
}

func (s *Server) list() []Info {
	s.mu.Lock()
	live := make([]*session, 0, len(s.live))
	for _, session := range s.live {
		live = append(live, session)
	}
	s.mu.Unlock()

	result := make([]Info, 0, len(live))
	for _, session := range live {
		session.mu.Lock()
		if !session.closed {
			result = append(result, session.info)
		}
		session.mu.Unlock()
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Handle < result[j].Handle })
	return result
}

type peer struct {
	ctx       context.Context
	conn      net.Conn
	writeMu   sync.Mutex
	session   *session
	attachKey [32]byte
	attached  Snapshot
}

func (p *peer) reply(id uint64, value any, protocolErr *Error) {
	res := response{ID: id, Error: protocolErr}
	if protocolErr == nil {
		var err error
		res.Result, err = json.Marshal(value)
		if err != nil || len(res.Result) >= maxFrame-256 {
			res.Result = nil
			res.Error = failure(Limit, "daemon response exceeds frame limit")
		}
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := writeFrame(p.conn, res, time.Now().Add(attachTimeout)); err != nil {
		p.conn.Close()
	}
}

func (s *Server) serve(ctx context.Context, conn net.Conn) {
	select {
	case s.clients <- struct{}{}:
		defer func() { <-s.clients }()
	default:
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	p := &peer{ctx: ctx, conn: conn}
	var waits sync.WaitGroup
	defer func() {
		cancel()
		conn.Close()
		if p.session != nil {
			p.session.detach(p)
		}
		waits.Wait()
	}()

	reader := readFrames(conn)
	_ = conn.SetReadDeadline(time.Now().Add(attachTimeout))
	var hello request
	if err := reader.read(&hello); err != nil || hello.ID == 0 || hello.Method != "hello" {
		return
	}
	if hello.Version != ProtocolVersion {
		p.reply(hello.ID, nil, failure(Version, fmt.Sprintf("daemon protocol version is %d", ProtocolVersion)))
		return
	}
	p.reply(hello.ID, handshake{Version: ProtocolVersion, Instance: s.instance}, nil)
	_ = conn.SetReadDeadline(time.Time{})

	nextSlots := make(chan struct{}, 1)
	resultSlots := make(chan struct{}, 4)
	previous := hello.ID
	for {
		var req request
		if err := reader.read(&req); err != nil || req.ID <= previous {
			return
		}
		previous = req.ID
		if live := p.session; live != nil && (req.Method == "next" || req.Method == "wait_result") {
			var value uint64
			if err := json.Unmarshal(req.Params, &value); err != nil {
				p.reply(req.ID, nil, failure(Invalid, "expected an event cursor or operation number"))
				continue
			}
			slots := nextSlots
			if req.Method == "wait_result" {
				slots = resultSlots
			}
			select {
			case slots <- struct{}{}:
				waits.Go(func() {
					var result any
					var err *Error
					if req.Method == "next" {
						result, err = live.next(ctx, value)
					} else {
						result, err = live.waitResult(ctx, value)
					}
					// Release before replying. The client can start another
					// poll as soon as the reply reaches it.
					<-slots
					if ctx.Err() == nil {
						p.reply(req.ID, result, err)
					}
				})
			default:
				p.reply(req.ID, nil, failure(Busy, "too many pending polls"))
			}
			continue
		}
		result, err := s.dispatch(p, req)
		p.reply(req.ID, result, err)
	}
}

func (s *Server) dispatch(p *peer, req request) (any, *Error) {
	switch req.Method {
	case "list":
		return s.list(), nil
	case "open":
		return decodeRequest(req.Params, s.open)
	case "attach":
		return decodeRequest(req.Params, func(opts AttachOptions) (Snapshot, *Error) {
			key := fingerprint(opts)
			if p.session != nil {
				if p.attachKey == key {
					return p.attached, nil
				}
				return Snapshot{}, failure(Conflict, "connection is already attached")
			}
			live, err := s.lookup(opts)
			if err != nil {
				return Snapshot{}, err
			}
			snapshot, err := live.attach(p, opts)
			if err == nil {
				p.session, p.attachKey, p.attached = live, key, snapshot
			}
			return snapshot, err
		})
	}
	live := p.session
	if live == nil {
		return nil, failure(Invalid, "attach to a live session first")
	}
	switch req.Method {
	case "snapshot":
		return live.snapshot(p)
	case "claim":
		return live.claim(p)
	case "call":
		return decodeRequest(req.Params, func(call Call) (uint64, *Error) { return live.submit(p, call) })
	case "result":
		return decodeRequest(req.Params, live.result)
	case "terminal/output":
		return decodeRequest(req.Params, live.terminalOutput)
	case "answer":
		return decodeRequest(req.Params, func(answer Answer) (bool, *Error) { return true, live.answer(p, answer) })
	case "cancel":
		return decodeRequest(req.Params, func(turn uint64) (bool, *Error) { return true, live.cancelTurn(p, turn) })
	case "close":
		return nil, live.closeBy(p)
	default:
		return nil, failure(Invalid, "unknown daemon method")
	}
}

func decodeRequest[P, R any](raw json.RawMessage, apply func(P) (R, *Error)) (R, *Error) {
	var params P
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &params) != nil {
		var zero R
		return zero, failure(Invalid, "invalid daemon request parameters")
	}
	return apply(params)
}
