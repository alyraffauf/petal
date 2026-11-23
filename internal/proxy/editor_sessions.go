package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/paths"
	"petal/internal/sessions"
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
	// request holds the cwd and MCP servers that opened the conversation, so a
	// model switch can open the same conversation on another route.
	request Object
	// handoff is the routed session whose context the next prompt carries to
	// the route chosen by a model switch.
	handoff string
	// lineage is the history a fork inherited from the conversations it was
	// forked from, oldest first. Petal's archive holds only a fork's own turns,
	// so a model switch hands these off too.
	lineage []historySegment
	// legacyFork marks a fork saved before Petal recorded lineage. Its earlier
	// history is unknown, so it cannot switch models.
	legacyFork bool
}

// historySegment is an archived conversation up to the row where a fork left it.
type historySegment struct {
	ID   string `json:"id"`
	Head int64  `json:"head"`
}

// forkLine encodes the fork's lineage for the third line of its binding.
func (s *editorSession) forkLine() string {
	if s.legacyFork {
		return "fork"
	}
	if len(s.lineage) == 0 {
		return ""
	}
	line, _ := json.Marshal(s.lineage)
	return string(line)
}

// saveBinding persists a change to the pending handoff, so a close and resume
// or a restart before the next prompt keeps it. The caller holds s.mu.
func (s *editorSession) saveBinding(id string) {
	if err := saveEditorBinding(s.cwd, id, s.routedID, s.handoff, s.forkLine()); err != nil {
		slog.Error("save editor session binding", "error", err)
	}
}

