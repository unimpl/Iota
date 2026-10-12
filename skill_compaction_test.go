package iota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSkillInstructionsSurviveCompactionAndSessionRestore(t *testing.T) {
	dir := t.TempDir()
	content := writeTestSkill(t, dir, "review", "Keep this exact multi-step instruction, including the middle.\n"+strings.Repeat("Detailed procedure. ", 300))
	provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if request.SystemPrompt == compactionPrompt {
			if strings.Contains(request.Messages[0].Content, "Detailed procedure.") || !strings.Contains(request.Messages[0].Content, "instructions activated") {
				t.Fatal("summary rewrites skill instructions or loses activation provenance")
			}
			return Response{Content: "Review is unfinished; preserve user selection and workflow progress.", StopReason: "stop"}, nil
		}
		return Response{Content: "Working " + strings.Repeat("historical detail ", 1500), StopReason: "stop"}, nil
	}}
	agent := newSkillAgent(t, dir, provider)
	agent.keepRecentTokens = 1
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Run(t.Context(), agent, "$skill:review review this project", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), agent, "Continue the review", nil); err != nil {
		t.Fatal(err)
	}
	active := agent.ActiveSkills()
	state, err := session.Compact(t.Context(), agent, "retain progress", nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != "summary" || !reflect.DeepEqual(agent.ActiveSkills(), active) {
		t.Fatal("summary changed durable instruction state")
	}
	for _, message := range agent.Messages() {
		if message.Skill != nil || strings.Contains(message.Content, content) || message.RuntimeContext {
			t.Fatal("skill snapshot was persisted inside compacted history")
		}
	}
	request, err := agent.currentRequest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tail := request.Messages[len(request.Messages)-1]
	if !tail.RuntimeContext || !strings.Contains(tail.Content, content) || !strings.Contains(tail.Content, active[0].Hash) || state.AfterChars != requestChars(request) {
		t.Fatal("request did not rehydrate the exact original or budget excluded skill content")
	}
	// Repeated compaction must preserve the same snapshot, including budget rewrites.
	if _, err := session.Run(t.Context(), agent, "Continue", nil); err != nil {
		t.Fatal(err)
	}
	provider.respond = func(_ context.Context, request Request, _ int) (Response, error) {
		return Response{Content: "Review remains unfinished.", StopReason: "stop"}, nil
	}
	if _, err := session.Compact(t.Context(), agent, "retain progress again", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	fresh := newSkillAgent(t, dir, &fakeProvider{responses: []Response{{Content: "resumed"}}})
	if err := resumed.Restore(fresh); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.ActiveSkills(), active) || len(fresh.Skills().Skills) != 0 {
		t.Fatal("restoration depends on the current installation rather than recorded version")
	}
	if _, err := resumed.Run(t.Context(), fresh, "Continue", nil); err != nil {
		t.Fatal(err)
	}
	if request, err := fresh.currentRequest(t.Context()); err != nil || !strings.Contains(request.Messages[len(request.Messages)-1].Content, content) {
		t.Fatal("restored request lost skill content:", err)
	}
	if err := resumed.Reset(fresh); err != nil || len(fresh.ActiveSkills()) != 0 {
		t.Fatal("reset retained active skill state:", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	afterReset, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer afterReset.Close()
	if len(afterReset.activeSkills) != 0 {
		t.Fatal("reset skills resurrected on replay")
	}
}

func TestSkillCompactionPruningAndOverflowProtection(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "review", strings.Repeat("Must preserve full instructions. ", 500))
	provider := &compactionProvider{}
	agent := newSkillAgent(t, dir, provider)
	agent.keepRecentTokens = 60
	activation, err := agent.skillActivation("review", "model")
	if err != nil {
		t.Fatal(err)
	}
	agent.activeSkills = []SkillActivation{activation}
	instruction := Message{Role: RoleTool, ToolName: "read_skill", ToolCallID: "skill", Content: renderSkill(activation), Skill: &SkillReference{Name: "review", Hash: activation.Hash}}
	agent.messages = append([]Message{
		{Role: RoleUser, Content: "Review"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "skill", Name: "read_skill", Arguments: json.RawMessage(`{"name":"review"}`)}}},
		instruction,
	}, compactionHistory()...)
	state, err := agent.Compact(t.Context(), "", nil)
	if err != nil || state.Stage != "tool_results" || state.TrimmedToolResults != 1 || !reflect.DeepEqual(agent.Messages()[2], *cloneMessagePointer(instruction)) {
		t.Fatalf("pruning modified skill instruction state: stage=%s err=%v", state.Stage, err)
	}
	// Protected authoritative skill text must count against the actual capacity.
	agent.contextLimitTokens = 2000
	before := agent.Messages()
	if _, err := agent.compact(t.Context(), "focus", "overflow", true, 1, nil, nil); err == nil || !strings.Contains(err.Error(), "exceeding reported capacity") {
		t.Fatal("oversized authoritative skills were silently discarded:", err)
	}
	if !reflect.DeepEqual(before, agent.Messages()) || len(provider.requests) != 0 {
		t.Fatal("oversized authoritative instructions caused mutation or summary work")
	}
}

