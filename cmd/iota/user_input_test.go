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
	"strings"
	"testing"
	"time"

	"github.com/chzyer/readline"
	iota "github.com/unimpl/Iota"
)

type inputProvider struct {
	requests  []iota.Request
	responses []iota.Response
}

func (p *inputProvider) Stream(_ context.Context, request iota.Request, _ func(iota.Delta)) (iota.Response, error) {
	p.requests = append(p.requests, request)
	if len(p.responses) == 0 {
		return iota.Response{}, fmt.Errorf("unexpected model request")
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}

func cliInputQuestion(id string) iota.UserInputQuestion {
	return iota.UserInputQuestion{ID: id, Question: "数据保存位置？", RecommendedOptionID: "user", Options: []iota.UserInputOption{
		{ID: "project", Label: "项目目录", Description: "随仓库共享"},
		{ID: "user", Label: "用户目录", Description: "独立于工作目录"},
	}}
}

func cliInputCall(t *testing.T, id string, questions ...iota.UserInputQuestion) iota.ToolCall {
	t.Helper()
	args, err := json.Marshal(iota.UserInputRequest{Format: iota.UserInputStructured, Questions: questions})
	if err != nil {
		t.Fatal(err)
	}
	return iota.ToolCall{ID: id, Name: "request_user_input", Arguments: args}
}

func TestInteractiveUserInputRecommendationsCustomAndBatchProgress(t *testing.T) {
	for _, mode := range []iota.Mode{iota.ModePlan, iota.ModeDefault} {
		t.Run(string(mode), func(t *testing.T) {
			provider := &inputProvider{responses: []iota.Response{
				{ToolCalls: []iota.ToolCall{cliInputCall(t, "first", cliInputQuestion("one"), cliInputQuestion("two")), cliInputCall(t, "second", cliInputQuestion("three"))}},
				{Content: "done"},
			}}
			ui := &userInputUI{}
			agent, err := iota.New(iota.Config{Provider: provider, Model: "test", Mode: mode, Tools: []iota.Tool{iota.NewRequestUserInputTool()}, UserInputHandler: ui.handle})
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if _, err := stdin.WriteString("go\n  \n2\n自定义位置\n/exit\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := stdin.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, nil, ui); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			if len(provider.requests) != 2 {
				t.Fatalf("requests=%d stderr=%s", len(provider.requests), &stderr)
			}
			messages := provider.requests[1].Messages
			for _, want := range []string{`"source":"recommended"`, `"option_id":"user"`, `"source":"selected"`, `"option_id":"project"`} {
				if !strings.Contains(messages[2].Content, want) {
					t.Fatalf("missing %s: %s", want, messages[2].Content)
				}
			}
			if !strings.Contains(messages[3].Content, "自定义位置") || !strings.Contains(messages[3].Content, `"source":"custom"`) {
				t.Fatalf("result=%s", messages[3].Content)
			}
			for _, want := range []string{"Input 1/2 · Question 2/2", "Input 2/2 · Question 1/1", "Assistant question", "1. 用户目录 (recommended)", "Enter: use 用户目录"} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("missing %s: %s", want, &stderr)
				}
			}
		})
	}
}

func TestInteractiveUserInputCancellationReturnsToPrompt(t *testing.T) {
	for _, key := range []string{"\x1b", "\x03"} {
		t.Run(fmt.Sprintf("key=%x", key), func(t *testing.T) {
			provider := &inputProvider{responses: []iota.Response{
				{ToolCalls: []iota.ToolCall{cliInputCall(t, "first", cliInputQuestion("one"), cliInputQuestion("two")), cliInputCall(t, "second", cliInputQuestion("three"))}},
				{Content: "next run"},
			}}
			ui := &userInputUI{}
			agent, err := iota.New(iota.Config{Provider: provider, Model: "test", Tools: []iota.Tool{iota.NewRequestUserInputTool()}, UserInputHandler: ui.handle})
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if _, err := stdin.WriteString("go\n\n" + key + "continue\n/exit\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := stdin.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, nil, ui); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			if len(provider.requests) != 2 || !strings.Contains(stderr.String(), "iota: canceled") || strings.Contains(stderr.String(), "Input 2/2") {
				t.Fatalf("requests=%d stderr=%s", len(provider.requests), &stderr)
			}
			messages := agent.Messages()
			if len(messages) != 6 || messages[4].Content != "continue" || !strings.Contains(messages[2].Content, `"source":"recommended"`) || !strings.Contains(messages[2].Content, `"source":"cancelled"`) {
				t.Fatalf("messages=%+v", messages)
			}
		})
	}
}

