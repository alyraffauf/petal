package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"time"
)

// The lookup key stays internal. Editor request IDs can contain arbitrary text.
type requestTrace struct {
	log   *slog.Logger
	start time.Time
}
type traceKey struct{}

func enableTrace(ctx context.Context, w *adapterWire) {
	path := os.Getenv("PETAL_ACP_TRACE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		slog.Warn("cannot open Petal ACP trace", "error", err)
		return
	}
	w.traceLog = slog.New(slog.NewJSONHandler(f, nil))
	context.AfterFunc(ctx, func() { _ = f.Close() })
}

func (w *adapterWire) traceRequest(ctx context.Context, method string, id, raw json.RawMessage) context.Context {
	if w.traceLog == nil || id == nil || (method != "session/prompt" && method != "session/set_config_option" && method != "session/set_model") {
		return ctx
	}
	t := &requestTrace{w.traceLog.With("request", rand.Text(), "method", method), time.Now()}
	w.responses.Store(string(id), t)
	var params Object
	if method != "session/prompt" {
		_ = json.Unmarshal(raw, &params)
	}
	t.event("editor_request_received", settingFields(params)...)
	return context.WithValue(ctx, traceKey{}, t)
}

func settingFields(params Object) []any {
	key := stringField(params, "configId")
	switch key {
	case "effort", "reasoning_effort", "model":
		return []any{"setting", key, "value", stringField(params, "value")}
	}
	if model := stringField(params, "modelId"); model != "" {
		return []any{"setting", "model", "value", model}
	}
	return nil
}
func (t *requestTrace) event(stage string, fields ...any) {
	if t != nil {
		t.log.Info(stage, append([]any{"elapsed_us", time.Since(t.start).Microseconds()}, fields...)...)
	}
}

func traceAdapter(ctx context.Context, w *adapterWire, method string, params Object) func(json.RawMessage, error) {
	t, _ := ctx.Value(traceKey{}).(*requestTrace)
	if t == nil {
		return func(json.RawMessage, error) {}
	}
	session := stringField(params, "sessionId")
	fields := append(settingFields(params), "adapter_session", traceSession(session))
	t.event("adapter_request_started", fields...)
	if method == "session/prompt" {
		w.firstUpdates.Store(session, t)
	}
	return func(raw json.RawMessage, err error) {
		if method == "session/prompt" && (!w.nativeV2 || err != nil) {
			w.firstUpdates.CompareAndDelete(session, t)
		}
		fields := []any{"ok", err == nil}
		if err == nil && method == "session/set_config_option" {
			var result struct{ ConfigOptions []Object }
			_ = json.Unmarshal(raw, &result)
			for _, o := range result.ConfigOptions {
				key := stringField(params, "configId")
				if (stringField(o, "id") == key || stringField(o, "configId") == key) && (key == "effort" || key == "reasoning_effort" || key == "model") {
					fields = append(fields, "setting", key, "acknowledged", stringField(o, "currentValue"))
					break
				}
			}
		}
		t.event("adapter_response_received", fields...)
	}
}

func traceSession(session string) string {
	digest := sha256.Sum256([]byte(session))
	return hex.EncodeToString(digest[:12])
}
