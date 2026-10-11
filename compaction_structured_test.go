package iota

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestStructuredMergeInheritsOriginalSourcesAndArchivesChangedDecision(t *testing.T) {
	old := CompactionMemory{Entries: []SummaryEntry{
		{ID: "cache", Section: "decisions", Text: "Use A because project isolation is required.", Status: "active", Sources: []SourceRange{{StartSeq: 4, EndSeq: 8, RunID: "plan", Description: "User chose A."}}},
		{ID: "local", Section: "constraints", Text: "Data must stay local.", Status: "active", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3}}},
	}}
	inputs := []turnInput{{ID: "t12", Messages: []Message{{Role: RoleUser, Content: "Use B instead for isolation."}}, Sources: []SourceRange{{StartSeq: 40, EndSeq: 45, RunID: "revision", StepID: "cache"}}}}
	result := `{"entries":[{"id":"cache","section":"decisions","text":"Use B for isolation; explicitly replaces A.","status":"active","reason":"User explicitly changed the location.","refs":["summary:cache","turn:t12"]},{"id":"local","section":"constraints","text":"Data must stay local.","status":"active","refs":["summary:local"]}]}`
	memory, err := bindStructuredSummary(result, inputs, old)
	if err != nil {
		t.Fatal(err)
	}
	if len(memory.Entries) != 2 || len(memory.Entries[0].Sources) != 2 || memory.Entries[0].Sources[0] != old.Entries[0].Sources[0] || memory.Entries[0].Sources[1].StartSeq != 40 || memory.Entries[0].Sources[1].StepID != "cache" || len(memory.Archived) != 1 || memory.Archived[0].Text != old.Entries[0].Text {
		t.Fatalf("invalid inherited sources or archived decision: %+v", memory)
	}
	if old.Entries[0].Text != "Use A because project isolation is required." {
		t.Fatal("binding mutated old state")
	}
}

