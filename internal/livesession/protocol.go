//go:build linux || darwin

// Package livesession owns the local daemon's live ACP sessions and the
// frontend protocol. Its protocol version is independent of the ACP version.
package livesession

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

const (
	ProtocolVersion = 1
	maxFrame        = 12 << 20
	maxPromptBody   = 10 << 20
	maxBody         = 4 << 20
	attachTimeout   = 5 * time.Second
)

// Error codes describe daemon errors. Adapter errors are preserved separately
// in Outcome.ACPError with their original ACP code and data.
type Code string

const (
	Invalid  Code = "invalid"
	Version  Code = "version"
	NotFound Code = "not_found"
	Conflict Code = "conflict"
	ReadOnly Code = "read_only"
	Busy     Code = "busy"
	Closed   Code = "closed"
	Expired  Code = "expired"
	Gap      Code = "gap"
	Limit    Code = "limit"
	Internal Code = "internal"
)

type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Floor   uint64 `json:"floor,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func failure(code Code, message string) *Error { return &Error{Code: code, Message: message} }

// OpenOptions configures a single workspace and one live adapter session.
// ResumeID is Petal's durable routed ACP ID. Handle is an idempotency key
// generated with NewHandle and can also identify the session before ACP setup.
type OpenOptions struct {
	Handle   string `json:"handle"`
	Cwd      string `json:"cwd"`
	Model    string `json:"model,omitempty"`
	Account  string `json:"account,omitempty"`
	ResumeID string `json:"resumeID,omitempty"`
}

// NewHandle creates an open retry key. A previously unseen key must be
// presented within ten minutes. Live handles remain valid until close.
func NewHandle() string {
	var nonce [16]byte
	rand.Read(nonce[:])
	return strconv.FormatInt(time.Now().Unix(), 10) + "-" + hex.EncodeToString(nonce[:])
}

const handleLifetime = 10 * time.Minute

func handleExpiry(handle string) (time.Time, bool) {
	stamp, nonce, ok := strings.Cut(handle, "-")
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if !ok || err != nil || len(nonce) != 32 {
		return time.Time{}, false
	}
	var bytes [16]byte
	if _, err := hex.Decode(bytes[:], []byte(nonce)); err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).Add(handleLifetime), true
}

// Info identifies a live daemon session. SessionID is set after setup succeeds.
type Info struct {
	Handle        string    `json:"handle"`
	SessionID     string    `json:"sessionID,omitempty"`
	ResumeID      string    `json:"resumeID,omitempty"`
	Cwd           string    `json:"cwd"`
	Model         string    `json:"model,omitempty"`
	PinnedAccount string    `json:"pinnedAccount,omitempty"`
	Account       string    `json:"account,omitempty"`
	ACPVersion    uint16    `json:"acpVersion,omitempty"`
	LastAttached  time.Time `json:"lastAttached"`
	AdapterExited bool      `json:"adapterExited,omitempty"`
}

// AttachOptions selects a live session by handle or durable ID. After is the
// last event the caller has applied. A nil After starts at the oldest retained
// event. Control requests an exclusive lease for this connection.
type AttachOptions struct {
	Handle    string  `json:"handle"`
	SessionID string  `json:"sessionID,omitempty"`
	Cwd       string  `json:"cwd,omitempty"`
	After     *uint64 `json:"after"`
	Control   bool    `json:"control"`
}

// Snapshot is captured with Head under the same lock used to publish events.
// Replay (After, Head] before applying Pending and other state at Head. Use
// events after Head to incrementally update that state. Floor is the cursor
// immediately before the oldest available event.
type Snapshot struct {
	Info
	After             uint64          `json:"after"`
	Floor             uint64          `json:"floor"`
	Head              uint64          `json:"head"`
	Control           bool            `json:"control"`
	NextOperation     uint64          `json:"nextOperation"`
	ActiveMethod      string          `json:"activeMethod,omitempty"`
	ActiveOperation   uint64          `json:"activeOperation,omitempty"`
	SteeringOperation uint64          `json:"steeringOperation,omitempty"`
	TurnStarted       bool            `json:"turnStarted,omitempty"`
	Turn              uint64          `json:"turn,omitempty"`
	Foreground        string          `json:"foreground,omitempty"`
	Running           bool            `json:"running,omitempty"`
	Canceling         bool            `json:"canceling,omitempty"`
	Closed            bool            `json:"closed,omitempty"`
	Hello             json.RawMessage `json:"hello,omitempty"`
	Setup             json.RawMessage `json:"setup,omitempty"`
	ArchiveHead       int64           `json:"archiveHead,omitempty"`
	SetupMethod       string          `json:"setupMethod,omitempty"`
	Pending           []Question      `json:"pending,omitempty"`

	// AwaitingStart marks an ACP v2 prompt that has yet to enter active foreground.
	AwaitingStart bool `json:"awaitingStart,omitempty"`
}

type EventKind string

const (
	Update         EventKind = "update"
	QuestionOpened EventKind = "question"
	QuestionClosed EventKind = "question_closed"
	CallStarted    EventKind = "call_started"
	CallFinished   EventKind = "call_finished"
	CancelFinished EventKind = "cancel_finished"
	AdapterExited  EventKind = "adapter_exited"
	SessionClosed  EventKind = "closed"
)

type Event struct {
	Sequence   uint64          `json:"sequence"`
	Kind       EventKind       `json:"kind"`
	Method     string          `json:"method"`
	Params     json.RawMessage `json:"params,omitempty"`
	Question   *Question       `json:"question,omitempty"`
	QuestionID uint64          `json:"questionID"`
	Operation  uint64          `json:"operation"`
	Outcome    *Outcome        `json:"outcome,omitempty"`
}

// Question carries an ACP permission or elicitation callback, or a daemon-host
// workspace approval. For workspace approvals Method is "host/approve".
type Question struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Answer uses OptionID for ACP permission and host/approve (allow or deny).
// For elicitation/create use Result with the ACP elicitation response.
// An empty answer cancels the question.
type Answer struct {
	QuestionID uint64          `json:"questionID"`
	OptionID   string          `json:"optionID,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

