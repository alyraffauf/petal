package proxy

import (
	"encoding/json"
	"testing"
)

func TestAdvertiseSkillsAddsDollarCommandsForTaggedSkills(t *testing.T) {
	item := Object{"availableCommands": json.RawMessage(`[
		{"name":"babysit-pr","description":"Watch a PR. (user)","input":null},
		{"name":"lint","description":"Lint this repo. (project)"},
		{"name":"review:deep","description":"Deep review. (plugin:review)"},
		{"name":"compact","description":"Free up context","input":{"hint":"<instructions>"}},
		{"name":"deep-research","description":"Research. (dynamic workflow)"},
		{"name":"$how","description":"Explain code."},
		{"name":"how","description":"Explain code. (user)"}
	]`)}
	skills, ok := advertiseSkills(item)
	if !ok {
		t.Fatal("no skills advertised")
	}
	var commands []struct{ Name, Description string }
	if err := json.Unmarshal(item["availableCommands"], &commands); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range commands {
		got[c.Name] = c.Description
	}
	want := map[string]string{
		"$babysit-pr":  "Watch a PR.",
		"$lint":        "Lint this repo.",
		"$review:deep": "Deep review.",
	}
	for name, description := range want {
		if got[name] != description {
			t.Errorf("%s description = %q, want %q", name, got[name], description)
		}
	}
	for _, name := range []string{"$compact", "$deep-research", "$$how"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s was advertised as a skill", name)
		}
	}
	if len(commands) != 10 {
		t.Errorf("got %d commands, want the 7 originals plus 3 skills", len(commands))
	}
	if !skills["babysit-pr"] || !skills["lint"] || !skills["review:deep"] || skills["compact"] || skills["how"] {
		t.Errorf("skills = %v", skills)
	}
}

func TestAdvertiseSkillsLeavesCodexCommandsAlone(t *testing.T) {
	raw := json.RawMessage(`[{"name":"review","description":"Review changes"},{"name":"$how","description":"Explain code."}]`)
	item := Object{"availableCommands": raw}
	if _, ok := advertiseSkills(item); ok {
		t.Fatal("rewrote a command list with no tagged skills")
	}
	if string(item["availableCommands"]) != string(raw) {
		t.Fatalf("commands changed to %s", item["availableCommands"])
	}
}

func TestInvokeSkillRewritesLeadingSkillMention(t *testing.T) {
	skills := map[string]bool{"babysit-pr": true, "review:deep": true}
	for _, tc := range []struct{ in, want string }{
		{"$babysit-pr https://example.test/pull/1", "/babysit-pr https://example.test/pull/1"},
		{"  $review:deep\nthe diff", "/review:deep\nthe diff"},
		{"$babysit-pr", "/babysit-pr"},
		{"$babysit-pro now", "$babysit-pro now"},
		{"please $babysit-pr", "please $babysit-pr"},
		{"costs $5", "costs $5"},
	} {
		prompt, _ := json.Marshal([]map[string]string{{"type": "text", "text": tc.in}, {"type": "text", "text": "$babysit-pr"}})
		out, _ := invokeSkill(prompt, skills)
		var blocks []struct{ Text string }
		if err := json.Unmarshal(out, &blocks); err != nil {
			t.Fatal(err)
		}
		if blocks[0].Text != tc.want || blocks[1].Text != "$babysit-pr" {
			t.Errorf("%q became %q, %q; want %q and an untouched second block", tc.in, blocks[0].Text, blocks[1].Text, tc.want)
		}
	}
}
