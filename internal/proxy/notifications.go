package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	acp "github.com/coder/acp-go-sdk"
)

// MethodHandler omits both the incoming request ID and the request/notification
// distinction. Carry that information through SDK dispatch, then strip the tags
// before writing to a peer. The SDK still owns cancellation, ordering and replies.
const notificationPrefix = "\x00petal/notification/"
const requestPrefix = "\x00petal/request/"
const v2RequestPrefix = "\x00petal/v2-request/"

type editorRequestID struct{}

func tagged(method string, id json.RawMessage) string {
	return requestPrefix + base64.RawURLEncoding.EncodeToString(id) + "/" + method
}
func untag(method string) (string, json.RawMessage) {
	s, ok := strings.CutPrefix(method, requestPrefix)
	if !ok {
		return method, nil
	}
	token, name, ok := strings.Cut(s, "/")
	if !ok {
		return method, nil
	}
	id, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return method, nil
	}
	return name, id
}
func forward[T any](conn *acp.Connection, ctx context.Context, method string, params any) (T, error) {
	if id, ok := ctx.Value(editorRequestID{}).(json.RawMessage); ok {
		method = tagged(method, id)
	}
	return acp.SendRequest[T](conn, ctx, method, params)
}

// Keep awaiting a v2 response after cancellation. In particular, an already
// inserted prompt still returns its message ID and a cancelled permission
// request still returns its outcome. The SDK's normal cancellation discards
// those responses, so send the notification separately using its wire ID.
func forwardV2(conn *acp.Connection, wire *adapterWire, ctx context.Context, source context.Context, method string, params any, id json.RawMessage) (json.RawMessage, error) {
	wait, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(source, cancel)
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token := strconv.FormatUint(wire.nextRequest.Add(1), 10)
	request := &v2Request{ctx: ctx, sourceID: id}
	wire.requests.Store(token, request)
	defer wire.requests.Delete(token)
	defer func() {
		if request.stopCancel != nil {
			request.stopCancel()
			wire.ids.Delete(request.wireID)
		}
	}()
	return acp.SendRequest[json.RawMessage](conn, wait, v2RequestPrefix+token+"/"+method, params)
}

type v2Request struct {
	ctx        context.Context
	sourceID   json.RawMessage
	wireID     string
	stopCancel func() bool
}

type adapterWire struct {
	outputMu    sync.Mutex
	selectedV2  atomic.Bool
	updates     chan struct{}
	lifetime    context.Context
	nativeV2    bool
	conn        *acp.Connection
	nextRequest atomic.Uint64
	requests    sync.Map
	io.Writer
	ids sync.Map // adapter request ID -> originating editor request ID
}

func (w *adapterWire) Write(line []byte) (int, error) {
	w.outputMu.Lock()
	defer w.outputMu.Unlock()
	if !bytes.Contains(line, []byte(`\u0000petal/request/`)) && !bytes.Contains(line, []byte(`\u0000petal/v2-request/`)) {
		return w.Writer.Write(line)
	}
	var frame Object
	if err := json.Unmarshal(line, &frame); err != nil {
		return 0, err
	}
	var method string
	_ = json.Unmarshal(frame["method"], &method)
	name, editorID := untag(method)
	var request *v2Request
	if tag, ok := strings.CutPrefix(method, v2RequestPrefix); ok {
		token, native, _ := strings.Cut(tag, "/")
		if value, ok := w.requests.Load(token); ok {
			request = value.(*v2Request)
			name, editorID = native, request.sourceID
		}
	}
	if editorID == nil && request == nil {
		return w.Writer.Write(line)
	}
	id := frame["id"]
	frame["method"] = encoded(name)
	w.ids.Store(string(id), editorID)
	data, err := json.Marshal(frame)
	if err != nil {
		return 0, err
	}
	_, err = w.Writer.Write(append(data, '\n'))
	if err != nil {
		w.ids.Delete(string(id))
		return 0, err
	}
	if request != nil {
		request.wireID = string(id)
		request.stopCancel = context.AfterFunc(request.ctx, func() {
			if _, pending := w.ids.Load(string(id)); pending {
				_ = w.conn.SendNotification(w.lifetime, "$/cancel_request", Object{"requestId": id})
			}
		})
	}
	return len(line), nil
}

type wireReader struct {
	editor  *adapterWire
	scan    *bufio.Scanner
	pending []byte
	adapter *adapterWire
}

func incoming(r io.Reader, adapter *adapterWire) *wireReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 10<<20)
	return &wireReader{scan: s, adapter: adapter}
}
func (r *wireReader) Read(dst []byte) (int, error) {
	for len(r.pending) == 0 {
		if !r.scan.Scan() {
			if err := r.scan.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := r.scan.Bytes()
		if r.editor != nil && r.editor.selectedV2.Load() && bytes.HasPrefix(bytes.TrimSpace(line), []byte("[")) && json.Valid(line) {
			// A v2 receiver rejects a batch as one invalid request.
			if _, err := r.editor.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"ACP does not support batches"}}` + "\n")); err != nil {
				return 0, err
			}
			continue
		}
		var head struct {
			ID     json.RawMessage
			Method string
		}
		if json.Unmarshal(line, &head) == nil {
			method := head.Method
			if method == "" {
				if r.adapter != nil {
					r.adapter.ids.Delete(string(head.ID))
				}
				if r.editor != nil {
					r.editor.ids.Delete(string(head.ID))
				}
			}
			if method != "" && method != "$/cancel_request" {
				if strings.HasPrefix(method, "\x00") {
					return 0, fmt.Errorf("invalid ACP method name")
				}
				if head.ID == nil {
					// Bound incoming replay updates before the SDK's notification
					// queue, which disconnects on overflow. The proxy forwards
					// these notifications without awaiting a backend response.
					if method == "session/update" && r.adapter != nil && r.adapter.updates != nil {
						select {
						case r.adapter.updates <- struct{}{}:
						case <-r.adapter.lifetime.Done():
							return 0, r.adapter.lifetime.Err()
						}
					}
					method = notificationPrefix + method
				}
				if head.ID != nil && (r.adapter == nil || r.adapter.nativeV2) {
					method = tagged(method, head.ID)
				}
				elicit := r.adapter != nil && (head.Method == "elicitation/create" || head.Method == "elicitation/complete")
				if method != head.Method || elicit {
					var msg Object
					_ = json.Unmarshal(line, &msg)
					msg["method"] = encoded(method)
					if elicit {
						var params Object
						_ = json.Unmarshal(msg["params"], &params)
						if id := params["requestId"]; id != nil {
							if editorID, ok := r.adapter.ids.Load(string(id)); ok {
								params["requestId"] = editorID.(json.RawMessage)
								msg["params"], _ = json.Marshal(params)
							}
						}
					}
					line, _ = json.Marshal(msg)
				}
			}
		}
		r.pending = append(line, '\n')
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
