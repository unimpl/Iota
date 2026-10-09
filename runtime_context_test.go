package iota

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func requestCollaboration(t *testing.T, request Request) CollaborationState {
	t.Helper()
	tail := request.Messages[len(request.Messages)-1]
	if !tail.RuntimeContext {
		t.Fatal("runtime suffix missing")
	}
	parts := strings.SplitN(tail.Content, "<runtime-context>\n", 2)
	if len(parts) != 2 {
		t.Fatal("runtime state missing")
	}
	text := strings.SplitN(parts[1], "\n</runtime-context>", 2)[0]
	var state CollaborationState
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRuntimeSuffixPreservesPrefixAndResumesProgress(t *testing.T) {
	provider := &compactionProvider{respond: func(_ context.Context, request Request, call int) (Response, error) {
		if call == 1 {
			return Response{StopReason: "tool_calls", ToolCalls: []ToolCall{{ID: "progress", Name: "update_progress", Arguments: json.RawMessage(`{"plan":[{"step":"Implement cache","status":"in_progress"},{"step":"Verify","status":"pending"}]}`)}}}, nil
		}
		return Response{}, context.Canceled
	}}
	agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{NewUpdateProgressTool()}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Run(t.Context(), agent, "Implement a cache and verify it", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	first, second := provider.requests[0], provider.requests[1]
	if first.SystemPrompt != second.SystemPrompt || !reflect.DeepEqual(first.Tools, second.Tools) || !reflect.DeepEqual(first.Messages[:len(first.Messages)-1], second.Messages[:len(first.Messages)-1]) {
		t.Fatal("progress update changed the reusable request prefix")
	}
	if !strings.Contains(first.Messages[len(first.Messages)-1].Content, "[Run entry]") || strings.Contains(second.Messages[len(second.Messages)-1].Content, "[Run entry]") {
		t.Fatal("task decomposition was not limited to the first model turn")
	}
	state := agent.Collaboration()
	if state.Progress == nil || state.Progress.ID == "" || state.Progress.Version != 1 || state.Progress.Plan[0].ID == "" {
		t.Fatal("progress IDs were not generated")
	}
	for _, message := range agent.Messages() {
		if message.RuntimeContext {
			t.Fatal("runtime suffix leaked into canonical history")
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	nextProvider := &compactionProvider{respond: func(_ context.Context, request Request, call int) (Response, error) {
		if call == 1 {
			progress := *state.Progress
			progress.Plan = append([]PlanStep(nil), progress.Plan...)
			progress.Version = 0 // version is assigned by the program, not the model.
			progress.Plan[0].Status, progress.Plan[1].Status = "completed", "in_progress"
			arguments, _ := json.Marshal(struct {
				ID   string     `json:"id"`
				Plan []PlanStep `json:"plan"`
			}{progress.ID, progress.Plan})
			return Response{StopReason: "tool_calls", ToolCalls: []ToolCall{{ID: "resume", Name: "update_progress", Arguments: arguments}}}, nil
		}
		return Response{Content: "continued", StopReason: "stop"}, nil
	}}
	next, err := New(Config{Provider: nextProvider, Model: "test", Tools: []Tool{NewUpdateProgressTool()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Restore(next); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.Collaboration(), state) {
		t.Fatal("progress changed during resume")
	}
	if _, err := resumed.Run(t.Context(), next, "go on", nil); err != nil {
		t.Fatal(err)
	}
	if nextProvider.requests[0].Messages[len(nextProvider.requests[0].Messages)-2].Source.StepID != "" {
		t.Fatal("new user input was assigned to a step before intent interpretation")
	}
	progress := next.Collaboration().Progress
	if progress.ID != state.Progress.ID || progress.Version != 2 || progress.Plan[0].ID != state.Progress.Plan[0].ID || progress.Plan[1].ID != state.Progress.Plan[1].ID {
		t.Fatal("continuation replaced stable IDs or lost the version")
	}
	tail := nextProvider.requests[0].Messages[len(nextProvider.requests[0].Messages)-1]
	if !tail.RuntimeContext || !strings.Contains(tail.Content, state.Progress.ID) || !strings.Contains(tail.Content, "[Run entry]") {
		t.Fatal("resume did not supply current progress at run entry")
	}
}

func TestProgressRejectsForeignIDsAndDuplicateIDs(t *testing.T) {
	previous := &PlanProgress{ID: "checklist", Version: 3, Plan: []PlanStep{{ID: "one", Step: "one", Status: "pending"}}}
	for _, progress := range []PlanProgress{
		{ID: "unknown", Plan: []PlanStep{{Step: "one", Status: "pending"}}},
		{ID: "checklist", Plan: []PlanStep{{ID: "unknown", Step: "one", Status: "pending"}}},
		{ID: "checklist", Plan: []PlanStep{{ID: "one", Step: "one", Status: "pending"}, {ID: "one", Step: "two", Status: "pending"}}},
	} {
		if err := prepareProgress(&progress, previous); err == nil {
			t.Fatal("invalid progress IDs accepted")
		}
	}
}

func TestApprovedPlanVersionSurvivesResumeAndReset(t *testing.T) {
	plans := filepath.Join(t.TempDir(), "plans")
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "save", Name: "save_plan", Arguments: json.RawMessage(`{"content":"# Plan\nImplement and verify."}`)}}},
		{Content: "ready"},
	}}
	agent, err := New(Config{Provider: provider, Model: "test", Mode: ModePlan, PlansDir: plans, Tools: []Tool{NewSavePlanTool()}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Run(t.Context(), agent, "plan", nil); err != nil {
		t.Fatal(err)
	}
	if agent.Collaboration().Approval != nil {
		t.Fatal("saving a plan approved it")
	}
	if _, err := session.ApprovePlan(agent, nil); err != nil {
		t.Fatal(err)
	}
	want := agent.Collaboration()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	next, _ := New(Config{Provider: &compactionProvider{}, Model: "test", PlansDir: plans})
	if err := resumed.Restore(next); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.Collaboration().Approval, want.Approval) {
		t.Fatal("execution authorization was lost")
	}
	bad := cloneCollaboration(want)
	bad.Plan.Content = "# Changed\nDifferent work."
	if err := validateCollaboration(bad); err == nil {
		t.Fatal("approval accepted a different plan version")
	}
	if err := resumed.Reset(next); err != nil {
		t.Fatal(err)
	}
	if next.Collaboration().Approval != nil {
		t.Fatal("reset retained execution authorization")
	}
}