// Call submits one daemon-owned operation. Operation is the monotonic number
// returned by Snapshot.NextOperation or by a successful prior submission + 1.
// Retry with the same number and payload after an ambiguous transport error.
type Call struct {
	Operation uint64          `json:"operation"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// Outcome is the retained receipt for a completed call. CallFinished carries
// the operation number for lookup through Client.Result.
type Outcome struct {
	Result   json.RawMessage   `json:"result,omitempty"`
	ACPError *acp.RequestError `json:"acpError,omitempty"`
	Error    *Error            `json:"error,omitempty"`
}

// Err returns either the daemon error or the adapter's original ACP error.
func (o Outcome) Err() error {
	if o.Error != nil {
		return o.Error
	}
	if o.ACPError != nil {
		return o.ACPError
	}
	return nil
}

type Receipt struct {
	Done bool `json:"done"`
	Outcome
}

// TerminalRef identifies an ACP terminal within its owning routed session.
type TerminalRef struct {
	SessionID  string `json:"sessionID,omitempty"`
	TerminalID string `json:"terminalID,omitempty"`
}

type handshake struct {
	Version  int    `json:"version"`
	Instance string `json:"instance"`
}

type request struct {
	ID      uint64
	Method  string
	Version int             `json:",omitempty"`
	Params  json.RawMessage `json:",omitempty"`
}

type response struct {
	ID     uint64
	Result json.RawMessage `json:",omitempty"`
	Error  *Error          `json:",omitempty"`
}

type frameReader struct{ *bufio.Scanner }

func readFrames(r io.Reader) frameReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxFrame)
	return frameReader{scanner}
}

func (r frameReader) read(value any) error {
	if !r.Scan() {
		if err := r.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	return json.Unmarshal(r.Bytes(), value)
}

func writeFrame(conn net.Conn, value any, deadline time.Time) error {
	line, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(line)+1 >= maxFrame {
		return fmt.Errorf("daemon protocol frame exceeds %d bytes", maxFrame)
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err = conn.Write(append(line, '\n'))
	return err
}