// openRequest keeps the fields of session/new from a session/new, load, or
// resume request.
func openRequest(params Object) Object {
	request := maps.Clone(params)
	delete(request, "sessionId")
	delete(request, "configOptions")
	return request
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

// saveEditorBinding records the route an editor session runs on. A second
// line holds the routed session whose context its next prompt carries, and a
// third line holds a fork's lineage.
func saveEditorBinding(cwd, id, routed, handoff, fork string) error {
	if handoff != "" || fork != "" {
		routed += "\n" + handoff
	}
	if fork != "" {
		routed += "\n" + fork
	}
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
	route := &proxy{recorder: p.recorder, Config: p.Config, editor: p.editor, editorWire: p.editorWire, lifetime: p.lifetime, editorPicker: true}
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
		result, err := route.handle(ctx, "session/new", raw)
		if err != nil {
			return nil, err
		}
		// Track the session like one opened without a selection, so the routed
		// model list it advertises can switch it.
		opened, ok := result.(Object)
		routed := stringField(opened, "sessionId")
		if !ok || routed == "" {
			return result, nil
		}
		s := &editorSession{setup: openRequest(params), route: route, routedID: routed, options: opened["configOptions"], request: openRequest(params), cwd: projectKey(stringField(params, "cwd"))}
		route.aliases.Store(routed, routed)
		p.editorSessions.sessions.Store(routed, s)
		opened["configOptions"] = p.sessionModelOptions(s)["configOptions"]
		return opened, nil
	}
	if !filepath.IsAbs(stringField(params, "cwd")) {
		return nil, acp.NewInvalidParams("cwd must be absolute")
	}
	id := "petal-" + rand.Text()
	s := &editorSession{setup: params, request: openRequest(params), cwd: projectKey(stringField(params, "cwd"))}
	// A working default adapter supplies model-specific settings before the first
	// prompt. Keep the local picker usable if that route cannot start.
	if _, err := p.bindEditorSession(ctx, s, id, p.Model); err == nil {
		p.editorSessions.sessions.Store(id, s)
		response := p.sessionModelOptions(s)
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
	request := s.setup
	if request == nil {
		request = s.request
	}
	setup, _ := json.Marshal(request)
	result, err := route.handle(ctx, "session/new", setup)
	if err != nil {
		return nil, err
	}
	opened, ok := result.(Object)
	if !ok {
		return nil, fmt.Errorf("invalid session/new response")
	}
	routed := stringField(opened, "sessionId")
	if err = saveEditorBinding(s.cwd, id, routed, s.handoff, s.forkLine()); err != nil {
		return nil, err
	}
	s.route, s.routedID = route, routed
	s.options = opened["configOptions"]
	route.aliases.Store(routed, id)
	return opened, nil
}

func (p *proxy) sessionModelOptions(s *editorSession) Object {
	return s.route.editorOptions(s.options)
}

// editorOptions replaces the adapter's own model option with Petal's routed
// model list. Editors such as T3 Code treat the session's model option as the
// provider's whole catalog, so it must list every route even after a session
// is bound to one of them.
func (p *proxy) editorOptions(raw json.RawMessage) Object {
	var options []Object
	_ = json.Unmarshal(raw, &options)
	options = slices.DeleteFunc(options, func(o Object) bool {
		return stringField(o, "category") == "model" || stringField(o, "id") == "model" || stringField(o, "configId") == "model"
	})
	options = append([]Object{p.modelOption()}, options...)
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
	if !found && (strings.HasPrefix(id, "petal-") || decodeRouteID(id, 3) != nil) && (method == "session/load" || method == "session/resume") {
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
					if err = saveEditorBinding(projectKey(cwd), id, string(routed), "", ""); err == nil {
						_ = os.Remove(legacy)
					}
				}
			}
		}
		if errors.Is(err, os.ErrNotExist) && !strings.HasPrefix(id, "petal-") {
			// A session the editor imported from session/list is its own route
			// until a model switch binds it to another one.
			routed, err = []byte(id), nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, unknownSession(id)
		}
		if err != nil {
			return nil, err
		}
		lines := strings.SplitN(string(routed), "\n", 3)
		bound, handoff := lines[0], ""
		if len(lines) > 1 {
			handoff = lines[1]
		}
		var lineage []historySegment
		legacyFork := false
		if len(lines) == 3 {
			// A fork saved before lineage was recorded has only a "fork" marker.
			legacyFork = json.Unmarshal([]byte(lines[2]), &lineage) != nil
		}
		parts := decodeRouteID(bound, 3)
		if parts == nil {
			return nil, unknownSession(id)
		}
		route, err := p.editorRoute(ctx, parts[1])
		if err != nil {
			return nil, err
		}
		value, _ = e.sessions.LoadOrStore(id, &editorSession{route: route, cwd: projectKey(stringField(params, "cwd")), routedID: bound, handoff: handoff, lineage: lineage, legacyFork: legacyFork, request: openRequest(params)})
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
	if method == "session/load" || method == "session/resume" {
		s.request = openRequest(params)
	}
	selecting := method == "session/set_model" || method == "session/set_config_option" && stringField(params, "configId") == "model"
	requested := stringField(params, "value")
	if method == "session/set_model" {
		requested = stringField(params, "modelId")
	}
	if method == "session/options" && s.route != nil && s.setup != nil {
		options := p.sessionModelOptions(s)
		s.mu.Unlock()
		return options, nil
	}
	// Choosing the real model shown for a "/default" route binds that model's
	// route, because the session's workspace may default to another model.
	repick := selecting && s.route != nil && slices.Contains(p.ModelNames(), requested) && requested != s.route.Model
	if repick && s.legacyFork {
		s.mu.Unlock()
		return nil, acp.NewInvalidParams("Petal did not record where this conversation was forked from, so switching models would lose the earlier conversation; start a new conversation to use another model")
	}
	if selecting && s.route != nil && requested == s.route.Model {
		options := p.sessionModelOptions(s)
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
		if repick && s.setup == nil && s.handoff == "" {
			// The new model learns the conversation only from Petal's archive.
			// A conversation the editor imported from an adapter may have none.
			// Closing the old route saves history it has observed but not stored.
			cwd := stringField(s.request, "cwd")
			rows, err := sessions.Transcript(cwd, oldID)
			if err == nil {
				err = lineageAvailable(cwd, s.lineage)
			}
			if err == nil && len(rows) == 0 && len(s.lineage) == 0 && (p.recorder == nil || !p.recorder.unsaved(oldID)) {
				err = acp.NewInvalidParams("Petal has no saved history for this conversation, so switching models would lose it; start a new conversation to use another model")
			}
			if err != nil {
				s.mu.Unlock()
				return nil, err
			}
		}
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
			// After the first prompt, the new route continues the conversation.
			// Keep the oldest unsent handoff when the model changes twice.
			if s.setup == nil && s.handoff == "" {
				s.handoff = oldID
				s.saveBinding(id)
			}
			oldRoute.aliases.Delete(oldID)
			closeParams, _ := json.Marshal(Object{"sessionId": encoded(oldID)})
			_, _ = oldRoute.handle(ctx, "session/close", closeParams)
		}
		if selecting {
			returnOptions := p.sessionModelOptions(s)
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
	if method == "session/fork" && s.handoff != "" {
		// The new model has not received the earlier conversation yet, so a
		// fork of its session would start without it.
		s.mu.Unlock()
		return nil, acp.NewInvalidParams("send a prompt after switching models before forking this conversation")
	}
	handoff := ""
	var handoffLineage []historySegment
	if method == "session/prompt" {
		s.setup = nil
		if s.handoff != "" {
			prompt, err := handoffPrompt(stringField(s.request, "cwd"), s.lineage, s.handoff, params["prompt"])
			if err != nil {
				s.mu.Unlock()
				return nil, err
			}
			// The prompt consumes the handoff. A later switch hands off from this
			// route, which records the prompt, rather than the one before it.
			params["prompt"], handoff, handoffLineage = prompt, s.handoff, s.lineage
			// The new route records this prompt, so its archive now holds the
			// inherited history and the lineage is no longer needed.
			s.handoff, s.lineage = "", nil
			s.saveBinding(id)
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
	lineage, legacyFork := slices.Clone(s.lineage), s.legacyFork
	route.aliases.Store(routed, id)
	s.mu.Unlock()
	params["sessionId"] = encoded(routed)
	raw, _ = json.Marshal(params)
	result, err := route.handle(ctx, method, raw)
	if err != nil && handoff != "" {
		// Retry the handoff with the next prompt unless the model changed since.
		s.mu.Lock()
		if s.route == route && s.routedID == routed && s.handoff == "" {
			s.handoff, s.lineage = handoff, handoffLineage
			s.saveBinding(id)
		}
		s.mu.Unlock()
	}
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
				response["configOptions"] = p.sessionModelOptions(s)["configOptions"]
				result = response
			}
			s.mu.Unlock()
		}
	}
	if err == nil && method == "session/fork" {
		// Track the fork so its model option and selections go through Petal.
		if opened, ok := result.(Object); ok {
			if forked := stringField(opened, "sessionId"); forked != "" && forked != routed {
				fs := &editorSession{route: route, routedID: forked, legacyFork: legacyFork, options: opened["configOptions"], request: openRequest(params), cwd: projectKey(stringField(params, "cwd"))}
				// The fork inherits the parent's conversation up to its last
				// archived row. Rows the parent adds later are not the fork's.
				if head, err := sessions.TranscriptHead(ctx, stringField(params, "cwd"), routed); err == nil {
					fs.lineage = append(lineage, historySegment{ID: routed, Head: head})
				} else {
					fs.legacyFork = true
				}
				route.aliases.Store(forked, forked)
				e.sessions.Store(forked, fs)
				fs.saveBinding(forked)
				if opened["configOptions"] != nil {
					opened["configOptions"] = p.sessionModelOptions(fs)["configOptions"]
				}
			}
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
