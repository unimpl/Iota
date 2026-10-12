package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iota "github.com/unimpl/Iota"
)

type skillTestProvider struct{}

func (skillTestProvider) Stream(context.Context, iota.Request, func(iota.Delta)) (iota.Response, error) {
	return iota.Response{}, fmt.Errorf("skill commands must not call the model")
}

func TestInteractiveSkillReloadUsesSDKCommand(t *testing.T) {
	dir := t.TempDir()
	agent, err := iota.New(iota.Config{Provider: skillTestProvider{}, Model: "test", SkillsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	writePromptFile(t, filepath.Join(dir, "review", "SKILL.md"), "---\nname: review\ndescription: Review code.\n---\nFollow review steps.")
	stdin, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := io.WriteString(stdin, "/skill reload\n/skill list\n/skill invalid\n/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, nil, nil); code != 0 || len(agent.Skills().Skills) != 1 || !strings.Contains(stderr.String(), "review: Review code.") || !strings.Contains(stderr.String(), "usage: /skill") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if command, err := agent.HandleSkillCommand(t.Context(), "/skillful", nil, nil); command.Handled || err != nil {
		t.Fatal("command grammar consumed another command")
	}
}

func TestCLISkillMentionAndDefaultModelTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	content := "---\nname: review\ndescription: Review code.\n---\nFollow these exact review steps."
	writePromptFile(t, filepath.Join(home, ".iota", "skills", "review", "SKILL.md"), content)
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		requests <- string(data)
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"reviewed\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--model", "test", "--base-url", server.URL, "--cwd", t.TempDir(), "--no-session", "-p", "$skill:review inspect the code"}, stdin, &stdout, &stderr); code != 0 || stdout.String() != "reviewed\n" || !strings.Contains(stderr.String(), "skill: review (user)") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var request struct {
		Messages []iota.Message `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(<-requests), &request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.Messages[2].Content, content) {
		t.Fatal("CLI did not deliver SDK skill instructions")
	}
	names := make(map[string]bool)
	for _, tool := range request.Tools {
		names[tool.Function.Name] = true
	}
	if !names["read_skill"] || !names["read_skill_resource"] {
		t.Fatal("CLI default skill tools are missing")
	}
}

type installSkillCLIProvider struct {
	sourceDir string
	requests  []iota.Request
}

func (p *installSkillCLIProvider) Stream(_ context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
	p.requests = append(p.requests, request)
	if len(p.requests) == 1 {
		arguments, err := json.Marshal(struct {
			SourceDir string `json:"source_dir"`
		}{p.sourceDir})
		return iota.Response{ToolCalls: []iota.ToolCall{{ID: "install", Name: "install_skill", Arguments: arguments}}}, err
	}
	emit(iota.Delta{Text: "installed"})
	return iota.Response{Content: "installed", StopReason: "stop"}, nil
}

func TestInteractiveSkillInstallRunsPromptAndRefreshesTools(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "downloaded-skill")
	writePromptFile(t, filepath.Join(sourceDir, "SKILL.md"), "---\nname: review\ndescription: Review code.\n---\nFollow review steps.")
	provider := &installSkillCLIProvider{sourceDir: sourceDir}
	tools, err := createTools(t.TempDir(), defaultTools, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test", SkillsDir: filepath.Join(t.TempDir(), "skills"), PlansDir: t.TempDir(), Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := io.WriteString(stdin, "/skill install 安装一个代码审查 skill\n/skill list\n/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, nil, nil); code != 0 || stdout.String() != "installed\n" || !strings.Contains(stderr.String(), "skill installed: review") || !strings.Contains(stderr.String(), "review: Review code.") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(provider.requests) != 2 || provider.requests[0].Messages[0].Content != "/skill install 安装一个代码审查 skill" || !strings.Contains(provider.requests[0].SystemPrompt, "install_skill") || !strings.Contains(provider.requests[1].SystemPrompt, "Review code.") {
		t.Fatal("CLI did not pass the SDK installation task or refresh its request")
	}
	first, second := make(map[string]bool), make(map[string]bool)
	for _, tool := range provider.requests[0].Tools {
		first[tool.Name] = true
	}
	for _, tool := range provider.requests[1].Tools {
		second[tool.Name] = true
	}
	if !first["install_skill"] || first["read_skill"] || !second["install_skill"] || !second["read_skill"] || !second["read_skill_resource"] {
		t.Fatal("initial installation or newly available skill tools were omitted")
	}
}
