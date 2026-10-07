package iota

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeavingPlanModePreservesHistoryAndRestoresDefaultWorkflow(t *testing.T) {
	writes := 0
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "save", Name: "save_plan", Arguments: json.RawMessage(`{"content":"# Saved plan\nChange the source and run tests."}`)}}},
		{Content: "The plan is ready and waiting for approval. Use /execute."},
		{Content: "Hello."},
		{ToolCalls: []ToolCall{{ID: "write", Name: "write", Arguments: json.RawMessage(`{}`)}}},
		{Content: "Implemented."},
	}}
	agent, err := New(Config{Provider: provider, Model: "test", PlansDir: filepath.Join(t.TempDir(), "plans"), Tools: []Tool{
		NewSavePlanTool(),
		{Name: "write", Schema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { writes++; return "written", nil }},
	}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.SetMode(agent, ModePlan, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), agent, "plan this change", nil); err != nil {
		t.Fatal(err)
	}
	plan := *agent.Collaboration().Plan
	history := agent.Messages()
	if err := session.SetMode(agent, ModeDefault, nil); err != nil {
		t.Fatal(err)
	}
	if len(agent.Messages()) != len(history) || *agent.Collaboration().Plan != plan {
		t.Fatal("mode switch cleared the saved plan or history")
	}
	if _, err := session.Run(t.Context(), agent, "hi", nil); err != nil {
		t.Fatal(err)
	}
	request := provider.requests[2]
	if request.Mode != ModeDefault || request.Messages[len(request.Messages)-1].Content != "hi" || writes != 0 {
		t.Fatalf("request=%+v writes=%d", request, writes)
	}
	if !strings.Contains(request.Messages[len(history)-1].Content, "waiting for approval") {
		t.Fatal("original planning history was removed")
	}
	for _, instruction := range []string{"[Collaboration mode: default]", "Earlier planning-only restrictions", "Respond to the user's latest message", "does not approve, cancel, or start", "do not suggest /execute alone", "Saved plan file (reference only): " + plan.Path} {
		if !strings.Contains(request.SystemPrompt, instruction) {
			t.Fatalf("missing current-mode guidance %q", instruction)
		}
	}
	if strings.Contains(request.SystemPrompt, "# Iota 规划模式") {
		t.Fatal("planning template leaked into default mode")
	}
	if len(request.Tools) != 1 || request.Tools[0].Name != "write" {
		t.Fatalf("default tools=%+v", request.Tools)
	}
	if _, err := session.Run(t.Context(), agent, "implement the requested change now", nil); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("default mode still blocked implementation: writes=%d", writes)
	}
}
