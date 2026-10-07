package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

func TestEnteringPlanModeInitializesUserTemplateWithoutModelCall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plansDir := filepath.Join(home, ".iota", "plans")
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("entering plan mode called the model") }))
	defer server.Close()
	provider, err := openaicompat.New(openaicompat.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test", PlansDir: plansDir, PlanTemplatePath: filepath.Join(plansDir, "template.md")})
	if err != nil {
		t.Fatal(err)
	}
	session, err := iota.NewSession(filepath.Join(home, ".iota", "sessions"), iota.SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	handled, prompt, err := planningCommand("/plan", agent, session, nil, &bytes.Buffer{})
	if err != nil || !handled || prompt != "" || agent.Mode() != iota.ModePlan {
		t.Fatalf("handled=%t prompt=%q err=%v", handled, prompt, err)
	}
	data, err := os.ReadFile(filepath.Join(plansDir, "template.md"))
	if err != nil || !strings.Contains(string(data), "自定义输入") {
		t.Fatalf("template missing: %v", err)
	}
	custom := "# User template\nPreserve the user's custom instructions."
	if err := os.WriteFile(filepath.Join(plansDir, "template.md"), []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := planningCommand("/default", agent, session, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := planningCommand("/plan", agent, session, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(plansDir, "template.md"))
	if err != nil || string(data) != custom {
		t.Fatal("entering plan mode overwrote the user template")
	}
}

func TestCLIUserPlanSurvivesWorkingDirectoryChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("IOTA_MODE", "")
	if err := os.Unsetenv("IOTA_MODE"); err != nil {
		t.Fatal(err)
	}
	plansDir := filepath.Join(home, ".iota", "plans")
	userTemplate := "# User planning guide\nuser-template-marker: offer options and custom answers."
	writePromptFile(t, filepath.Join(plansDir, "template.md"), userTemplate)
	firstCWD, nextCWD := t.TempDir(), t.TempDir()
	writePromptFile(t, filepath.Join(firstCWD, "iota", "plans", "template.md"), "# Project template\nproject-template-marker")
	content := "# User plan\nImplement and validate the requested change."
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests++
		if !strings.Contains(body.Messages[0].Content, "user-template-marker") || strings.Contains(body.Messages[0].Content, "project-template-marker") {
			t.Error("CLI used a project template instead of the user template")
		}
		if requests == 1 {
			arguments, err := json.Marshal(struct {
				Content string `json:"content"`
			}{content})
			if err != nil {
				t.Error(err)
				return
			}
			encoded, err := json.Marshal(string(arguments))
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"save\",\"type\":\"function\",\"function\":{\"name\":\"save_plan\",\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", encoded)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ready\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--model", "test", "--base-url", server.URL, "--cwd", firstCWD, "--mode", "plan", "-p", "make a plan"}, stdin, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	files, err := os.ReadDir(filepath.Join(home, ".iota", "sessions"))
	if err != nil || len(files) != 1 {
		t.Fatalf("sessions=%v err=%v", files, err)
	}
	sessionPath := filepath.Join(home, ".iota", "sessions", files[0].Name())
	log, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved *iota.SavedPlan
	for _, line := range bytes.Split(log, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record struct {
			Type    string     `json:"type"`
			Payload iota.Event `json:"payload"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Type == "plan_saved" {
			saved = record.Payload.Collaboration.Plan
		}
	}
	if saved == nil || filepath.Dir(saved.Path) != plansDir {
		t.Fatalf("plan saved outside user directory: %+v", saved)
	}
	data, err := os.ReadFile(saved.Path)
	if err != nil || string(data) != content {
		t.Fatalf("plan content=%q err=%v", data, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--model", "test", "--base-url", server.URL, "--cwd", nextCWD, "--resume", sessionPath, "-p", "continue planning"}, stdin, &stdout, &stderr); code != 0 {
		t.Fatalf("resume code=%d stderr=%s", code, &stderr)
	}
	if requests != 3 {
		t.Fatalf("requests=%d", requests)
	}
	for _, path := range []string{filepath.Join(firstCWD, ".iota", "plans"), filepath.Join(nextCWD, ".iota", "plans"), filepath.Join(nextCWD, "iota", "plans")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected project plan directory %s: %v", path, err)
		}
	}
}
