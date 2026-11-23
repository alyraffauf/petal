package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/paths"
)

// Editors discover the picker through session/new, before they send a model.
// A local picker remains available when the default adapter cannot open.
type editorSessions struct {
	commonV1 bool
	mu       sync.Mutex
	hello    json.RawMessage
	routes   map[string]*proxy
	sessions sync.Map
}
type editorSession struct {
	mu       sync.Mutex
	cwd      string
	setup    Object
	route    *proxy
	routedID string
	options  json.RawMessage
}

func (p *proxy) modelOption() Object {
	var choices []map[string]string
	for _, id := range p.PickerModels() {
		choices = append(choices, map[string]string{"value": id, "name": p.ModelLabel(id)})
	}
	options, _ := json.Marshal(choices)
	key := "id"
	if p.version == 2 {
		key = "configId"
	}
	return Object{key: encoded("model"), "category": encoded("model"), "name": encoded("Model"), "type": encoded("select"), "currentValue": encoded(p.PickerModel(p.Model)), "options": options}
}

func projectKey(cwd string) string {
	if info, err := os.Stat(cwd); err == nil && info.IsDir() {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			return fmt.Sprintf("dir:%d:%d", stat.Dev, stat.Ino)
		}
	}
	return filepath.Clean(cwd)
}

func editorBindingPath(key, id string) (string, error) {
	return paths.XDG("STATE", fmt.Sprintf("editor-sessions/%x", sha256.Sum256([]byte(key+"\x00"+id))))
}

func saveEditorBinding(cwd, id, routed string) error {
	path, err := editorBindingPath(cwd, id)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".binding-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(routed)
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp.Name(), path)
}

func (p *proxy) editorRoute(ctx context.Context, model string) (*proxy, error) {
	if model == p.Model {
		return p, nil
	}
	e := p.editorSessions
	e.mu.Lock()
	defer e.mu.Unlock()
	if route := e.routes[model]; route != nil {
		return route, nil
	}
	if !slices.Contains(p.ModelNames(), model) {
		return nil, acp.NewInvalidParams("unknown Petal model")
	}
	route := &proxy{recorder: p.recorder, Config: p.Config, editor: p.editor, editorWire: p.editorWire, lifetime: p.lifetime}
	route.Model = model
	route.configureDelegation()
	var hello Object
	if err := json.Unmarshal(e.hello, &hello); err != nil {
		return nil, err
	}
	hello["protocolVersion"], _ = json.Marshal(p.version)
	raw, _ := json.Marshal(hello)
	var accounts []string
	for _, name := range slices.Sorted(maps.Keys(route.Accounts)) {
		if len(route.Accounts[name].Models[model]) > 0 && (route.PinnedAccount == "" || route.PinnedAccount == name) {
			accounts = append(accounts, name)
		}
	}
	var err error
	if p.version == 2 {
		var ok bool
		_, ok, err = route.initializeV2(ctx, raw, accounts, 2)
		if err == nil && !ok {
			err = acp.NewInvalidParams("model adapter does not support the negotiated ACP version")
		}
	} else {
		_, err = route.initializeV1(ctx, raw, accounts, false)
	}
	if err != nil {
		return nil, err
	}

	e.routes[model] = route
	return route, nil
}

