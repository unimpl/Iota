package iota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeTestSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: >-\n  Use for testing\n  skill workflows.\nlicense: MIT\ncompatibility: Go\nallowed-tools: bash\nmetadata:\n  version: '1'\n---\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return content
}

func newSkillAgent(t *testing.T, dir string, provider Provider) *Agent {
	t.Helper()
	agent, err := New(Config{Provider: provider, Model: "test", SkillsDir: dir, Tools: []Tool{NewReadSkillTool(), NewReadSkillResourceTool()}})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestSkillDiscoveryValidationAndReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".iota", "skills")
	agent, err := New(Config{Provider: &fakeProvider{}, Model: "test"})
	if err != nil || agent.Skills().Directory != dir || len(agent.Skills().Skills) != 0 {
		t.Fatalf("agent=%v err=%v", agent, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discovery created the installation directory")
	}
	writeTestSkill(t, dir, "review", "# Review\nFollow the checklist.")
	writeTestSkill(t, dir, "bad--name", "invalid")
	catalog, err := agent.ReloadSkills(t.Context(), nil)
	if err != nil || len(catalog.Skills) != 1 || len(catalog.Diagnostics) != 1 {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	skill := catalog.Skills[0]
	if skill.Name != "review" || skill.Description != "Use for testing skill workflows." || skill.Metadata["version"] != "1" || skill.AllowedTools != "bash" {
		t.Fatalf("metadata=%+v", skill)
	}
	catalog.Skills[0].Metadata["version"] = "mutated"
	if agent.Skills().Skills[0].Metadata["version"] != "1" {
		t.Fatal("catalog exposes mutable internal metadata")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ReloadSkills(t.Context(), nil); err == nil || len(agent.Skills().Skills) != 1 {
		t.Fatal("failed reload replaced the previous registry")
	}
}

func TestSkillFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review", "SKILL.md")
	for _, frontmatter := range []string{
		"name: review\ndescription: [invalid]",
		"name: review\ndescription: 123",
		"name: review\ndescription: valid\nmetadata:\n  version: 1",
		"name: different\ndescription: valid",
		"name: review\ndescription: ''",
		"name: review\nname: review\ndescription: valid",
		"name: review\ndescription: '" + strings.Repeat("x", 1025) + "'",
		"name: review\ndescription: valid\ncompatibility: '" + strings.Repeat("x", 501) + "'",
	} {
		if _, err := parseSkill(path, "---\n"+frontmatter+"\n---\nBody"); err == nil {
			t.Fatalf("invalid frontmatter accepted: %s", frontmatter)
		}
	}
	if _, err := parseSkill(path, "\ufeff---\r\nname: review\r\ndescription: 'Use for: review'\r\nx-extension: true\r\n---\r\nBody"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"代码审查", "révision", strings.Repeat("审", 64), "ｒｅｖｉｅｗ"} {
		path := filepath.Join(t.TempDir(), name, "SKILL.md")
		if _, err := parseSkill(path, "---\nname: "+name+"\ndescription: Unicode skill\n---\nBody"); err != nil {
			t.Fatalf("valid Unicode name rejected: %s: %v", name, err)
		}
	}
}

func TestSkillUnicodeLookupAndDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "review", "Canonical review instructions.")
	writeTestSkill(t, dir, "ｒｅｖｉｅｗ", "Shadowed review instructions.")
	writeTestSkill(t, dir, "代码审查", "中文指令。")
	provider := &fakeProvider{responses: []Response{{Content: "done"}}}
	agent := newSkillAgent(t, dir, provider)
	if len(agent.Skills().Skills) != 2 || len(agent.Skills().Diagnostics) != 1 {
		t.Fatal("normalized name collision was not diagnosed")
	}
	if _, err := agent.Run(t.Context(), "$skill:ｒｅｖｉｅｗ $skill:代码审查", nil); err != nil {
		t.Fatal(err)
	}
	if len(agent.ActiveSkills()) != 2 || !strings.Contains(agent.ActiveSkills()[0].Content, "Canonical review instructions.") {
		t.Fatal("Unicode lookup did not select the stable registered version")
	}
}

