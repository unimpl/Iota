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
	"time"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

func TestPlanningOptionsAndCustomAnswerUseLoggedConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	question := "数据保存位置？\nA. 项目目录（推荐）\nB. 用户目录\nC. 自定义输入：直接描述方案。"
	answer := "自定义：保存在项目的 data 目录，不使用推荐路径。"
	plan := "# 数据保存计划\n\n## 关键决策\n使用用户指定的项目 data 目录。\n\n## 实施步骤\n1. 保存数据。\n\n## 验证方案\n测试保存和恢复。"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests++
		if !strings.Contains(body.Messages[0].Content, "自定义输入") {
			t.Error("question guidance missing")
		}
		if requests == 2 {
			if body.Messages[len(body.Messages)-2].Content != answer {
				t.Error("custom answer was not sent verbatim")
			}
			arguments, err := json.Marshal(struct {
				Content string `json:"content"`
			}{plan})
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
		text := question
		if requests == 3 {
			text = "计划已保存，请查看。"
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", encoded)
	}))
	defer server.Close()
	provider, err := openaicompat.New(openaicompat.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := createTools(cwd, defaultTools, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	plansDir := filepath.Join(home, ".iota", "plans")
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test", Tools: tools, PlansDir: plansDir, PlanTemplatePath: filepath.Join(plansDir, "template.md")})
	if err != nil {
		t.Fatal(err)
	}
	session, err := iota.NewSession(t.TempDir(), iota.SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stdin, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := stdin.WriteString("/plan 设计数据保存功能\n" + answer + "\n/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := interactive(agent, make(chan os.Signal), stdin, &stdout, &stderr, session, nil); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	if requests != 3 || agent.Mode() != iota.ModePlan || agent.Collaboration().Plan == nil || agent.Collaboration().Plan.Content != plan {
		t.Fatalf("requests=%d state=%+v", requests, agent.Collaboration())
	}
	if !strings.Contains(stdout.String(), question) {
		t.Fatal("options were not displayed")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	var foundQuestion, foundAnswer bool
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
		message := record.Payload.Message
		if record.Type != "message_added" || message == nil {
			continue
		}
		foundQuestion = foundQuestion || message.Role == iota.RoleAssistant && message.Content == question
		foundAnswer = foundAnswer || message.Role == iota.RoleUser && message.Content == answer
	}
	if !foundQuestion || !foundAnswer {
		t.Fatal("question or custom answer missing from session")
	}
}