func (p *proxy) openEditorSession(ctx context.Context, params Object) (any, error) {
	// Clients using session/options can send their selection in the open request.
	var initial []Object
	if data := params["configOptions"]; data != nil && json.Unmarshal(data, &initial) != nil {
		return nil, acp.NewInvalidParams("configOptions must be an array")
	}
	for i, option := range initial {
		if stringField(option, "configId") != "model" || !slices.Contains(p.ModelNames(), stringField(option, "value")) {
			continue
		}
		route, err := p.editorRoute(ctx, stringField(option, "value"))
		if err != nil {
			return nil, err
		}
		params["configOptions"], _ = json.Marshal(slices.Delete(initial, i, i+1))
		raw, _ := json.Marshal(params)
		return route.handle(ctx, "session/new", raw)
	}
	if !filepath.IsAbs(stringField(params, "cwd")) {
		return nil, acp.NewInvalidParams("cwd must be absolute")
	}
	id := "petal-" + rand.Text()
	s := &editorSession{setup: params, cwd: projectKey(stringField(params, "cwd"))}
	// A working default adapter supplies model-specific settings before the first
	// prompt. Keep the local picker usable if that route cannot start.
	if _, err := p.bindEditorSession(ctx, s, id, p.Model); err == nil {
		p.editorSessions.sessions.Store(id, s)
		response := p.pendingModelOptions(s)
		response["sessionId"] = encoded(id)
		return response, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.editorSessions.sessions.Store(id, s)
	return map[string]any{"sessionId": id, "configOptions": []Object{p.modelOption()}}, nil
}

func (p *proxy) bindEditorSession(ctx context.Context, s *editorSession, id, model string) (Object, error) {
	route, err := p.editorRoute(ctx, model)
	if err != nil {
		return nil, err
	}
	setup, _ := json.Marshal(s.setup)
	result, err := route.handle(ctx, "session/new", setup)
	if err != nil {
		return nil, err
	}
	opened, ok := result.(Object)
	if !ok {
		return nil, fmt.Errorf("invalid session/new response")
	}
	routed := stringField(opened, "sessionId")
	if err = saveEditorBinding(s.cwd, id, routed); err != nil {
		return nil, err
	}
	s.route, s.routedID = route, routed
	s.options = opened["configOptions"]
	route.aliases.Store(routed, id)
	return opened, nil
}

func (p *proxy) pendingModelOptions(s *editorSession) Object {
	return s.route.editorOptions(s.options, s.setup != nil)
}

func (p *proxy) editorOptions(raw json.RawMessage, switchable bool) Object {
	var options []Object
	_ = json.Unmarshal(raw, &options)
	if switchable {
		options = slices.DeleteFunc(options, func(o Object) bool {
			return stringField(o, "category") == "model" || stringField(o, "id") == "model" || stringField(o, "configId") == "model"
		})
		options = append([]Object{p.modelOption()}, options...)
	}
	for _, o := range options {
		if stringField(o, "category") == "thought_level" {
			key := "id"
			if o["configId"] != nil {
				key = "configId"
			}
			o[key] = encoded("effort")
		}
	}
	data, _ := json.Marshal(options)
	return Object{"configOptions": data}
}

func (p *proxy) handleEditor(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	e := p.editorSessions
	if method == "initialize" {
		e.mu.Lock()
		defer e.mu.Unlock()
		result, err := p.handle(ctx, method, raw)
		if err == nil {
			e.hello = slices.Clone(raw)
		}
		return result, err
	}
	var params Object
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
	}
	p.mu.Lock()
	ready := p.initialized && !p.initializing
	if ready && !strings.HasPrefix(method, notificationPrefix) {
		p.used = true
	}
	p.mu.Unlock()
	if !ready {
		return p.handle(ctx, method, raw)
	}
	id := stringField(params, "sessionId")
	if method == "session/list" {
		return p.editorHistory(ctx, params)
	}
	if method == "session/options" && id == "" {
		return map[string]any{"configOptions": []Object{p.modelOption()}}, nil
	}
	if method == "session/new" {
		return p.openEditorSession(ctx, params)
	}
	value, found := e.sessions.Load(id)
	if !found && strings.HasPrefix(id, "petal-") && (method == "session/load" || method == "session/resume") {
		cwd := filepath.Clean(stringField(params, "cwd"))
		path, err := editorBindingPath(projectKey(cwd), id)
		if err != nil {
			return nil, err
		}
		routed, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			legacy, pathErr := editorBindingPath(cwd, id)
			if pathErr == nil {
				routed, err = os.ReadFile(legacy)
				if err == nil {
					if err = saveEditorBinding(projectKey(cwd), id, string(routed)); err == nil {
						_ = os.Remove(legacy)
					}
				}
			}
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, unknownSession(id)
		}
		if err != nil {
			return nil, err
		}
		parts := decodeRouteID(string(routed), 3)
		if parts == nil {
			return nil, unknownSession(id)
		}
		route, err := p.editorRoute(ctx, parts[1])
		if err != nil {
			return nil, err
		}
		value, _ = e.sessions.LoadOrStore(id, &editorSession{route: route, cwd: projectKey(stringField(params, "cwd")), routedID: string(routed)})
		found = true
	}
	if !found {
		if parts := decodeRouteID(id, 3); parts != nil && parts[1] != p.Model {
			route, err := p.editorRoute(ctx, parts[1])
			if err != nil {
				return nil, err
			}
			return route.handle(ctx, method, raw)
		}
		return p.handle(ctx, method, raw)
	}
	s := value.(*editorSession)
	if (method == "session/load" || method == "session/resume") && projectKey(stringField(params, "cwd")) != s.cwd {
		return nil, unknownSession(id)
	}
	s.mu.Lock()
	selecting := method == "session/set_model" || method == "session/set_config_option" && stringField(params, "configId") == "model"
	requested := stringField(params, "value")
	if method == "session/set_model" {
		requested = stringField(params, "modelId")
	}
	if method == "session/options" && s.route != nil && s.setup != nil {
		options := p.pendingModelOptions(s)
		s.mu.Unlock()
		return options, nil
	}
	repick := selecting && s.route != nil && s.setup != nil && slices.Contains(p.ModelNames(), requested) && requested != s.route.Model
	if selecting && s.route != nil && s.setup != nil && requested == s.route.Model {
		options := p.pendingModelOptions(s)
		s.mu.Unlock()
		if method == "session/set_model" {
			return Object{}, nil
		}
		return options, nil
	}
	if s.route == nil || repick {
		if method == "session/options" {
			s.mu.Unlock()
			return map[string]any{"configOptions": []Object{p.modelOption()}}, nil
		}
		if method == "session/close" || method == "session/delete" {
			e.sessions.Delete(id)
			s.mu.Unlock()
			return Object{}, nil
		}
		if method == "session/cancel" {
			s.mu.Unlock()
			return Object{}, nil
		}
		if !selecting && method != "session/prompt" && !strings.HasPrefix(method, notificationPrefix) {
			s.mu.Unlock()
			return nil, acp.NewInvalidParams("choose a model before configuring adapter options")
		}
		if strings.HasPrefix(method, notificationPrefix) {
			s.mu.Unlock()
			return nil, nil
		}
		model := p.Model
		oldRoute, oldID := s.route, s.routedID
		if selecting {
			model = stringField(params, "value")
			if method == "session/set_model" {
				model = stringField(params, "modelId")
			}
		}
		_, err := p.bindEditorSession(ctx, s, id, model)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if repick && oldID != s.routedID {
			oldRoute.aliases.Delete(oldID)
			closeParams, _ := json.Marshal(Object{"sessionId": encoded(oldID)})
			_, _ = oldRoute.handle(ctx, "session/close", closeParams)
		}
		if selecting {
			returnOptions := p.pendingModelOptions(s)
			s.mu.Unlock()
			if method == "session/set_model" {
				update := Object{"sessionUpdate": encoded("config_option_update"), "configOptions": returnOptions["configOptions"]}
				if err := p.editor.SendNotification(ctx, "session/update", map[string]any{"sessionId": id, "update": update}); err != nil {
					return nil, err
				}
				return Object{}, nil
			}
			return returnOptions, nil
		}

	}
	if method == "session/prompt" && s.setup != nil {
		s.setup = nil
		if s.options != nil {
			update := s.route.editorOptions(s.options, false)
			update["sessionUpdate"] = encoded("config_option_update")
			_ = p.editor.SendNotification(ctx, "session/update", map[string]any{"sessionId": id, "update": update})
		}
	}
	if method == "session/set_config_option" && stringField(params, "configId") == "effort" {
		var options []Object
		_ = json.Unmarshal(s.options, &options)
		for _, o := range options {
			if stringField(o, "category") == "thought_level" {
				key := "id"
				if o["configId"] != nil {
					key = "configId"
				}
				params["configId"] = o[key]
				break
			}
		}
	}
	route, routed := s.route, s.routedID
	route.aliases.Store(routed, id)
	s.mu.Unlock()
	params["sessionId"] = encoded(routed)
	raw, _ = json.Marshal(params)
	result, err := route.handle(ctx, method, raw)
	if err == nil && (method == "session/set_config_option" || method == "session/options" || method == "session/resume" || method == "session/load") {
		var response Object
		switch v := result.(type) {
		case json.RawMessage:
			_ = json.Unmarshal(v, &response)
		case Object:
			response = v
		}
		if response["configOptions"] != nil {
			s.mu.Lock()
			if s.route == route {
				s.options = response["configOptions"]
				response["configOptions"] = p.pendingModelOptions(s)["configOptions"]
				result = response
			}
			s.mu.Unlock()
		}
	}
	if err == nil && method == "session/delete" {
		if path, err := editorBindingPath(s.cwd, id); err == nil {
			_ = os.Remove(path)
		}
	}
	if err == nil && (method == "session/close" || method == "session/delete") {
		e.sessions.Delete(id)
	}
	if obj, ok := result.(Object); ok && stringField(obj, "sessionId") == routed {
		obj["sessionId"] = encoded(id)
	}
	return result, err
}
