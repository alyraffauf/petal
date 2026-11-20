package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// MethodHandler omits both the incoming request ID and the request/notification
// distinction. Carry that information through SDK dispatch, then strip the tags
// before writing to a peer. The SDK still owns cancellation, ordering and replies.
const notificationPrefix = "\x00petal/notification/"
const requestPrefix = "\x00petal/request/"

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

type adapterWire struct {
	io.Writer
	ids sync.Map // adapter request ID -> originating editor request ID
}

func (w *adapterWire) Write(line []byte) (int, error) {
	if !bytes.Contains(line, []byte(`\u0000petal/request/`)) {
		return w.Writer.Write(line)
	}
	var frame Object
	if err := json.Unmarshal(line, &frame); err != nil {
		return 0, err
	}
	var method string
	_ = json.Unmarshal(frame["method"], &method)
	name, editorID := untag(method)
	if editorID == nil {
		return w.Writer.Write(line)
	}
	frame["method"] = encoded(name)
	w.ids.Store(string(frame["id"]), editorID)
	data, err := json.Marshal(frame)
	if err != nil {
		return 0, err
	}
	_, err = w.Writer.Write(append(data, '\n'))
	if err != nil {
		w.ids.Delete(string(frame["id"]))
		return 0, err
	}
	return len(line), nil
}

type wireReader struct {
	scan    *bufio.Scanner
	pending []byte
	adapter *adapterWire
}

func incoming(r io.Reader, adapter *adapterWire) io.Reader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 10<<20)
	return &wireReader{scan: s, adapter: adapter}
}
func (r *wireReader) Read(dst []byte) (int, error) {
	if len(r.pending) == 0 {
		if !r.scan.Scan() {
			if err := r.scan.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := r.scan.Bytes()
		var head struct {
			ID     json.RawMessage
			Method string
		}
		if json.Unmarshal(line, &head) == nil {
			method := head.Method
			if r.adapter != nil && method == "" {
				r.adapter.ids.Delete(string(head.ID))
			}
			if method != "" && method != "$/cancel_request" {
				if strings.HasPrefix(method, "\x00") {
					return 0, fmt.Errorf("invalid ACP method name")
				}
				if head.ID == nil {
					method = notificationPrefix + method
				}
				if head.ID != nil && r.adapter == nil {
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