func TestInteractiveMixedInputFreeformEmptyMeansUnanswered(t *testing.T) {
	for _, mode := range []iota.Mode{iota.ModePlan, iota.ModeDefault} {
		t.Run(string(mode), func(t *testing.T) {
			freeform := iota.ToolCall{ID: "game", Name: "request_user_input", Arguments: json.RawMessage(`{"format":"freeform","questions":[{"id":"superpower","question":"你想拥有什么超能力？"},{"id":"color","question":"你喜欢什么颜色？"}]}`)}
			provider := &inputProvider{responses: []iota.Response{{ToolCalls: []iota.ToolCall{cliInputCall(t, "storage", cliInputQuestion("storage")), freeform}}, {Content: "done"}}}
			ui := &userInputUI{}
			agent, err := iota.New(iota.Config{Provider: provider, Model: "test", Mode: mode, Tools: []iota.Tool{iota.NewRequestUserInputTool()}, UserInputHandler: ui.handle})
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if _, err := stdin.WriteString("go\n\n  \n 2 \n/exit\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := stdin.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, nil, ui); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			if len(provider.requests) != 2 {
				t.Fatalf("requests=%d", len(provider.requests))
			}
			messages := provider.requests[1].Messages
			var response iota.UserInputResponse
			if err := json.Unmarshal([]byte(messages[3].Content), &response); err != nil {
				t.Fatal(err)
			}
			if response.Cancelled || response.Answers["superpower"].Source != "unanswered" || response.Answers["superpower"].Value != "" || response.Answers["color"].Source != "custom" || response.Answers["color"].Value != " 2 " {
				t.Fatalf("response=%+v", response)
			}
			if !strings.Contains(messages[2].Content, `"source":"recommended"`) {
				t.Fatal("structured default changed")
			}
			freeformUI := strings.SplitN(stderr.String(), "你想拥有什么超能力？", 2)[1]
			for _, want := range []string{"Empty Enter means no answer provided", "No answer provided.", "Input 2/2 · Question 2/2"} {
				if !strings.Contains(freeformUI, want) {
					t.Fatalf("missing %s: %s", want, freeformUI)
				}
			}
			if strings.Contains(freeformUI, "recommended") || strings.Contains(stderr.String(), "recommended_option_id") || strings.Contains(stderr.String(), "[request_user_input]") {
				t.Fatalf("protocol or choices leaked into freeform UI: %s", &stderr)
			}
		})
	}
}

func TestNonInteractiveCLIHasNoUserInputToolOrQuestions(t *testing.T) {
	for _, mode := range []string{"plan", "default"} {
		for _, promptFlag := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%s/prompt=%t", mode, promptFlag), func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Messages []struct{ Content string }
						Tools    []struct{ Function struct{ Name string } }
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					for _, tool := range request.Tools {
						if tool.Function.Name == "request_user_input" {
							t.Error("input declared in non-interactive run")
						}
					}
					if !strings.Contains(request.Messages[0].Content, "Do not call request_user_input or ask questions") {
						t.Error("missing non-interactive instruction")
					}
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}))
				defer server.Close()
				args := []string{"--model", "test", "--base-url", server.URL, "--cwd", t.TempDir(), "--mode", mode, "--tools", "request_user_input,read", "--no-session"}
				var stdin *os.File
				var err error
				if promptFlag {
					stdin, err = os.Open(os.DevNull)
					args = append(args, "-p", "go")
				} else {
					stdin, err = os.CreateTemp(t.TempDir(), "input")
					if err == nil {
						_, err = stdin.WriteString("go")
						if err == nil {
							_, err = stdin.Seek(0, 0)
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				defer stdin.Close()
				var stdout, stderr bytes.Buffer
				if code := run(args, stdin, &stdout, &stderr); code != 0 {
					t.Fatalf("code=%d stderr=%s", code, &stderr)
				}
			})
		}
	}
}

func TestQuestionInputLoneEscapeAndContextInterrupt(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		reader, writer := io.Pipe()
		input := newQuestionInput(reader)
		input.asking.Store(true)
		done := make(chan byte, 1)
		go func() { var data [1]byte; _, _ = input.Read(data[:]); done <- data[0] }()
		if interrupt {
			input.interrupt()
		} else {
			if _, err := writer.Write([]byte{readline.CharEsc}); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case key := <-done:
			if key != readline.CharInterrupt {
				t.Fatalf("key=%d", key)
			}
		case <-time.After(time.Second):
			t.Fatal("input did not wake")
		}
		input.Close()
		writer.Close()
		reader.Close()
	}
	input := newQuestionInput(strings.NewReader("\x1b[D"))
	defer input.Close()
	input.asking.Store(true)
	data := make([]byte, 3)
	if _, err := io.ReadFull(input, data); err != nil || string(data) != "\x1b[D" {
		t.Fatalf("cursor sequence=%q err=%v", data, err)
	}
}
