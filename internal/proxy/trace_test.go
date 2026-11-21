package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestFirstUpdateAfterV2PromptAcknowledgment(t *testing.T) {
	var log bytes.Buffer
	trace := &requestTrace{slog.New(slog.NewJSONHandler(&log, nil)), time.Now()}
	ctx := context.WithValue(context.Background(), traceKey{}, trace)
	w := &adapterWire{nativeV2: true}
	done := traceAdapter(ctx, w, "session/prompt", Object{"sessionId": encoded("s")})
	done(nil, nil)
	r := incoming(strings.NewReader(`{"method":"session/update","params":{"sessionId":"s","update":{}}}`+"\n"), w)
	r.traceUpdates = trace.log
	if _, err := r.Read(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), `"msg":"first_session_update"`) {
		t.Fatal(log.String())
	}
}
