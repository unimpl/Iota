package iota

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestTokenProtectionKeepsCompleteRecentToolTurn(t *testing.T) {
	recent := []Message{
		{Role: RoleUser, Content: "inspect the latest file"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "read", Name: "read", Arguments: json.RawMessage(`{"path":"recent.go"}`)}}},
		{Role: RoleTool, ToolCallID: "read", ToolName: "read", Content: strings.Repeat("recent output ", 4000)},
	}
	agent, err := New(Config{Provider: &compactionProvider{}, Model: "test", KeepRecentTokens: 8000})
	if err != nil {
		t.Fatal(err)
	}
	agent.messages = append([]Message{{Role: RoleUser, Content: strings.Repeat("old task ", 3000)}, {Role: RoleAssistant, Content: "old result"}}, recent...)
	before := agent.Messages()
	cut, kept, tokens := compactionBoundary(before, agent.keepRecentTokens)
	if cut != 2 || kept != 1 || tokens < 8000 {
		t.Fatalf("cut=%d kept=%d tokens=%d", cut, kept, tokens)
	}
	state, err := agent.compact(t.Context(), "summarize old task", "overflow", true, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(agent.Messages()[len(agent.Messages())-len(recent):], before[len(before)-len(recent):]) || state.KeptTokens < 8000 {
		t.Fatal("automatic compaction crossed the protected tool-turn boundary")
	}
}

func TestSummaryReadsOriginalToolOutputAfterEarlierPruning(t *testing.T) {
	const middle = "MIDDLE RULE: preserve new records on conflict"
	provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if request.SystemPrompt == compactionPrompt && !strings.Contains(request.Messages[0].Content, middle) {
			t.Fatal("summary read the previously pruned excerpt instead of the original tool result")
		}
		return Response{Content: "Old cache output inspected.", StopReason: "stop"}, nil
	}}
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, message := range compactionHistory() {
		if message.Role == RoleTool {
			message.Content = strings.Repeat("A", 10000) + middle + strings.Repeat("Z", 10000)
		}
		if err := session.write("seed", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: cloneMessagePointer(message)}); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Restore(agent); err != nil {
		t.Fatal(err)
	}
	first, err := session.Compact(t.Context(), agent, "", nil)
	if err != nil || first.Stage != "tool_results" {
		t.Fatalf("prune err=%v stage=%s", err, first.Stage)
	}
	if strings.Contains(agent.Messages()[2].Content, middle) {
		t.Fatal("test tool output was not actually pruned")
	}
	if _, err := session.Compact(t.Context(), agent, "preserve migration rules", nil); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedSummariesAreSearchableByTheirOwnTerms(t *testing.T) {
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, message := range []Message{{Role: RoleUser, Content: "cache"}, {Role: RoleAssistant, Content: "done"}} {
		if err := session.write("run", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: &message}); err != nil {
			t.Fatal(err)
		}
	}
	memory := CompactionMemory{Archived: []ArchivedSummary{{Text: "Earlier migration used option A.", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3, RunID: "run"}}}}}
	session.mu.Lock()
	session.messages = []Message{checkpointMessage(memory)}
	session.mu.Unlock()
	text, err := session.queryHistory(t.Context(), historyArguments{Query: "migration"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var page historyPage
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || len(page.SummaryMatches) != 1 || page.SummaryMatches[0].Sources[0].StartSeq != 2 {
		t.Fatalf("archived summary did not route to original seq: %+v", page)
	}
}

func TestCompactionDefaultsAndInvalidBudgetConfig(t *testing.T) {
	provider := &compactionProvider{}
	agent, err := New(Config{Provider: provider, Model: "test"})
	if err != nil || agent.keepRecentTokens != 8000 || agent.compactionTargetRatio != 0.2 {
		t.Fatalf("default token protection or target ratio is wrong: %v", err)
	}
	for _, config := range []Config{
		{Provider: provider, Model: "test", KeepRecentTokens: -1},
		{Provider: provider, Model: "test", CompactionTargetRatio: -0.1},
		{Provider: provider, Model: "test", CompactionTargetRatio: 1},
	} {
		if _, err := New(config); err == nil {
			t.Fatal("invalid compaction configuration was accepted")
		}
	}
}

func TestUnstructuredExistingCheckpointIsNotSilentlyDiscarded(t *testing.T) {
	agent, err := New(Config{Provider: &compactionProvider{}, Model: "test", KeepRecentTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	agent.messages = []Message{
		{Role: RoleUser, ContextSummary: true, Content: "Earlier decision: use cache A."},
		{Role: RoleUser, Content: strings.Repeat("old work ", 200)},
		{Role: RoleAssistant, Content: "old answer"},
		{Role: RoleUser, Content: "continue"},
		{Role: RoleAssistant, Content: "working"},
	}
	before := agent.Messages()
	if _, err := agent.Compact(t.Context(), "preserve decisions", nil); err == nil || !strings.Contains(err.Error(), "no structured memory") {
		t.Fatalf("unstructured checkpoint was accepted: %v", err)
	}
	if !reflect.DeepEqual(agent.Messages(), before) {
		t.Fatal("failed compaction replaced historical checkpoint")
	}
}