func TestSkillEventsAreDurableAndRestoreModelUse(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "review", "Body")
	if err := os.WriteFile(dir+"/review/reference.md", []byte("Reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "activate", Name: "read_skill", Arguments: json.RawMessage(`{"name":"review"}`)}, {ID: "read", Name: "read_skill_resource", Arguments: json.RawMessage(`{"name":"review","path":"reference.md"}`)}}},
		{Content: "done"},
	}}
	agent := newSkillAgent(t, dir, provider)
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), agent, "Review", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ReloadSkills(t.Context(), agent, nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record sessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		counts[record.Type]++
	}
	for _, kind := range []EventType{EventSkillActivated, EventSkillResourceRead, EventSkillsReloaded} {
		if counts[string(kind)] != 1 {
			t.Fatalf("event %s persisted %d times", kind, counts[string(kind)])
		}
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	fresh := newSkillAgent(t, dir, &fakeProvider{})
	if err := resumed.Restore(fresh); err != nil || !reflect.DeepEqual(fresh.ActiveSkills(), agent.ActiveSkills()) {
		t.Fatal("model activation was not restored:", err)
	}
}

func TestSkillCompactionFailurePreservesInstructions(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "review", "Instruction")
	provider := &compactionProvider{respond: func(context.Context, Request, int) (Response, error) { return Response{}, errors.New("network failed") }}
	agent := newSkillAgent(t, dir, provider)
	agent.keepRecentTokens = 60
	activation, _ := agent.skillActivation("review", "user")
	agent.activeSkills = []SkillActivation{activation}
	agent.messages = append([]Message{{Role: RoleUser, Content: renderSkill(activation), Skill: &SkillReference{Name: "review", Hash: activation.Hash}}}, compactionHistory()...)
	before := agent.Messages()
	active := agent.ActiveSkills()
	if _, err := agent.Compact(t.Context(), "retain progress", nil); err == nil || !reflect.DeepEqual(before, agent.Messages()) || !reflect.DeepEqual(active, agent.ActiveSkills()) {
		t.Fatal("failed compaction changed instructions or history")
	}
}

func TestSkillAutomaticOverflowRecoveryRetainsCompleteInstructions(t *testing.T) {
	dir := t.TempDir()
	content := writeTestSkill(t, dir, "review", "Exact original procedure.\n"+strings.Repeat("Detailed steps. ", 300))
	normalRequests := 0
	provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if request.SystemPrompt == compactionPrompt {
			if strings.Contains(request.Messages[0].Content, "Detailed steps.") {
				t.Fatal("overflow summary rewrote protected skill instructions")
			}
			return Response{Content: "Review remains in progress.", StopReason: "stop"}, nil
		}
		normalRequests++
		present := false
		for _, message := range request.Messages {
			if strings.Contains(message.Content, content) {
				present = true
			}
		}
		if !present {
			t.Fatal("a recovery request lost the original instructions")
		}
		if normalRequests < 3 {
			return Response{}, errors.New("maximum context length is 128000 tokens")
		}
		return Response{Content: "recovered", StopReason: "stop"}, nil
	}}
	agent := newSkillAgent(t, dir, provider)
	agent.keepRecentTokens = 60
	activation, _ := agent.skillActivation("review", "user")
	agent.activeSkills = []SkillActivation{activation}
	agent.messages = append([]Message{{Role: RoleUser, Content: renderSkill(activation), Skill: &SkillReference{Name: "review", Hash: activation.Hash}}}, compactionHistory()...)
	result, err := agent.Run(t.Context(), "Continue the review", nil)
	if err != nil || result.Text != "recovered" || normalRequests != 3 || !reflect.DeepEqual(agent.ActiveSkills(), []SkillActivation{activation}) {
		t.Fatalf("recovery result=%+v requests=%d err=%v", result, normalRequests, err)
	}
}
