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

func TestTurnSummariesAndImportantUpdatesBindProgramSources(t *testing.T) {
	inputs := []turnInput{
		{ID: "t0", Sources: []SourceRange{{StartSeq: 4, EndSeq: 5, RunID: "plan"}}},
		{ID: "t1", Sources: []SourceRange{{StartSeq: 8, EndSeq: 9, RunID: "inspect"}}},
	}
	var memory CompactionMemory
	response := `{"turns":[{"id":"t1","text":"Inspected cache migration rules."},{"id":"t0","text":"User chose cache location A."}],"important":[{"kind":"decision","key":"cache_location","text":"Use A because project isolation is required.","status":"active","turn_ids":["t0"]},{"kind":"fact","key":"batch_size","text":"Legacy migration uses batches of 500.","status":"active","turn_ids":["t1"]}]}`
	if err := bindTurnBatch(response, inputs, &memory); err != nil {
		t.Fatal(err)
	}
	if len(memory.Ordinary) != 2 || memory.Ordinary[0].Sources[0].StartSeq != 4 || memory.Ordinary[1].Sources[0].StartSeq != 8 || len(memory.Important) != 2 || memory.Important[0].Sources[0].StartSeq != 4 {
		t.Fatalf("model output changed program source order or addresses: %+v", memory)
	}
	update := `{"turns":[{"id":"t0","text":"User changed the location to B."}],"important":[{"kind":"decision","key":"cache_location","text":"Use B because the user changed the requirement.","status":"active","turn_ids":["t0"]}]}`
	if err := bindTurnBatch(update, []turnInput{{ID: "t0", Sources: []SourceRange{{StartSeq: 12, EndSeq: 13, RunID: "revision"}}}}, &memory); err != nil {
		t.Fatal(err)
	}
	if len(memory.Important) != 2 || memory.Important[0].Text != "Use B because the user changed the requirement." || len(memory.Important[0].Sources) != 2 || memory.Important[0].Sources[1].StartSeq != 12 {
		t.Fatalf("same-topic decision was not updated with both sources: %+v", memory.Important)
	}
}

func TestRepeatedOrdinaryConclusionMergesSources(t *testing.T) {
	inputs := []turnInput{{ID: "t0", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3}}}, {ID: "t1", Sources: []SourceRange{{StartSeq: 5, EndSeq: 6}}}}
	var memory CompactionMemory
	response := `{"turns":[{"id":"t0","text":"Cache migration still needs verification."},{"id":"t1","text":"Cache migration still needs verification."}],"important":[]}`
	if err := bindTurnBatch(response, inputs, &memory); err != nil {
		t.Fatal(err)
	}
	if len(memory.Ordinary) != 1 || len(memory.Ordinary[0].Sources) != 2 || memory.Ordinary[0].Sources[0].StartSeq != 2 || memory.Ordinary[0].Sources[1].StartSeq != 5 {
		t.Fatalf("duplicate conclusion did not retain both addresses: %+v", memory)
	}
}

func TestInvalidImportantCitationPreservesActiveHistory(t *testing.T) {
	provider := &compactionProvider{rawSummary: true, respond: func(context.Context, Request, int) (Response, error) {
		return Response{Content: `{"turns":[{"id":"t0","text":"Old cache reviewed."},{"id":"t1","text":"Work remains."}],"important":[{"kind":"decision","key":"cache","text":"Use A.","status":"active","turn_ids":["invented"]}]}`, StopReason: "stop"}, nil
	}}
	agent, err := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 60})
	if err != nil {
		t.Fatal(err)
	}
	agent.messages = compactionHistory()
	before := agent.Messages()
	if _, err := agent.Compact(t.Context(), "preserve decision", nil); err == nil || !strings.Contains(err.Error(), "unknown turn ID") {
		t.Fatalf("invalid source citation was accepted: %v", err)
	}
	if !reflect.DeepEqual(agent.Messages(), before) {
		t.Fatal("failed summary replaced active history")
	}
}

func TestAutomaticBudgetEvictsOrdinaryButKeepsImportantAndRecent(t *testing.T) {
	memory := CompactionMemory{Important: []ImportantFact{{Kind: "decision", Key: "cache", Text: "User selected option A.", Status: "active", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3}}}}}
	for i := 0; i < 3; i++ {
		memory.Ordinary = append(memory.Ordinary, OrdinarySummary{Text: strings.Repeat("old work summary ", 25), Sources: []SourceRange{{StartSeq: uint64(i*2 + 2), EndSeq: uint64(i*2 + 3)}}})
	}
	recent := []Message{{Role: RoleUser, Content: "continue"}, {Role: RoleAssistant, Content: "working"}}
	agent, err := New(Config{Provider: &compactionProvider{}, Model: "test", KeepRecentTokens: 1, CompactionTargetRatio: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	agent.messages = append([]Message{checkpointMessage(memory)}, recent...)
	before := agent.Messages()
	agent.contextLimitTokens = 1000
	var stages []string
	state, err := agent.compact(t.Context(), "", "overflow", true, 1, func(event Event) {
		if event.Type == EventCompactionProgress {
			stages = append(stages, event.CompactionProgress.Stage)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.TargetTokens != 200 || state.EvictedSummaries != 3 || len(state.Memory.Archived) != 3 || len(state.Memory.Ordinary) != 0 || len(state.Memory.Important) != 1 || !reflect.DeepEqual(agent.Messages()[1:], before[1:]) {
		t.Fatal("budget eviction lost protected data")
	}
	if !strings.Contains(strings.Join(stages, ","), "budget") || !strings.Contains(strings.Join(stages, ","), "evict") {
		t.Fatalf("budget stages missing: %v", stages)
	}
}

func TestAutomaticBudgetEvictsOldestUntilFullRequestFits(t *testing.T) {
	memory := CompactionMemory{}
	for index, name := range []string{"first", "second", "third"} {
		memory.Ordinary = append(memory.Ordinary, OrdinarySummary{Text: name + strings.Repeat(" detail", 55), Sources: []SourceRange{{StartSeq: uint64(index*2 + 2), EndSeq: uint64(index*2 + 3)}}})
	}
	agent, err := New(Config{Provider: &compactionProvider{}, Model: "test", KeepRecentTokens: 1, CompactionTargetRatio: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	agent.messages = []Message{checkpointMessage(memory), {Role: RoleUser, Content: "continue"}, {Role: RoleAssistant, Content: "working"}}
	withOldest := agent.Messages()
	withoutOldest := cloneCompactionMemory(memory)
	withoutOldest.Archived = append(withoutOldest.Archived, withoutOldest.Ordinary[0])
	withoutOldest.Ordinary = withoutOldest.Ordinary[1:]
	agent.messages[0] = checkpointMessage(withoutOldest)
	request, err := agent.currentRequest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target := estimateContextUsage(request, 0).Tokens
	agent.messages = withOldest
	agent.contextLimitTokens = target * 5
	state, err := agent.compact(t.Context(), "", "overflow", true, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.TargetTokens != target || state.AfterUsage.Tokens > target || state.EvictedSummaries != 1 || len(state.Memory.Archived) != 1 || !strings.HasPrefix(state.Memory.Archived[0].Text, "first") || len(state.Memory.Ordinary) != 2 {
		t.Fatalf("did not stop after evicting oldest summary: target=%d after=%d evicted=%d", target, state.AfterUsage.Tokens, state.EvictedSummaries)
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
	memory := CompactionMemory{Archived: []OrdinarySummary{{Text: "Earlier migration used option A.", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3, RunID: "run"}}}}}
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
