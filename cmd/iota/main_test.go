package main

import (
	"bytes"
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
	"github.com/unimpl/Iota/provider/openaicompat"
)

// TestParseOptions 验证主要命令行参数的解析与类型转换。
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

// TestLoadSystemPrompt 验证内置、附加和项目规则都会进入系统提示。
func TestLoadSystemPrompt(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
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

// TestLoadSystemPromptFiles 验证两类提示文件分别按项目优先查找，并保持追加顺序。
func TestLoadSystemPromptFiles(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	projectSystem := filepath.Join(cwd, ".iota", "SYSTEM.md")
	projectAppend := filepath.Join(cwd, ".iota", "APPEND_SYSTEM.md")
	writePromptFile(t, filepath.Join(home, ".iota", "SYSTEM.md"), "home base")
	writePromptFile(t, filepath.Join(home, ".iota", "APPEND_SYSTEM.md"), "home append")
	writePromptFile(t, projectSystem, "project base")
	writePromptFile(t, projectAppend, "project append")
	writePromptFile(t, filepath.Join(cwd, "AGENTS.md"), "project rule")

	for _, step := range []struct {
		remove string
		want   string
	}{
		{want: "project base\n\nflag extra\n\nproject append\n\nProject instructions from AGENTS.md:\nproject rule"},
		{remove: projectAppend, want: "project base\n\nflag extra\n\nhome append\n\nProject instructions from AGENTS.md:\nproject rule"},
		{remove: projectSystem, want: "home base\n\nflag extra\n\nhome append\n\nProject instructions from AGENTS.md:\nproject rule"},
	} {
		if step.remove != "" {
			if err := os.Remove(step.remove); err != nil {
				t.Fatal(err)
			}
		}
		got, err := loadSystemPromptFrom(cwd, home, "flag extra")
		if err != nil || got != step.want {
			t.Fatalf("prompt=%q err=%v, want %q", got, err, step.want)
		}
	}
}

// TestLoadSystemPromptEmptyFileAndReadError 验证空替换文件回到默认值，读取失败会中止启动。
func TestLoadSystemPromptEmptyFileAndReadError(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	projectSystem := filepath.Join(cwd, ".iota", "SYSTEM.md")
	writePromptFile(t, filepath.Join(home, ".iota", "SYSTEM.md"), "home base")
	writePromptFile(t, projectSystem, "")
	got, err := loadSystemPromptFrom(cwd, home, "")
	if err != nil || got != defaultSystemPrompt {
		t.Fatalf("prompt=%q err=%v", got, err)
	}
	if err := os.Remove(projectSystem); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(projectSystem, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSystemPromptFrom(cwd, home, ""); err == nil || !strings.Contains(err.Error(), projectSystem) {
		t.Fatalf("got %v, want read error for %s", err, projectSystem)
	}
}

// writePromptFile 创建测试提示及父目录，确保路径查找不依赖开发机配置。
func writePromptFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCreateTools 验证工具顺序、去重和未知名称报错。
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

// TestFormatRunErrorAddsResetHint 确认上下文容量错误会提示重置操作。
func TestFormatRunErrorAddsResetHint(t *testing.T) {
	message := formatRunError(&testError{"maximum context token length exceeded"})
	if !strings.Contains(message, "/reset") {
		t.Fatalf("message = %q", message)
	}
}

// TestRunWithPipedInput 验证管道输入可触发单次运行并输出模型文本。
func TestRunWithPipedInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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

// TestRunOptionalSession 验证 CLI 可关闭保存，或从 SDK 会话恢复后继续追加。
func TestRunOptionalSession(t *testing.T) {
	for _, mode := range []string{"disabled", "path", "uuid", "latest", "empty"} {
		t.Run(mode, func(t *testing.T) {
			resume := mode != "disabled"
			home := t.TempDir()
			t.Setenv("HOME", home)
			var sessionPath string
			if resume {
				dir := filepath.Join(home, ".iota", "sessions")
				if mode == "path" {
					dir = t.TempDir()
				}
				session, err := iota.NewSession(dir, iota.SessionInfo{Model: "test"})
				if err != nil {
					t.Fatal(err)
				}
				firstServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"first answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}))
				defer firstServer.Close()
				provider, err := openaicompat.New(openaicompat.Config{BaseURL: firstServer.URL})
				if err != nil {
					t.Fatal(err)
				}
				agent, err := iota.New(iota.Config{Provider: provider, Model: "test"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := session.Run(t.Context(), agent, "first question", nil); err != nil {
					t.Fatal(err)
				}
				sessionPath = session.Path()
				if err := session.Close(); err != nil {
					t.Fatal(err)
				}
			}
			requests := make(chan []iota.Message, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body struct {
					Messages []iota.Message `json:"messages"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body.Messages
				fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			stdin, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			args := []string{"--model", "test", "--base-url", server.URL, "--cwd", t.TempDir(), "--tools", "none", "-p", "next question"}
			if resume {
				switch mode {
				case "path":
					args = append(args, "--resume", sessionPath)
				case "uuid":
					name := filepath.Base(sessionPath)
					args = append(args, "--resume", strings.TrimSuffix(name[11:], ".jsonl"))
				case "latest":
					args = append(args, "--resume=")
				case "empty":
					args = append(args, "--resume", "")
				}
			} else {
				args = append(args, "--no-session")
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, stdin, &stdout, &stderr); code != 0 || stdout.String() != "answer\n" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			messages := <-requests
			if resume {
				if !strings.HasSuffix(stderr.String(), "To resume this conversation, run: iota --resume '"+sessionPath+"'\n") {
					t.Fatalf("missing exit hint: %q", stderr.String())
				}
				if len(messages) != 4 || messages[1].Content != "first question" || messages[2].Content != "first answer" || messages[3].Content != "next question" {
					t.Fatalf("messages=%+v", messages)
				}
			} else {
				if strings.Contains(stderr.String(), "session:") || strings.Contains(stderr.String(), "To resume") {
					t.Fatalf("stderr=%q", stderr.String())
				}
				if _, err := os.Stat(filepath.Join(home, ".iota", "sessions")); !os.IsNotExist(err) {
					t.Fatalf("disabled session created directory: %v", err)
				}
			}
		})
	}
}

// TestRunRejectsPromptWithPipedInput 防止同时从参数和管道获取提示而产生歧义。
func TestRunRejectsPromptWithPipedInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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

// TestRunWithPromptFlag 验证单次运行会记录原始请求、流事件及会话结束事件。
func TestRunWithPromptFlag(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home with ' quote")
	t.Setenv("HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"thinking\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
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
	entries, err := os.ReadDir(filepath.Join(home, ".iota", "sessions"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("session entries=%v err=%v", entries, err)
	}
	path := filepath.Join(home, ".iota", "sessions", entries[0].Name())
	if !strings.HasSuffix(stderr.String(), "To resume this conversation, run: iota --resume '"+strings.ReplaceAll(path, "'", "'\"'\"'")+"'\n") {
		t.Fatalf("missing quoted exit hint: %q", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(home, ".iota", "sessions", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"session_start", "run_start", "message_added", "model_request", "reasoning_delta", "text_delta", "model_stream_finish", "model_stream_done", "model_response", "run_end", "session_end"} {
		if !strings.Contains(string(data), `"type":"`+kind+`"`) {
			t.Fatalf("session is missing %s: %s", kind, data)
		}
	}
	var requestBody string
	var textChunks, reasoningChunks, finishChunks, doneChunks, otherChunks int
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				RawRequest string `json:"raw_request"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Type == "model_request_body" {
			t.Fatal("raw request was recorded as a separate event")
		}
		if record.Type == "model_request" {
			requestBody = record.Payload.RawRequest
		}
		if record.Type == "text_delta" {
			textChunks++
		}
		if record.Type == "reasoning_delta" {
			reasoningChunks++
		}
		if record.Type == "model_stream_other" {
			otherChunks++
		}
		if record.Type == "model_stream_finish" {
			finishChunks++
		}
		if record.Type == "model_stream_done" {
			doneChunks++
		}
	}
	if textChunks != 1 || reasoningChunks != 1 || finishChunks != 1 || doneChunks != 1 || otherChunks != 0 || !strings.Contains(requestBody, `"stream":true`) ||
		!strings.Contains(string(data), `"raw_chunk":"data: {`) ||
		!strings.Contains(string(data), `"type":"model_stream_done"`) {
		t.Fatalf("session is missing raw model traffic: %s", data)
	}
}

// TestExecuteReasoning 验证推理片段连续显示，并在正文或错误前关闭标识。
func TestExecuteReasoning(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先\"}}]}\n\n")
				fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"思考\"}}]}\n\n")
				if !failed {
					fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"回答\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			provider, err := openaicompat.New(openaicompat.Config{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := iota.New(iota.Config{Provider: provider, Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := execute(agent, "question", make(chan os.Signal), &stdout, &stderr, false, nil)
			wantCode, wantOutput := 0, "回答\n"
			wantReasoning := "[thinking]\n先思考\n[/thinking]\n"
			if failed {
				wantCode, wantOutput = 1, "\n"
				wantReasoning += "iota: unexpected EOF\n"
			}
			if code != wantCode || stdout.String() != wantOutput || stderr.String() != wantReasoning {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

// TestInteractiveResetAndExit 验证交互命令能清空历史并正常退出。
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
	if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, nil); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "conversation reset") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

// testError 为错误格式测试提供可控的消息文本。
type testError struct{ message string }

// Error 实现 error 接口，以便传入 formatRunError。
func (e *testError) Error() string { return e.message }
