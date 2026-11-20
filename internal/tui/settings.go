package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"
	"petal/internal/proxy"
	"petal/internal/sessions"
)

type valueChoice struct {
	Name, Value, Description string
	Options                  []valueChoice
}
type setting struct {
	ID, Name, Type, Category, Description string
	CurrentValue                          json.RawMessage
	Options                               []valueChoice
}
type settingsUpdate struct {
	Config        []setting
	Modes         []option
	CurrentModeID string
}

func settingsFrom(data proxy.Object) settingsUpdate {
	var u settingsUpdate
	_ = json.Unmarshal(data["configOptions"], &u.Config)
	var modes struct {
		AvailableModes []struct{ ID, Name string }
		CurrentModeID  string
	}
	if json.Unmarshal(data["modes"], &modes) == nil {
		u.Modes = []option{}
		u.CurrentModeID = modes.CurrentModeID
		for _, m := range modes.AvailableModes {
			u.Modes = append(u.Modes, option{m.ID, m.Name})
		}
	}
	return u
}

type settingResult struct {
	selection sessions.Selection
	options   settingsUpdate
	err       error
}

func (s setting) current() string {
	var value string
	if json.Unmarshal(s.CurrentValue, &value) == nil {
		return value
	}
	if s.Type == "boolean" && (string(s.CurrentValue) == "true" || string(s.CurrentValue) == "false") {
		return string(s.CurrentValue)
	}
	return ""
}

func (s setting) currentLabel() string {
	current := s.current()
	for _, choice := range s.choices() {
		if choice.Value == current {
			return choice.Name
		}
	}
	return current
}

func (s setting) choices() []valueChoice {
	switch s.Type {
	case "boolean":
		return []valueChoice{{Value: "true", Name: "On"}, {Value: "false", Name: "Off"}}
	case "select":
		var choices []valueChoice
		for _, choice := range s.Options {
			if choice.Options == nil {
				choices = append(choices, choice)
			} else {
				for _, item := range choice.Options {
					item.Name = choice.Name + " · " + item.Name
					choices = append(choices, item)
				}
			}
		}
		return choices
	default:
		return nil
	}
}

func (s setting) hasChoices() bool {
	switch s.Type {
	case "boolean":
		return true
	case "select":
		return slices.ContainsFunc(s.Options, func(choice valueChoice) bool {
			return choice.Options == nil || len(choice.Options) > 0
		})
	default:
		return false
	}
}

func (u settingsUpdate) byCategory(category string) setting {
	for _, s := range u.Config {
		if s.Category == category && s.hasChoices() {
			return s
		}
	}
	return setting{}
}

type settingMenu struct {
	title   string
	target  setting
	options []option
}

func settingLabel(name, detail string) string {
	if detail == "" {
		return name
	}
	return name + " · " + detail
}

func (m *terminal) openSettings(category string) {
	menu := settingMenu{title: "Choose an agent setting"}
	var current string
	switch {
	case category == "":
		for _, s := range m.agentSettings.Config {
			if !s.hasChoices() {
				continue
			}
			label := settingLabel(s.Name, s.currentLabel())
			menu.options = append(menu.options, option{s.ID, settingLabel(label, s.Description)})
		}
	case category == "mode" && m.agentSettings.byCategory(category).ID == "":
		menu.title = "Choose agent mode"
		menu.target.Type = "mode"
		current = m.agentSettings.CurrentModeID
		for _, mode := range m.agentSettings.Modes {
			if mode.OptionID == current {
				mode.Name += " (current)"
			}
			menu.options = append(menu.options, mode)
		}
	default:
		m.openSettingValues(m.agentSettings.byCategory(category))
		return
	}
	m.openSettingPicker(menu, current)
}

func (m *terminal) openSettingValues(s setting) {
	menu := settingMenu{title: s.Name, target: s}
	current := s.current()
	for _, choice := range s.choices() {
		label := choice.Name
		if choice.Value == current {
			label += " (current)"
		}
		menu.options = append(menu.options, option{choice.Value, settingLabel(label, choice.Description)})
	}
	m.openSettingPicker(menu, current)
}

func (m *terminal) openSettingPicker(menu settingMenu, current string) {
	if len(menu.options) == 0 {
		return
	}
	picker := newPicker(m.cfg)
	picker.setting = &menu
	picker.embedded = true
	picker.models = nil
	for _, opt := range menu.options {
		picker.models = append(picker.models, opt.Name)
	}
	picker.width, picker.height = m.Width, m.Height
	picker.filter()
	for i, opt := range menu.options {
		if opt.OptionID == current {
			picker.index = i
			break
		}
	}
	m.launch = picker
}

func (m *terminal) selectSetting(picker *picker) tea.Cmd {
	menu := picker.setting
	value := menu.options[picker.matches[picker.index].Index].OptionID
	m.launch = nil
	if menu.target.Type == "" {
		for _, s := range m.agentSettings.Config {
			if s.ID == value {
				m.openSettingValues(s)
				break
			}
		}
		return nil
	}
	m.changingSetting = true
	m.Status = "Applying setting"
	h, ctx, target := m.h, m.ctx, menu.target
	return func() tea.Msg { return h.changeSetting(ctx, target, value) }
}

func (h *harness) changeSetting(ctx context.Context, target setting, value string) settingResult {
	params := map[string]any{"sessionId": h.id}
	method := "session/set_config_option"
	if target.Type == "mode" {
		method = "session/set_mode"
		params["modeId"] = value
	} else {
		params["configId"], params["value"] = target.ID, value
		if target.Type == "boolean" {
			params["type"] = "boolean"
			params["value"] = value == "true"
		}
	}
	r, err := acp.SendRequest[proxy.Object](h.conn, ctx, method, params)
	updated := settingsFrom(r)
	if err == nil && target.Type == "mode" {
		updated.CurrentModeID = value
	}
	return settingResult{options: updated, err: err, selection: sessions.Selection{ID: target.ID, Value: value, LegacyMode: target.Type == "mode"}}
}

func contentText(raw json.RawMessage) string {
	var b struct {
		Type, Text, MimeType, Name, URI string
		Resource                        struct{ URI, Text, MimeType string }
	}
	if json.Unmarshal(raw, &b) != nil {
		return ""
	}
	switch b.Type {
	case "text", "":
		return b.Text
	case "resource_link":
		return strings.TrimSpace(b.Name + " " + b.URI)
	case "resource":
		return strings.TrimSpace(b.Resource.URI + "\n" + b.Resource.Text)
	case "image", "audio":
		return fmt.Sprintf("[%s · %s]", b.Type, b.MimeType)
	}
	return ""
}
