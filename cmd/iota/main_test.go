package main

import (
	"bytes"
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
	"github.com/unimpl/Iota/provider/openaicompat"
)

func TestParseOptions(t *testing.T) {
	var stderr bytes.Buffer
	opts, err := parseOptionsWithConfig(
		[]string{"--model", "model", "--max-turns", "3", "--timeout", "5s", "--tools", "none"},
		&stderr,
		fileConfig{},
		func(string) (string, bool) { return "", false },
	)
	if err != nil {
		t.Fatal(err)
	}
	if opts.model != "model" || opts.maxTurns != 3 || opts.timeout != 5*time.Second || opts.tools != "none" {
		t.Fatalf("options = %+v", opts)
	}
}

func TestLoadSystemPrompt(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("project rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt, err := loadSystemPrompt(cwd, "extra rule")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{defaultSystemPrompt, "extra rule", "project rule"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("prompt does not contain %q: %s", text, prompt)
		}
	}
}

func TestCreateTools(t *testing.T) {
	tools, err := createTools(t.TempDir(), "read,bash,read", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "read" || tools[1].Name != "bash" {
		t.Fatalf("tools = %+v", tools)
	}
	if _, err := createTools(t.TempDir(), "unknown", time.Second); err == nil {
		t.Fatal("expected unknown tool error")
	}
}

func TestFormatRunErrorAddsResetHint(t *testing.T) {
	message := formatRunError(&testError{"maximum context token length exceeded"})
	if !strings.Contains(message, "/reset") {
		t.Fatalf("message = %q", message)
	}
}

func TestRunWithPipedInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	stdin, err := os.CreateTemp(t.TempDir(), "prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := io.WriteString(stdin, "question"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--model", "test", "--base-url", server.URL, "--cwd", t.TempDir(), "--tools", "none"}, stdin, &stdout, &stderr)
	if code != 0 || stdout.String() != "answer\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsPromptWithPipedInput(t *testing.T) {
	stdin, err := os.CreateTemp(t.TempDir(), "prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--model", "test", "-p", "question"}, stdin, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "cannot be combined") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunWithPromptFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--model", "test", "--base-url", server.URL, "--cwd", t.TempDir(), "--tools", "none", "-p", "question"}, stdin, &stdout, &stderr)
	if code != 0 || stdout.String() != "answer\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestInteractiveResetAndExit(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	provider, err := openaicompat.New(openaicompat.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := io.WriteString(stdin, "/reset\n/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "conversation reset") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }
