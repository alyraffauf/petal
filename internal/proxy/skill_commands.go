package proxy

import (
	"encoding/json"
	"strings"
	"unicode"
)

// Editors such as T3 Code list an ACP command as a skill only when its name
// starts with "$", which is how Codex advertises skills. Claude Code advertises
// skills as plain commands and marks their source at the end of the
// description, as in "Review a PR. (user)". advertiseSkills adds a "$name" copy
// of each marked command and returns the skill names, so a prompt that starts
// with "$name" can be sent to Claude Code as "/name".
func advertiseSkills(item Object) (map[string]bool, bool) {
	var commands []Object
	if json.Unmarshal(item["availableCommands"], &commands) != nil {
		return nil, false
	}
	listed := make(map[string]bool, len(commands))
	for _, c := range commands {
		listed[stringField(c, "name")] = true
	}
	skills := map[string]bool{}
	for _, c := range commands {
		name := stringField(c, "name")
		description, ok := skillDescription(stringField(c, "description"))
		if !ok || name == "" || strings.HasPrefix(name, "$") || listed["$"+name] {
			continue
		}
		skills[name] = true
		skill := make(Object, len(c))
		for k, v := range c {
			skill[k] = v
		}
		skill["name"] = encoded("$" + name)
		skill["description"] = encoded(description)
		commands = append(commands, skill)
	}
	if len(skills) == 0 {
		return nil, false
	}
	item["availableCommands"], _ = json.Marshal(commands)
	return skills, true
}

// skillDescription strips Claude Code's skill source tag, reporting false when
// the description has none. Built-in commands carry no tag.
func skillDescription(description string) (string, bool) {
	trimmed := strings.TrimRightFunc(description, unicode.IsSpace)
	open := strings.LastIndex(trimmed, " (")
	if open < 0 || !strings.HasSuffix(trimmed, ")") {
		return "", false
	}
	switch source := trimmed[open+2 : len(trimmed)-1]; {
	case source == "user", source == "project", strings.HasPrefix(source, "plugin"):
		return trimmed[:open], true
	}
	return "", false
}

// invokeSkill rewrites a prompt that starts with "$name" to start with "/name",
// the form Claude Code runs as a skill. Other prompts are returned unchanged.
func invokeSkill(prompt json.RawMessage, skills map[string]bool) (json.RawMessage, bool) {
	var blocks []Object
	if json.Unmarshal(prompt, &blocks) != nil || len(blocks) == 0 || stringField(blocks[0], "type") != "text" {
		return prompt, false
	}
	text := stringField(blocks[0], "text")
	rest := strings.TrimLeftFunc(text, unicode.IsSpace)
	name, ok := strings.CutPrefix(rest, "$")
	if !ok {
		return prompt, false
	}
	if end := strings.IndexFunc(name, unicode.IsSpace); end >= 0 {
		name = name[:end]
	}
	if !skills[name] {
		return prompt, false
	}
	blocks[0]["text"] = encoded("/" + rest[1:])
	rewritten, err := json.Marshal(blocks)
	if err != nil {
		return prompt, false
	}
	return rewritten, true
}
