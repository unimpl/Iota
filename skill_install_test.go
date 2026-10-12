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

func newInstallSkillAgent(t *testing.T, dir string, provider Provider) *Agent {
	t.Helper()
	agent, err := New(Config{Provider: provider, Model: "test", SkillsDir: dir, Tools: []Tool{NewInstallSkillTool(), NewReadSkillTool(), NewReadSkillResourceTool()}})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestInstallSkillCopiesCompletePackageAndRefreshesCatalog(t *testing.T) {
	sources := t.TempDir()
	content := writeTestSkill(t, sources, "review", "Read references/rules.md.")
	sourceDir := filepath.Join(sources, "downloaded-package")
	if err := os.Rename(filepath.Join(sources, "review"), sourceDir); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "script-executed")
	for _, file := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{"references/rules.md", []byte("Complete reference rules."), 0o600},
		{"scripts/install.sh", []byte("#!/bin/sh\ntouch '" + sentinel + "'\n"), 0o700},
		{"assets/binary.dat", []byte{0x00, 0xff, 0x01}, 0o600},
		{".git/hooks/post-checkout", []byte("unused git data"), 0o700},
	} {
		path := filepath.Join(sourceDir, file.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file.data, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "skills")
	agent := newInstallSkillAgent(t, dir, &fakeProvider{})
	before, err := agent.currentRequest(t.Context())
	if err != nil || len(before.Tools) != 1 || before.Tools[0].Name != "install_skill" || !strings.Contains(before.SystemPrompt, "install_skill") {
		t.Fatal("first-skill installation is unavailable:", err)
	}
	var installedEvents int
	installation, err := agent.InstallSkill(t.Context(), sourceDir, func(event Event) {
		if event.Type == EventSkillInstalled {
			installedEvents++
			if event.SkillInstall.Hash != skillContentHash(content) || len(agent.Skills().Skills) != 1 {
				t.Fatal("installation event preceded registry update or lost version")
			}
			event.SkillInstall.Skill.Metadata["version"] = "observer mutation"
			event.Skills.Skills[0].Metadata["version"] = "catalog mutation"
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Skill.Name != "review" || installation.SourceDir != sourceDir || installedEvents != 1 || len(agent.ActiveSkills()) != 0 || agent.Skills().Skills[0].Metadata["version"] != "1" || installation.Skill.Metadata["version"] != "1" {
		t.Fatalf("installation=%+v events=%d", installation, installedEvents)
	}
	installed := filepath.Join(dir, "review")
	if data, err := os.ReadFile(filepath.Join(installed, "SKILL.md")); err != nil || string(data) != content {
		t.Fatal("manifest was not copied verbatim:", err)
	}
	if data, err := os.ReadFile(filepath.Join(installed, "assets", "binary.dat")); err != nil || !reflect.DeepEqual(data, []byte{0x00, 0xff, 0x01}) {
		t.Fatal("binary bundled asset was changed:", err)
	}
	if info, err := os.Stat(filepath.Join(installed, "scripts", "install.sh")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatal("script execution mode was lost:", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("installation executed a package script")
	}
	if _, err := os.Stat(filepath.Join(installed, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("repository internals were installed")
	}
	if err := os.RemoveAll(sourceDir); err != nil {
		t.Fatal(err)
	}
	request, err := agent.currentRequest(t.Context())
	if err != nil || len(request.Tools) != 3 || !strings.Contains(request.SystemPrompt, "review") {
		t.Fatal("installation did not refresh the next request:", err)
	}
	if text, err := readSkillFile(installed, "references/rules.md"); err != nil || text != "Complete reference rules." {
		t.Fatal("resource depends on the temporary source:", err)
	}
}

func TestInstallSkillRejectsDuplicateAndPreservesExistingFiles(t *testing.T) {
	sources := t.TempDir()
	content := writeTestSkill(t, sources, "review", "Original content.")
	agent := newInstallSkillAgent(t, t.TempDir(), &fakeProvider{})
	sourceDir := filepath.Join(sources, "review")
	installation, err := agent.InstallSkill(t.Context(), sourceDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTestSkill(t, sources, "review", "Replacement content.")
	if _, err := agent.InstallSkill(t.Context(), sourceDir, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal("duplicate installation overwrote a skill:", err)
	}
	if data, err := os.ReadFile(installation.Skill.Path); err != nil || string(data) != content {
		t.Fatal("existing installed file was changed:", err)
	}
	activation, err := agent.skillActivation("review", "model")
	if err != nil || activation.Content != content {
		t.Fatal("failed duplicate installation changed the registry:", err)
	}
}

func TestInstallSkillRejectsInvalidPackagesAndRollsBack(t *testing.T) {
	for _, scenario := range []string{"missing", "invalid", "link", "oversized", "nested-destination", "cancel", "record"} {
		t.Run(scenario, func(t *testing.T) {
			sources := t.TempDir()
			writeTestSkill(t, sources, "review", "Original instructions.")
			sourceDir := filepath.Join(sources, "review")
			dir := filepath.Join(t.TempDir(), "skills")
			ctx := t.Context()
			var record func(Event) error
			switch scenario {
			case "missing":
				if err := os.Remove(filepath.Join(sourceDir, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(filepath.Join(sourceDir, "SKILL.md"), []byte("---\nname: invalid--name\ndescription: Valid description\n---\nBody"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "link":
				if err := os.Symlink("SKILL.md", filepath.Join(sourceDir, "link")); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				file, err := os.Create(filepath.Join(sourceDir, "huge"))
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(maxSkillBundleBytes + 1); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			case "nested-destination":
				dir = filepath.Join(sourceDir, "destination")
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "record":
				record = func(Event) error { return errors.New("durable write failed") }
			}
			agent := newInstallSkillAgent(t, dir, &fakeProvider{})
			if _, err := agent.installSkillIdle(ctx, sourceDir, nil, record); err == nil {
				t.Fatal("invalid or failed package installation succeeded")
			}
			if len(agent.Skills().Skills) != 0 {
				t.Fatal("failed installation changed the registry")
			}
			entries, err := os.ReadDir(dir)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatal("failed installation left partial files or staging directories")
			}
		})
	}
}

func TestInstallSkillCommandAndModelToolLoop(t *testing.T) {
	sources := t.TempDir()
	content := writeTestSkill(t, sources, "review", "Complete review instructions.")
	arguments, err := json.Marshal(struct {
		SourceDir string `json:"source_dir"`
	}{filepath.Join(sources, "review")})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "install", Name: "install_skill", Arguments: arguments}}},
		{ToolCalls: []ToolCall{{ID: "activate", Name: "read_skill", Arguments: json.RawMessage(`{"name":"review"}`)}}},
		{Content: "installed"},
	}}
	agent := newInstallSkillAgent(t, filepath.Join(t.TempDir(), "skills"), provider)
	line := "/skill install 请安装 $skill:review，来源为本地已准备的技能目录"
	command, err := agent.HandleSkillCommand(t.Context(), line, nil, nil)
	if err != nil || !command.Handled || command.Prompt != line || command.Output != "" {
		t.Fatalf("command=%+v err=%v", command, err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.Run(t.Context(), agent, command.Prompt, nil)
	if err != nil || result.Text != "installed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	first, second := provider.requests[0], provider.requests[1]
	if first.Messages[0].Content != line || !strings.Contains(first.Messages[len(first.Messages)-1].Content, "[Skill installation request]") || len(first.Tools) != 1 || first.Tools[0].Name != "install_skill" {
		t.Fatal("installation task was lost or triggered activation before installation")
	}
	if len(second.Tools) != 3 || !strings.Contains(second.SystemPrompt, "review") || strings.Contains(second.Messages[len(second.Messages)-1].Content, "[Skill installation request]") {
		t.Fatal("next model turn did not use the refreshed catalog")
	}
	if len(agent.ActiveSkills()) != 1 || agent.ActiveSkills()[0].Content != content || agent.ActiveSkills()[0].Source != "model" {
		t.Fatal("installed skill could not be explicitly activated by the model")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	installedEvents := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record sessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Type == string(EventSkillInstalled) {
			installedEvents++
			if record.RunID == "" {
				t.Fatal("tool installation event lost its run ID")
			}
		}
	}
	if installedEvents != 1 {
		t.Fatal("installation event was duplicated or omitted")
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	fresh := newInstallSkillAgent(t, agent.Skills().Directory, &fakeProvider{})
	if err := resumed.Restore(fresh); err != nil || !reflect.DeepEqual(agent.ActiveSkills(), fresh.ActiveSkills()) {
		t.Fatal("installation session cannot be restored:", err)
	}
}

func TestInstallSkillModeDisabledAndBusy(t *testing.T) {
	sources := t.TempDir()
	writeTestSkill(t, sources, "review", "Instructions.")
	sourceDir := filepath.Join(sources, "review")
	agent := newInstallSkillAgent(t, t.TempDir(), &fakeProvider{})
	if _, err := agent.HandleSkillCommand(t.Context(), "/skill install", nil, nil); err == nil {
		t.Fatal("empty installation prompt was accepted")
	}
	if err := agent.SetMode(ModePlan, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.InstallSkill(t.Context(), sourceDir, nil); err == nil || len(agent.activeToolDefinitions()) != 0 {
		t.Fatal("installation writes are allowed during planning:", err)
	}
	if _, err := agent.HandleSkillCommand(t.Context(), "/skill install review", nil, nil); err == nil {
		t.Fatal("installation command bypasses plan mode")
	}
	agent.running = true
	if _, err := agent.InstallSkill(t.Context(), sourceDir, nil); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent installation was accepted:", err)
	}
	disabled, err := New(Config{Provider: &fakeProvider{}, Model: "test", DisableSkills: true, Tools: []Tool{NewInstallSkillTool()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disabled.InstallSkill(t.Context(), sourceDir, nil); err == nil || len(disabled.activeToolDefinitions()) != 0 {
		t.Fatal("disabled installation is available:", err)
	}
	noTool := newSkillAgent(t, t.TempDir(), &fakeProvider{})
	if _, err := noTool.HandleSkillCommand(t.Context(), "/skill install review", nil, nil); err == nil {
		t.Fatal("installation command runs without its required tool")
	}
}

func TestSessionInstallSkillAPIAndNoReinstallationOnResume(t *testing.T) {
	sources := t.TempDir()
	writeTestSkill(t, sources, "review", "Instructions.")
	sourceDir := filepath.Join(sources, "review")
	dir := t.TempDir()
	agent := newInstallSkillAgent(t, dir, &fakeProvider{})
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.InstallSkill(t.Context(), agent, sourceDir, nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "review")); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	fresh := newInstallSkillAgent(t, dir, &fakeProvider{})
	if err := resumed.Restore(fresh); err != nil || len(fresh.Skills().Skills) != 0 || len(fresh.ActiveSkills()) != 0 {
		t.Fatal("resume reinstalled or activated historical packages:", err)
	}
}
