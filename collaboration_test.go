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

func TestPlanWorkflowPersistsAndRestores(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".iota", "plans")
	content := "# Cache fix\n\nInspect the cache and add a regression test."
	arguments, err := json.Marshal(struct {
		Content string `json:"content"`
	}{content})
	if err != nil {
		t.Fatal(err)
	}
	progress := json.RawMessage(`{"explanation":"Ready to implement","plan":[{"step":"Fix cache","status":"in_progress"},{"step":"Run tests","status":"pending"}]}`)
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{
			{ID: "write", Name: "write", Arguments: json.RawMessage(`{}`)},
			{ID: "checklist-in-plan", Name: "update_plan", Arguments: progress},
			{ID: "save", Name: "save_plan", Arguments: arguments},
		}, StopReason: "tool_calls"},
		{Content: "Plan ready for review.", StopReason: "stop"},
		{ToolCalls: []ToolCall{
			{ID: "checklist", Name: "update_plan", Arguments: progress},
			{ID: "save-in-default", Name: "save_plan", Arguments: arguments},
		}, StopReason: "tool_calls"},
		{Content: "Implementation done.", StopReason: "stop"},
	}}
	writes := 0
	tools := []Tool{
		{Name: "read", ReadOnly: true, Schema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { return "code", nil }},
		{Name: "write", Schema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { writes++; return "changed", nil }},
		NewSavePlanTool(), NewUpdatePlanTool(),
	}
	agent, err := New(Config{Provider: provider, Model: "test", SystemPrompt: "project rule", Tools: tools, PlansDir: dir})
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
	var events []Event
	if _, err := session.Run(t.Context(), agent, "Plan the cache fix", func(event Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	if writes != 0 || agent.Mode() != ModePlan {
		t.Fatalf("writes=%d mode=%s", writes, agent.Mode())
	}
	request := provider.requests[0]
	var names []string
	for _, tool := range request.Tools {
		names = append(names, tool.Name)
	}
	if !reflect.DeepEqual(names, []string{"read", "save_plan"}) || request.Mode != ModePlan || !strings.Contains(request.SystemPrompt, "project rule") || !strings.Contains(request.SystemPrompt, "Collaboration mode: plan") {
		t.Fatalf("request=%+v", request)
	}
	messages := agent.Messages()
	if !messages[2].IsError || !messages[3].IsError || messages[4].IsError {
		t.Fatalf("messages=%+v", messages)
	}
	state := agent.Collaboration()
	if state.Plan == nil || state.Plan.Content != content || state.Progress != nil {
		t.Fatalf("state=%+v", state)
	}
	data, err := os.ReadFile(state.Plan.Path)
	if err != nil || string(data) != content {
		t.Fatalf("plan=%q err=%v", data, err)
	}
	info, err := os.Stat(state.Plan.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plan info=%v err=%v", info, err)
	}
	saved := 0
	for _, event := range events {
		if event.Type == EventPlanSaved {
			saved++
		}
	}
	if saved != 1 {
		t.Fatalf("plan_saved emitted %d times", saved)
	}

	// Approval uses the editable file, rather than the old model-generated content.
	edited := "# Cache fix\n\nUse the revised invalidation strategy."
	if err := os.WriteFile(state.Plan.Path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	approved, err := session.ApprovePlan(agent, nil)
	if err != nil || approved.Content != edited || agent.Mode() != ModeDefault {
		t.Fatalf("approved=%+v mode=%s err=%v", approved, agent.Mode(), err)
	}
	if _, err := session.Run(t.Context(), agent, "Implement the approved plan", nil); err != nil {
		t.Fatal(err)
	}
	state = agent.Collaboration()
	if state.Progress == nil || state.Progress.Plan[0].Status != "in_progress" || state.Plan.Content != edited {
		t.Fatalf("state=%+v", state)
	}
	data, err = os.ReadFile(state.Plan.Path)
	if err != nil || string(data) != edited {
		t.Fatalf("default-mode save overwrote plan: %q %v", data, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	log, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[EventType]int)
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		var record sessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if isCollaborationEvent(EventType(record.Type)) {
			counts[EventType(record.Type)]++
			var event Event
			if err := json.Unmarshal(record.Payload, &event); err != nil {
				t.Fatal(err)
			}
			if err := validateCollaborationEvent(event); err != nil {
				t.Fatal(err)
			}
			if record.Type == string(EventPlanSaved) && (record.RunID == "" || event.Collaboration.Plan.Content != content) {
				t.Fatalf("save record=%+v", record)
			}
		}
	}
	for _, kind := range []EventType{EventModeChanged, EventPlanSaved, EventPlanUpdated, EventPlanApproved} {
		if counts[kind] != 1 {
			t.Fatalf("%s recorded %d times", kind, counts[kind])
		}
	}
	if err := os.Remove(state.Plan.Path); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	nextProvider := &fakeProvider{responses: []Response{{Content: "continued"}}}
	next, err := New(Config{Provider: nextProvider, Model: "test", Tools: tools, PlansDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Restore(next); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.Collaboration(), state) || !reflect.DeepEqual(next.Messages(), agent.Messages()) {
		t.Fatal("restored state differs")
	}
	data, err = os.ReadFile(state.Plan.Path)
	if err != nil || string(data) != edited {
		t.Fatalf("missing plan not recovered: %q %v", data, err)
	}
	if _, err := resumed.Run(t.Context(), next, "continue", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nextProvider.requests[0].SystemPrompt, "Fix cache") {
		t.Fatal("restored progress was not included in request")
	}

	// Mutating a returned snapshot must not alter live state.
	copy := next.Collaboration()
	copy.Progress.Plan[0].Step = "mutated"
	copy.Plan.Content = "mutated"
	if reflect.DeepEqual(next.Collaboration(), copy) {
		t.Fatal("collaboration snapshot aliases live state")
	}
	if err := resumed.Reset(next); err != nil {
		t.Fatal(err)
	}
	if next.Mode() != ModeDefault || next.Collaboration().Plan != nil || next.Collaboration().Progress != nil {
		t.Fatalf("reset state=%+v", next.Collaboration())
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	reset, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Close()
	if err := reset.Restore(next); err != nil || next.Collaboration().Plan != nil || len(next.Messages()) != 0 {
		t.Fatalf("reset restore: %v %+v", err, next.Collaboration())
	}
}

func TestPlanSaveRollsBackWhenSessionWriteFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{{ID: "save", Name: "save_plan", Arguments: json.RawMessage(`{"content":"# Plan\nDo the work."}`)}}, StopReason: "tool_calls"}}}
	agent, err := New(Config{Provider: provider, Model: "test", Mode: ModePlan, PlansDir: dir, Tools: []Tool{NewSavePlanTool()}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_, err = session.Run(t.Context(), agent, "plan", func(event Event) {
		if event.Type == EventToolStart {
			if err := session.file.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err == nil || agent.Collaboration().Plan != nil {
		t.Fatalf("err=%v state=%+v", err, agent.Collaboration())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("uncommitted file remains: %v %v", entries, err)
	}
}

func TestPlanFileFailureDoesNotCommitState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(dir, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "save", Name: "save_plan", Arguments: json.RawMessage(`{"content":"# Plan\nWork."}`)}}},
		{Content: "Could not save."},
	}}
	agent, err := New(Config{Provider: provider, Model: "test", Mode: ModePlan, PlansDir: dir, Tools: []Tool{NewSavePlanTool()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(t.Context(), "plan", nil); err != nil {
		t.Fatal(err)
	}
	if agent.Collaboration().Plan != nil || !agent.Messages()[2].IsError {
		t.Fatal("failed save was committed")
	}
}

func TestPlanProgressRejectsInvalidSteps(t *testing.T) {
	for _, arguments := range []string{
		`{"plan":[{"step":"one","status":"in_progress"},{"step":"two","status":"in_progress"}]}`,
		`{"plan":[{"step":" ","status":"pending"}]}`,
		`{"plan":[{"step":"one","status":"unknown"}]}`,
		`{"plan":[]}`,
	} {
		t.Run(arguments, func(t *testing.T) {
			provider := &fakeProvider{responses: []Response{
				{ToolCalls: []ToolCall{{ID: "update", Name: "update_plan", Arguments: json.RawMessage(arguments)}}},
				{Content: "invalid checklist"},
			}}
			agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{NewUpdatePlanTool()}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := agent.Run(t.Context(), "update", nil); err != nil {
				t.Fatal(err)
			}
			if agent.Collaboration().Progress != nil || !agent.Messages()[2].IsError {
				t.Fatal("invalid checklist accepted")
			}
		})
	}
}

func TestCollaborationChangesRejectBusyOrClosedState(t *testing.T) {
	provider := &fakeProvider{block: make(chan struct{})}
	agent, err := New(Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := session.Run(ctx, agent, "wait", func(event Event) {
			if event.Type == EventRunStart {
				close(started)
			}
		})
		done <- err
	}()
	<-started
	if err := agent.SetMode(ModePlan, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("agent mode error=%v", err)
	}
	if err := session.SetMode(agent, ModePlan, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("session mode error=%v", err)
	}
	if _, err := session.ApprovePlan(agent, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("approval error=%v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.SetMode(agent, ModePlan, nil); err == nil || agent.Mode() != ModeDefault {
		t.Fatalf("closed mode change: %v", err)
	}
}

func TestSessionRejectsMalformedCollaborationCheckpoint(t *testing.T) {
	header := `{"version":1,"seq":1,"session_id":"id","type":"session_start"}` + "\n"
	for _, payload := range []string{
		`{"type":"mode_changed"}`,
		`{"type":"mode_changed","collaboration":{"mode":"unknown"}}`,
		`{"type":"plan_updated","collaboration":{"mode":"default"}}`,
		`{"type":"plan_saved","collaboration":{"mode":"plan"}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			var event Event
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Fatal(err)
			}
			record, err := json.Marshal(sessionRecord{Version: 1, Sequence: 2, SessionID: "id", Type: string(event.Type), Payload: json.RawMessage(payload)})
			if err != nil {
				t.Fatal(err)
			}
			data := header + string(record) + "\n"
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if session, err := OpenSession(path); err == nil {
				session.Close()
				t.Fatal("malformed checkpoint accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatal("malformed log was changed")
			}
		})
	}
}

func TestResumePlanningKeepsModeAndManualFileEdits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "save", Name: "save_plan", Arguments: json.RawMessage(`{"content":"# Original plan\nInspect and implement."}`)}}},
		{Content: "ready"},
	}}
	agent, err := New(Config{Provider: provider, Model: "test", Mode: ModePlan, PlansDir: dir, Tools: []Tool{NewSavePlanTool()}})
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
	plan := agent.Collaboration().Plan
	edited := "# Reviewed plan\nKeep the user's revisions."
	if err := os.WriteFile(plan.Path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	next, err := New(Config{Provider: &fakeProvider{}, Model: "test", PlansDir: dir, Tools: []Tool{NewSavePlanTool()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Restore(next); err != nil {
		t.Fatal(err)
	}
	if next.Mode() != ModePlan {
		t.Fatal("planning mode was not restored")
	}
	data, err := os.ReadFile(plan.Path)
	if err != nil || string(data) != edited {
		t.Fatalf("manual edits overwritten: %q %v", data, err)
	}
	approved, err := resumed.ApprovePlan(next, nil)
	if err != nil || approved.Content != edited {
		t.Fatalf("approved=%+v err=%v", approved, err)
	}
	other, err := New(Config{Provider: &fakeProvider{}, Model: "test", PlansDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Restore(other); err == nil || other.Collaboration().Plan != nil || len(other.Messages()) != 0 {
		t.Fatal("plan restored to an unrelated directory")
	}
}