func TestSkillExplicitSelectionAndErrors(t *testing.T) {
	dir := t.TempDir()
	content := writeTestSkill(t, dir, "review", "Full original instructions.")
	provider := &fakeProvider{responses: []Response{{Content: "done"}}}
	agent := newSkillAgent(t, dir, provider)
	prompt := "请使用 $skill:review，检查代码。$skill:review"
	var activations []SkillActivation
	if _, err := agent.Run(t.Context(), prompt, func(event Event) {
		if event.Type == EventSkillActivated {
			activations = append(activations, *event.Skill)
			event.Skill.Skill.Metadata["version"] = "observer mutation"
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(activations) != 1 || activations[0].Source != "user" || len(provider.requests) != 1 {
		t.Fatalf("activations=%+v", activations)
	}
	request := provider.requests[0]
	if request.Messages[0].Content != prompt || request.Messages[1].Skill == nil || !strings.Contains(request.Messages[1].Content, content) || strings.Contains(request.SystemPrompt, content) {
		t.Fatal("explicit selection lost the prompt or injected the full catalog into system")
	}
	if strings.Contains(request.Messages[len(request.Messages)-1].Content, content) || agent.ActiveSkills()[0].Skill.Metadata["version"] != "1" {
		t.Fatal("runtime duplicated instructions or observer changed state")
	}
	before := agent.Messages()
	for _, invalid := range []string{"$skill:review $skill:missing", "$skill:review_bad", "$skill:"} {
		if _, err := agent.Run(t.Context(), invalid, nil); err == nil || !reflect.DeepEqual(agent.Messages(), before) {
			t.Fatalf("invalid selection changed history: %q err=%v", invalid, err)
		}
	}
	for _, literal := range []string{`\$skill:review`, "prefix$skill:review", "$$skill:review"} {
		if names, err := explicitSkillNames(literal); err != nil || len(names) != 0 {
			t.Fatalf("literal recognized: %q names=%v err=%v", literal, names, err)
		}
	}
}

func TestSkillModelActivationResourcesAndMode(t *testing.T) {
	dir := t.TempDir()
	content := writeTestSkill(t, dir, "review", "Read references/rules.md before review.")
	resource := filepath.Join(dir, "review", "references", "rules.md")
	if err := os.MkdirAll(filepath.Dir(resource), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("Reference rules."), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{
			{ID: "before", Name: "read_skill_resource", Arguments: json.RawMessage(`{"name":"review","path":"references/rules.md"}`)},
			{ID: "activate", Name: "read_skill", Arguments: json.RawMessage(`{"name":"review"}`)},
			{ID: "resource", Name: "read_skill_resource", Arguments: json.RawMessage(`{"name":"review","path":"references/rules.md"}`)},
			{ID: "escape", Name: "read_skill_resource", Arguments: json.RawMessage(`{"name":"review","path":"../outside"}`)},
		}}, {Content: "done"},
	}}
	agent := newSkillAgent(t, dir, provider)
	if err := agent.SetMode(ModePlan, nil); err != nil {
		t.Fatal(err)
	}
	var activated, read int
	if _, err := agent.Run(t.Context(), "Review the code", func(event Event) {
		if event.Type == EventSkillActivated {
			activated++
			if event.Skill.Source != "model" {
				t.Fatal("incorrect activation source")
			}
		}
		if event.Type == EventSkillResourceRead {
			read++
		}
	}); err != nil {
		t.Fatal(err)
	}
	messages := agent.Messages()
	if !messages[2].IsError || messages[3].IsError || messages[3].Skill == nil || !strings.Contains(messages[3].Content, content) || messages[4].Content != "Reference rules." || !messages[5].IsError || activated != 1 || read != 1 {
		t.Fatalf("messages=%+v activated=%d read=%d", messages, activated, read)
	}
	first := provider.requests[0]
	if first.Mode != ModePlan || !strings.Contains(first.SystemPrompt, "review") || strings.Contains(first.SystemPrompt, content) || len(first.Tools) != 2 {
		t.Fatal("catalog or read-only skill tools unavailable during planning")
	}
}

func TestSkillResourceBoundaries(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "review", "Body")
	root := filepath.Join(dir, "review")
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{0xff, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "huge"), []byte(strings.Repeat("x", maxSkillFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret", outside, "escape", ".", "binary", "huge"} {
		if _, err := readSkillFile(root, name); err == nil {
			t.Fatalf("unsafe resource accepted: %q", name)
		}
	}
	linked := filepath.Join(t.TempDir(), "review")
	if err := os.Symlink(root, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := readSkillFile(linked, "SKILL.md"); err != nil {
		t.Fatal("linked installation was not supported:", err)
	}
}

func TestSkillReloadDuringRunAndSnapshotVersion(t *testing.T) {
	dir := t.TempDir()
	old := writeTestSkill(t, dir, "review", "Old instruction version.")
	block := make(chan struct{})
	started := make(chan struct{})
	provider := &fakeProvider{block: block}
	agent := newSkillAgent(t, dir, provider)
	done := make(chan error, 1)
	go func() {
		_, err := agent.Run(t.Context(), "$skill:review", func(event Event) {
			if event.Type == EventModelRequest {
				close(started)
			}
		})
		done <- err
	}()
	<-started
	if _, err := agent.ReloadSkills(t.Context(), nil); !errors.Is(err, ErrBusy) {
		t.Fatal("reload accepted during a run:", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	updated := writeTestSkill(t, dir, "review", "New instruction version.")
	if _, err := agent.ReloadSkills(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if agent.ActiveSkills()[0].Content != old {
		t.Fatal("reload silently changed active instruction version")
	}
	provider.responses = []Response{{Content: "updated"}}
	if _, err := agent.Run(t.Context(), "$skill:review", nil); err != nil || agent.ActiveSkills()[0].Content != updated {
		t.Fatal("reactivation did not use the updated registry:", err)
	}
}

func TestSkillDurableActivationFailure(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "review", "Body")
	agent := newSkillAgent(t, dir, &fakeProvider{})
	activation, _ := agent.skillActivation("review", "user")
	failed := errors.New("write failed")
	if err := agent.activateSkill(t.Context(), activation, 0, nil, func(Event) error { return failed }); !errors.Is(err, failed) || len(agent.ActiveSkills()) != 0 {
		t.Fatal("failed durable activation changed state")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := agent.activateSkill(ctx, activation, 0, nil, nil); !errors.Is(err, context.Canceled) || len(agent.ActiveSkills()) != 0 {
		t.Fatal("canceled activation changed state")
	}
}

func TestSkillRepeatedActivationAvoidsDuplicateInstructions(t *testing.T) {
	dir := t.TempDir()
	content := writeTestSkill(t, dir, "review", "Instruction version.")
	provider := &fakeProvider{responses: []Response{
		{Content: "done"},
		{ToolCalls: []ToolCall{{ID: "again", Name: "read_skill", Arguments: json.RawMessage(`{"name":"review"}`)}}},
		{Content: "done again"},
	}}
	agent := newSkillAgent(t, dir, provider)
	for _, prompt := range []string{"$skill:review", "$skill:review use again"} {
		if _, err := agent.Run(t.Context(), prompt, nil); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for _, message := range agent.Messages() {
		if strings.Contains(message.Content, content) {
			count++
		}
	}
	if count != 1 || agent.ActiveSkills()[0].Source != "model" {
		t.Fatalf("body copies=%d active=%+v", count, agent.ActiveSkills())
	}
}

func TestSkillDisabledAndEmptyCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, disabled := range []bool{false, true} {
		agent, err := New(Config{Provider: &fakeProvider{}, Model: "test", DisableSkills: disabled, Tools: []Tool{NewReadSkillTool(), NewReadSkillResourceTool()}})
		if err != nil {
			t.Fatal(err)
		}
		request, err := agent.currentRequest(t.Context())
		if err != nil || len(request.Tools) != 0 || strings.Contains(request.SystemPrompt, "available-skills") {
			t.Fatal("empty or disabled registry exposes irrelevant tools or catalog:", err)
		}
		if disabled {
			if _, err := agent.ReloadSkills(t.Context(), nil); err == nil {
				t.Fatal("disabled discovery can be reloaded")
			}
		}
	}
}