func TestStructuredSummaryAllowsNecessaryDetailAndMergesMultipleTurnSources(t *testing.T) {
	text := strings.Repeat("Relevant condition and rationale. ", 60)
	data, err := json.Marshal(structuredOutput{Entries: []entryOutput{{ID: "conditions", Section: "context", Text: text, Status: "active", Refs: []string{"turn:t0", "turn:t1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := bindStructuredSummary(string(data), []turnInput{{ID: "t0", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3}}}, {ID: "t1", Sources: []SourceRange{{StartSeq: 5, EndSeq: 6}}}}, CompactionMemory{})
	if err != nil {
		t.Fatal(err)
	}
	if len(memory.Entries) != 1 || len(memory.Entries[0].Sources) != 2 || !strings.Contains(renderCompactionMemory(memory), text) {
		t.Fatal("joint structured statement lost detail or provenance")
	}
	for _, section := range summarySections {
		if !strings.Contains(renderCompactionMemory(memory), "<"+section+">") {
			t.Fatal("missing section marker", section)
		}
	}
}

func TestStructuredSummaryRejectsMissingStateAndIllegalTransitions(t *testing.T) {
	previous := CompactionMemory{Entries: []SummaryEntry{{ID: "test", Section: "progress", Text: "Run unit tests; integration tests remain pending.", Status: "planned", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3}}}}}
	assistantOnly := []turnInput{{ID: "t0", Messages: []Message{{Role: RoleAssistant, Content: "Everything passed."}}, Sources: []SourceRange{{StartSeq: 5, EndSeq: 6}}}}
	cases := map[string]string{
		"omitted":             `{"entries":[{"id":"new","section":"context","text":"new","status":"active","refs":["turn:t0"]}]}`,
		"unknown":             `{"entries":[{"id":"test","section":"progress","text":"pending","status":"planned","reason":"shorten","refs":["summary:test","turn:invented"]}]}`,
		"duplicate_ref":       `{"entries":[{"id":"test","section":"progress","text":"pending","status":"planned","reason":"shorten","refs":["summary:test","summary:test"]}]}`,
		"missing_reason":      `{"entries":[{"id":"test","section":"progress","text":"changed","status":"planned","refs":["summary:test","turn:t0"]}]}`,
		"planned_to_done":     `{"entries":[{"id":"test","section":"progress","text":"Tests completed.","status":"done","reason":"Assistant says done.","refs":["summary:test","turn:t0"]}]}`,
		"planned_to_verified": `{"entries":[{"id":"test","section":"progress","text":"All tests passed.","status":"verified","reason":"Assistant says done.","refs":["summary:test","turn:t0"]}]}`,
		"section_change":      `{"entries":[{"id":"test","section":"context","text":"pending","status":"active","reason":"move","refs":["summary:test","turn:t0"]}]}`,
		"invented_address":    `{"entries":[{"id":"test","section":"progress","text":"pending","status":"planned","reason":"shorten","refs":["summary:test","turn:t0"],"start_seq":999}]}`,
		"invalid_status":      `{"entries":[{"id":"test","section":"progress","text":"pending","status":"active","reason":"shorten","refs":["summary:test","turn:t0"]}]}`,
		"id_reuse":            `{"entries":[{"id":"test","section":"context","text":"new","status":"active","refs":["turn:t0"]}]}`,
		"trailing":            `{"entries":[]} {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := bindStructuredSummary(body, assistantOnly, previous); err == nil {
				t.Fatal("invalid replacement accepted")
			}
		})
	}
	// Partial verification cannot be promoted from an assistant claim alone.
	previous.Entries[0].Status = "verified_partial"
	body := `{"entries":[{"id":"test","section":"progress","text":"All tests passed.","status":"verified","reason":"Assistant says done.","refs":["summary:test","turn:t0"]}]}`
	if _, err := bindStructuredSummary(body, assistantOnly, previous); err == nil {
		t.Fatal("partial verification promoted without evidence")
	}
}

func TestStructuredProgressRecordsPartialVerificationAndPendingIntegration(t *testing.T) {
	prior := CompactionMemory{Entries: []SummaryEntry{{ID: "verification", Section: "progress", Text: "Run tests.", Status: "planned"}}}
	inputs := []turnInput{{ID: "t0", Messages: []Message{{Role: RoleTool, Content: "go test ./cache passed"}}, Sources: []SourceRange{{StartSeq: 40, EndSeq: 45}}}}
	body := `{"entries":[{"id":"verification","section":"progress","text":"go test ./cache passed; integration tests have not run.","status":"verified_partial","reason":"Unit test result observed; integration remains pending.","refs":["summary:verification","turn:t0"]},{"id":"integration","section":"next_steps","text":"Run integration tests.","status":"planned","refs":["turn:t0"]}]}`
	result, err := bindStructuredSummary(body, inputs, prior)
	if err != nil || result.Entries[0].Status != "verified_partial" {
		t.Fatalf("partial verification rejected: %v", err)
	}
	inputs[0].Incomplete = true
	if _, err := bindStructuredSummary(body, inputs, prior); err == nil {
		t.Fatal("incomplete excerpt certified completion")
	}
}

func TestJointCompactionReadsAllRawTurnsInOneRequest(t *testing.T) {
	provider := &compactionProvider{}
	agent, _ := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 60})
	agent.messages = compactionHistory()
	state, err := agent.Compact(t.Context(), "retain task state", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || len(state.Memory.Entries) != 1 {
		t.Fatalf("expected a single joint summary, got %d requests", len(provider.requests))
	}
	_, rest, _ := strings.Cut(provider.requests[0].Messages[0].Content, "<turns>\n")
	data, _, _ := strings.Cut(rest, "\n</turns>")
	var inputs []turnInput
	if err := json.Unmarshal([]byte(data), &inputs); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || !strings.Contains(inputs[0].Messages[2].Content, "important tail") {
		t.Fatal("joint input lost full original turns")
	}
}

func TestFailedLaterBatchDoesNotApplyEarlierCandidate(t *testing.T) {
	provider := &compactionProvider{respond: func(_ context.Context, _ Request, call int) (Response, error) {
		if call == 1 {
			return Response{}, errors.New("context_length_exceeded")
		}
		if call == 3 {
			return Response{}, errors.New("network unavailable")
		}
		return Response{Content: "first batch checkpoint", StopReason: "stop"}, nil
	}}
	session, agent := snapshotSession(t, provider)
	before := agent.Messages()
	if _, err := session.Compact(t.Context(), agent, "focus", nil); err == nil {
		t.Fatal("expected later-batch failure")
	}
	if len(provider.requests) != 3 || !reflect.DeepEqual(agent.Messages(), before) || !reflect.DeepEqual(session.messages, before) {
		t.Fatal("partial candidate replaced active history")
	}
}

func TestAutomaticBudgetRewritesCompleteStateWithoutDeletingOldest(t *testing.T) {
	memory := CompactionMemory{Entries: []SummaryEntry{
		{ID: "goal", Section: "goal", Text: "Keep cache local.", Status: "active", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3}}},
		{ID: "detail", Section: "context", Text: strings.Repeat("redundant cache detail ", 300), Status: "active", Sources: []SourceRange{{StartSeq: 4, EndSeq: 5}}},
	}}
	provider := &compactionProvider{rawSummary: true, respond: func(_ context.Context, request Request, call int) (Response, error) {
		if !strings.Contains(request.Messages[0].Content, "<previous-summary>") {
			t.Fatal("budget omitted previous state")
		}
		text := memory.Entries[1].Text
		reason := ""
		if call == 2 {
			text = "Cache details remain available in history."
			reason = "Deduplicate low-value detail for the budget."
		}
		data, err := json.Marshal(structuredOutput{Entries: []entryOutput{
			{ID: "goal", Section: "goal", Text: memory.Entries[0].Text, Status: "active", Refs: []string{"summary:goal"}},
			{ID: "detail", Section: "context", Text: text, Status: "active", Refs: []string{"summary:detail"}, Reason: reason},
		}})
		return Response{Content: string(data), StopReason: "stop"}, err
	}}
	agent, _ := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 1})
	agent.messages = []Message{checkpointMessage(memory), {Role: RoleUser, Content: "continue"}, {Role: RoleAssistant, Content: "working"}}
	short := cloneCompactionMemory(memory)
	short.Entries[1].Text = "Cache details remain available in history."
	short.Entries[1].Reason = "Deduplicate low-value detail for the budget."
	original := agent.Messages()
	agent.messages[0] = checkpointMessage(short)
	request, err := agent.currentRequest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target := estimateContextUsage(request, 0).Tokens + 200
	agent.messages = original
	agent.contextLimitTokens = target * 5
	state, err := agent.compact(t.Context(), "", "overflow", true, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 || state.AfterUsage.Tokens > target || len(state.Memory.Entries) != 2 || state.Memory.Entries[0].Text != memory.Entries[0].Text || len(state.Memory.Archived) != 1 || !reflect.DeepEqual(agent.Messages()[1:], original[1:]) {
		t.Fatal("budget lost required state or recent history")
	}
}

func TestUnreachableTargetKeepsNecessaryStateAndReportsUsage(t *testing.T) {
	provider := &compactionProvider{}
	agent, _ := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 1})
	agent.messages = append(compactionHistory()[:4], Message{Role: RoleUser, Content: strings.Repeat("protected recent context ", 10000)}, Message{Role: RoleAssistant, Content: "working"})
	before := agent.Messages()
	state, err := agent.compact(t.Context(), "focus", "overflow", true, 1, nil, nil)
	if err != nil || state.AfterUsage.Tokens <= state.TargetTokens || state.AfterChars >= state.BeforeChars || !reflect.DeepEqual(before[4:], agent.Messages()[1:]) {
		t.Fatalf("unreachable target lost necessary history or usage: %v", err)
	}
}

func TestSummaryReadsUnprunedEvidenceWithoutSession(t *testing.T) {
	const middle = "MIDDLE FACT: only unit tests passed; integration tests have not run."
	provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if !strings.Contains(request.Messages[0].Content, middle) {
			t.Fatal("summary input lost original tool evidence")
		}
		return Response{Content: "Unit tests passed; integration verification remains pending.", StopReason: "stop"}, nil
	}}
	agent, _ := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 60})
	agent.messages = compactionHistory()
	agent.messages[2].Content = strings.Repeat("A", 10000) + middle + strings.Repeat("Z", 10000)
	if _, err := agent.Compact(t.Context(), "preserve evidence", nil); err != nil {
		t.Fatal(err)
	}
}

func TestContextArchivingPreservesRequiredStateAndLookup(t *testing.T) {
	old := CompactionMemory{Entries: []SummaryEntry{
		{ID: "local", Section: "constraints", Text: "Keep data local.", Status: "active", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3}}},
		{ID: "detail", Section: "context", Text: "Earlier experiments used migration-A.", Status: "active", Sources: []SourceRange{{StartSeq: 4, EndSeq: 5}}},
	}}
	body := `{"entries":[{"id":"local","section":"constraints","text":"Keep data local.","status":"active","refs":["summary:local"]},{"id":"detail","section":"context","text":"Earlier experiments used migration-A.","status":"archived","reason":"Historical detail can be queried if needed.","refs":["summary:detail"]}]}`
	memory, err := bindStructuredSummary(body, nil, old)
	if err != nil {
		t.Fatal(err)
	}
	if len(memory.Entries) != 1 || len(memory.Archived) != 1 || strings.Contains(renderCompactionMemory(memory), "migration-A") || len(memorySources(memory)) != 2 {
		t.Fatalf("archiving lost source metadata or kept unnecessary detail: %+v", memory)
	}
	invalid := strings.Replace(body, `"section":"context"`, `"section":"constraints"`, 1)
	if _, err := bindStructuredSummary(invalid, nil, old); err == nil {
		t.Fatal("constraint was archived")
	}
}

func TestConflictKeepsBothClaimsAndOriginalSources(t *testing.T) {
	old := CompactionMemory{Entries: []SummaryEntry{{ID: "cache", Section: "decisions", Text: "User chose A for project isolation.", Status: "active", Sources: []SourceRange{{StartSeq: 2, EndSeq: 3}}}}}
	inputs := []turnInput{{ID: "t0", Messages: []Message{{Role: RoleTool, Content: "Configuration points to B."}}, Sources: []SourceRange{{StartSeq: 8, EndSeq: 9}}}}
	body := `{"entries":[{"id":"cache","section":"decisions","text":"User chose A for project isolation; configuration currently points to B. No explicit replacement found. Read original history and config before choosing.","status":"unresolved","reason":"Observed configuration conflicts with the user's decision.","refs":["summary:cache","turn:t0"]}]}`
	result, err := bindStructuredSummary(body, inputs, old)
	if err != nil || result.Entries[0].Status != "unresolved" || len(result.Entries[0].Sources) != 2 {
		t.Fatalf("conflicting claims lost state or sources: %v", err)
	}
}

func TestLegacyCheckpointCannotSilentlyLoseState(t *testing.T) {
	var memory CompactionMemory
	if err := json.Unmarshal([]byte(`{"ordinary":[{"text":"old work"}],"important":[{"kind":"constraint","key":"local","text":"Keep local.","status":"active"}]}`), &memory); err != nil {
		t.Fatal(err)
	}
	agent, _ := New(Config{Provider: &compactionProvider{}, Model: "test", KeepRecentTokens: 1})
	agent.messages = []Message{{Role: RoleUser, ContextSummary: true, Content: "old state", Checkpoint: &memory}, {Role: RoleUser, Content: strings.Repeat("old task ", 1000)}, {Role: RoleAssistant, Content: "done"}, {Role: RoleUser, Content: "continue"}, {Role: RoleAssistant, Content: "working"}}
	before := agent.Messages()
	if _, err := agent.Compact(t.Context(), "focus", nil); err == nil || !strings.Contains(err.Error(), "legacy summary format") {
		t.Fatalf("legacy state was silently accepted: %v", err)
	}
	if !reflect.DeepEqual(agent.Messages(), before) {
		t.Fatal("legacy checkpoint lost original state")
	}
}

func TestProtectedTurnExceedingWindowFailsWithoutSummarizing(t *testing.T) {
	provider := &compactionProvider{}
	agent, _ := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 1})
	agent.contextLimitTokens = 16000
	agent.messages = append(compactionHistory()[:4], Message{Role: RoleUser, Content: strings.Repeat("protected recent context ", 10000)}, Message{Role: RoleAssistant, Content: "working"})
	before := agent.Messages()
	if _, err := agent.compact(t.Context(), "focus", "overflow", true, 1, nil, nil); err == nil || !strings.Contains(err.Error(), "exceeding reported capacity") {
		t.Fatalf("oversized protected turn was unreported: %v", err)
	}
	if len(provider.requests) != 0 || !reflect.DeepEqual(before, agent.Messages()) {
		t.Fatal("protected turn was summarized or modified")
	}
}

func TestArchivalCannotBypassConstraintAndDuplicateIDValidation(t *testing.T) {
	old := CompactionMemory{Entries: []SummaryEntry{
		{ID: "goal", Section: "goal", Text: "Implement cache.", Status: "active"},
		{ID: "local", Section: "constraints", Text: "Keep data local.", Status: "active"},
	}}
	inputs := []turnInput{{ID: "t0", Messages: []Message{{Role: RoleUser, Content: "continue"}}}}
	body := `{"entries":[{"id":"goal","section":"goal","text":"Implement cache.","status":"active","refs":["summary:goal"]},{"id":"local","section":"constraints","text":"Keep data local.","status":"archived","reason":"Make room.","refs":["summary:local","turn:t0"]}]}`
	if _, err := bindStructuredSummary(body, inputs, old); err == nil {
		t.Fatal("archival bypassed protected constraint validation")
	}
	duplicate := `{"entries":[{"id":"same","section":"context","text":"active","status":"active","refs":["turn:t0"]},{"id":"same","section":"context","text":"discarded","status":"archived","reason":"Old detail.","refs":["turn:t0"]}]}`
	if _, err := bindStructuredSummary(duplicate, inputs, CompactionMemory{}); err == nil {
		t.Fatal("archival bypassed duplicate ID validation")
	}
}

func TestKnownWindowUsesOneJointRequestWhenAllInputFits(t *testing.T) {
	provider := &compactionProvider{}
	session, agent := snapshotSession(t, provider)
	agent.contextLimitTokens = 128000
	var prepared *CompactionInput
	if _, err := session.Compact(t.Context(), agent, "focus", func(event Event) {
		if event.Type == EventCompactionPrepared {
			prepared = event.CompactionInput
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || prepared == nil || len(prepared.Units) != 2 || prepared.Units[0].ID != "t0" || prepared.Units[1].ID != "t1" {
		t.Fatal("known large window was unnecessarily split or prepared IDs were missing")
	}
	_, rest, _ := strings.Cut(provider.requests[0].Messages[0].Content, "<turns>\n")
	data, _, _ := strings.Cut(rest, "\n</turns>")
	var inputs []turnInput
	if err := json.Unmarshal([]byte(data), &inputs); err != nil {
		t.Fatal(err)
	}
	for i, input := range inputs {
		if input.ID != prepared.Units[i].ID || !reflect.DeepEqual(input.Sources, prepared.Units[i].Sources) {
			t.Fatal("request source IDs differed from prepared ranges")
		}
	}
}

func TestBudgetRewriteThatGrowsCannotReplaceSmallerCandidate(t *testing.T) {
	memory := CompactionMemory{Entries: []SummaryEntry{{ID: "detail", Section: "context", Text: strings.Repeat("original detail ", 400), Status: "active"}}}
	provider := &compactionProvider{rawSummary: true, respond: func(_ context.Context, _ Request, call int) (Response, error) {
		text := strings.Repeat("first summary ", 100)
		reason := "Shorten original wording."
		if call == 2 {
			text = strings.Repeat("expanded budget candidate ", 400)
			reason = "Attempt budget rewrite."
		}
		data, err := json.Marshal(structuredOutput{Entries: []entryOutput{{ID: "detail", Section: "context", Text: text, Status: "active", Refs: []string{"summary:detail"}, Reason: reason}}})
		return Response{Content: string(data), StopReason: "stop"}, err
	}}
	agent, _ := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 1})
	agent.messages = []Message{checkpointMessage(memory), {Role: RoleUser, Content: "continue"}, {Role: RoleAssistant, Content: "working"}}
	agent.contextLimitTokens = 6000
	state, err := agent.compact(t.Context(), "", "overflow", true, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 || state.Memory.Entries[0].Text != strings.Repeat("first summary ", 100) {
		t.Fatal("budget expansion replaced the smaller valid candidate")
	}
}

func TestUnresolvedClaimsCannotBeArchivedOrResolvedByAssistantAlone(t *testing.T) {
	old := CompactionMemory{Entries: []SummaryEntry{
		{ID: "goal", Section: "goal", Text: "Implement cache.", Status: "active"},
		{ID: "conflict", Section: "context", Text: "User chose A but config points to B; check original history.", Status: "unresolved"},
	}}
	inputs := []turnInput{{ID: "t0", Messages: []Message{{Role: RoleAssistant, Content: "Conflict is settled."}}}}
	for _, status := range []string{"archived", "resolved"} {
		data, err := json.Marshal(structuredOutput{Entries: []entryOutput{
			{ID: "goal", Section: "goal", Text: old.Entries[0].Text, Status: "active", Refs: []string{"summary:goal"}},
			{ID: "conflict", Section: "context", Text: old.Entries[1].Text, Status: status, Refs: []string{"summary:conflict", "turn:t0"}, Reason: "Assistant says conflict is settled."},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bindStructuredSummary(string(data), inputs, old); err == nil {
			t.Fatal("unresolved claims were discarded without evidence", status)
		}
	}
}
