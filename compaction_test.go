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

type compactionProvider struct {
	requests []Request
	respond  func(context.Context, Request, int) (Response, error)
}

func (p *compactionProvider) Stream(ctx context.Context, request Request, _ func(Delta)) (Response, error) {
	p.requests = append(p.requests, request)
	if p.respond != nil {
		return p.respond(ctx, request, len(p.requests))
	}
	return Response{Content: "Checkpoint: inspected source; implement next.", StopReason: "stop"}, nil
}

func compactionHistory() []Message {
	messages := []Message{
		{Role: RoleUser, Content: "old request " + strings.Repeat("old context ", 1500)},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "old-read", Name: "read", Arguments: json.RawMessage(`{"path":"old.go"}`)}}},
		{Role: RoleTool, ToolCallID: "old-read", ToolName: "read", Content: strings.Repeat("旧文件内容", 4000) + "important tail"},
		{Role: RoleAssistant, Content: "old complete"},
	}
	for _, prompt := range []string{"recent one", "recent two", "recent three"} {
		messages = append(messages, Message{Role: RoleUser, Content: prompt}, Message{Role: RoleAssistant, Content: "recent answer"})
	}
	return messages
}

func TestCompactPrunesToolsAndProtectsRecentTurns(t *testing.T) {
	provider := &compactionProvider{}
	agent, err := New(Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	agent.messages = compactionHistory()
	before := agent.Messages()
	state, err := agent.Compact(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != "tool_results" || state.TrimmedToolResults != 1 || state.KeptTurns != 3 || len(provider.requests) != 0 {
		t.Fatalf("state=%+v requests=%d", state, len(provider.requests))
	}
	after := agent.Messages()
	if !reflect.DeepEqual(before[4:], after[4:]) || !strings.HasSuffix(after[2].Content, "important tail") || !strings.Contains(after[2].Content, "omitted during compaction") {
		t.Fatal("recent messages or old tool tail were not preserved")
	}
	if pending, err := pendingSessionCalls(after); err != nil || len(pending) != 0 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}

func TestCompactFocusSummaryAndRepeatedCompaction(t *testing.T) {
	provider := &compactionProvider{}
	agent, _ := New(Config{Provider: provider, Model: "test", KeepRecentTurns: 3})
	agent.messages = compactionHistory()
	before := agent.Messages()
	var events []Event
	state, err := agent.Compact(context.Background(), "保留未解决问题", func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != "summary" || state.SummarizedMessages != 4 || !state.Context.Messages[0].ContextSummary || !reflect.DeepEqual(before[4:], state.Context.Messages[1:]) {
		t.Fatalf("incorrect summary checkpoint: %+v", state)
	}
	request := provider.requests[0]
	if len(request.Tools) != 0 || !strings.Contains(request.SystemPrompt, "CONTEXT CHECKPOINT COMPACTION") || !strings.Contains(request.Messages[0].Content, "保留未解决问题") || strings.Contains(request.Messages[0].Content, "recent one") {
		t.Fatalf("summary request=%+v", request)
	}
	if events[0].Type != EventCompactionStart || events[len(events)-2].Type != EventContextCompacted || events[len(events)-1].Type != EventCompactionEnd {
		t.Fatalf("events=%+v", events)
	}
	agent.messages = append(agent.messages, Message{Role: RoleUser, Content: strings.Repeat("next ", 800)}, Message{Role: RoleAssistant, Content: "done"})
	if _, err := agent.Compact(context.Background(), "carry prior decisions", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provider.requests[1].Messages[0].Content, state.Summary) {
		t.Fatal("previous checkpoint missing from next summary")
	}
}

func TestAutomaticCompactionStagesAndRunResult(t *testing.T) {
	provider := &compactionProvider{respond: func(_ context.Context, request Request, call int) (Response, error) {
		if request.SystemPrompt == compactionPrompt {
			return Response{Content: "Old work checkpoint", StopReason: "stop"}, nil
		}
		if call <= 2 {
			return Response{}, errors.New("maximum context length is 128000 tokens; requested 142912 tokens")
		}
		return Response{Content: "recovered", StopReason: "stop"}, nil
	}}
	agent, _ := New(Config{Provider: provider, Model: "test", MaxTurns: 1})
	agent.messages = compactionHistory()
	var states []CompactionState
	result, err := agent.Run(context.Background(), "continue", func(event Event) {
		if event.Type == EventContextCompacted {
			states = append(states, *event.Compaction)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered" || result.Turns != 1 || len(result.Messages) != 2 || result.Messages[0].Content != "continue" {
		t.Fatalf("result=%+v", result)
	}
	if len(states) != 2 || states[0].Stage != "tool_results" || states[1].Stage != "summary" || states[1].ContextLimitTokens != 128000 {
		t.Fatalf("states=%+v", states)
	}
	if len(provider.requests) != 5 || len(provider.requests[2].Tools) != 0 || len(provider.requests[3].Tools) != 0 {
		t.Fatalf("requests=%d", len(provider.requests))
	}
}

func TestCompactionFailurePreservesHistory(t *testing.T) {
	for _, scenario := range []string{"length", "tool", "empty", "oversized", "error", "cancel", "write"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := &compactionProvider{respond: func(_ context.Context, _ Request, _ int) (Response, error) {
				switch scenario {
				case "length":
					return Response{Content: "partial", StopReason: "length"}, nil
				case "tool":
					return Response{Content: "summary", StopReason: "stop", ToolCalls: []ToolCall{{ID: "unexpected", Name: "write", Arguments: json.RawMessage(`{}`)}}}, nil
				case "empty":
					return Response{StopReason: "stop"}, nil
				case "oversized":
					return Response{Content: strings.Repeat("x", summaryMaxChars+1), StopReason: "stop"}, nil
				case "error":
					return Response{}, errors.New("network unavailable")
				case "cancel":
					cancel()
				}
				return Response{Content: "valid summary", StopReason: "stop"}, nil
			}}
			agent, _ := New(Config{Provider: provider, Model: "test"})
			agent.messages = compactionHistory()
			before := agent.Messages()
			var record func(Event) error
			if scenario == "write" {
				record = func(Event) error { return errors.New("disk full") }
			}
			_, err := agent.compactIdle(ctx, "focus", nil, record)
			if err == nil || !reflect.DeepEqual(agent.Messages(), before) {
				t.Fatalf("err=%v history changed=%v", err, !reflect.DeepEqual(agent.Messages(), before))
			}
		})
	}
}

func TestCompactionSplitsOversizedSummaryInput(t *testing.T) {
	var pieces []string
	provider := &compactionProvider{respond: func(_ context.Context, request Request, call int) (Response, error) {
		if call == 1 {
			return Response{}, errors.New("prompt is too long: 50000 tokens > 16000 maximum")
		}
		text := request.Messages[0].Content
		piece := strings.Split(strings.Split(text, "<conversation>\n")[1], "\n</conversation>")[0]
		pieces = append(pieces, piece)
		if call > 2 && !strings.Contains(text, "prior checkpoint") {
			t.Fatal("lost previous chunk summary")
		}
		return Response{Content: "prior checkpoint", StopReason: "stop"}, nil
	}}
	agent, _ := New(Config{Provider: provider, Model: "test"})
	agent.messages = compactionHistory()
	_, err := agent.Compact(context.Background(), "focus", nil)
	if err != nil {
		t.Fatal(err)
	}
	var recovered []Message
	for _, piece := range pieces {
		var chunk []Message
		if err := json.Unmarshal([]byte(piece), &chunk); err != nil {
			t.Fatal(err)
		}
		if pending, err := pendingSessionCalls(chunk); err != nil || len(pending) != 0 {
			t.Fatalf("split tool batch: %v", err)
		}
		recovered = append(recovered, chunk...)
	}
	if len(pieces) < 2 || len(recovered) != 4 {
		t.Fatalf("incomplete conversation: %d pieces, %d messages", len(pieces), len(recovered))
	}
}

func TestCompactionRespectsRecentOnlyAndBusy(t *testing.T) {
	agent, _ := New(Config{Provider: &compactionProvider{}, Model: "test"})
	agent.messages = compactionHistory()[4:]
	if _, err := agent.Compact(context.Background(), "", nil); !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("err=%v", err)
	}
	agent.running = true
	if _, err := agent.Compact(context.Background(), "", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("err=%v", err)
	}
}

func TestCompactionPreservesPlanAndReloadsInstructions(t *testing.T) {
	provider := &compactionProvider{}
	rule := "AGENTS rule before"
	agent, err := New(Config{Provider: provider, Model: "test", Mode: ModePlan,
		SystemPromptLoader: func(context.Context) (string, error) { return rule + "\nActive skill instructions", nil },
		Tools: []Tool{{Name: "read", ReadOnly: true, Schema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { return "", nil }},
			{Name: "write", Schema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { return "", nil }}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.messages = compactionHistory()
	agent.collaboration.Plan = &SavedPlan{ID: "00000000-0000-4000-8000-000000000001", Path: filepath.Join(t.TempDir(), "00000000-0000-4000-8000-000000000001.md"), Content: "# Current plan\nStill requires user approval."}
	before := agent.Collaboration()
	if _, err := agent.Compact(context.Background(), "focus", nil); err != nil {
		t.Fatal(err)
	}
	rule = "AGENTS rule after"
	if _, err := agent.Run(context.Background(), "next", nil); err != nil {
		t.Fatal(err)
	}
	request := provider.requests[len(provider.requests)-1]
	if request.Mode != ModePlan || !strings.Contains(request.SystemPrompt, rule) || !reflect.DeepEqual(requestCollaboration(t, request), before) || !strings.Contains(request.SystemPrompt, "Active skill instructions") || len(request.Tools) != 1 || request.Tools[0].Name != "read" || !reflect.DeepEqual(before, agent.Collaboration()) {
		t.Fatalf("request=%+v state=%+v", request, agent.Collaboration())
	}
}

func TestSessionCompactionRestoresCheckpointAndKeepsRawHistory(t *testing.T) {
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range compactionHistory() {
		if err := session.write("seed", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: cloneMessagePointer(message)}); err != nil {
			t.Fatal(err)
		}
	}
	agent, _ := New(Config{Provider: &compactionProvider{}, Model: "test"})
	if err := session.Restore(agent); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Compact(context.Background(), agent, "keep decisions", nil); err != nil {
		t.Fatal(err)
	}
	want := agent.Messages()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "old request") || strings.Count(string(data), `"type":"context_compacted"`) != 2 {
		t.Fatal("raw history missing or checkpoint duplicated")
	}
	restored, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	fresh, _ := New(Config{Provider: &compactionProvider{}, Model: "test"})
	if err := restored.Restore(fresh); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Messages(), want) {
		t.Fatal("restored pre-compaction messages")
	}
	if _, err := restored.Run(context.Background(), fresh, "after restore", nil); err != nil {
		t.Fatal(err)
	}
	if err := restored.Reset(fresh); err != nil {
		t.Fatal(err)
	}
	if len(fresh.Messages()) != 0 {
		t.Fatal("reset retained checkpoint")
	}
}

func TestContextOverflowClassification(t *testing.T) {
	for _, test := range []struct {
		message  string
		limit    int
		overflow bool
	}{
		{"maximum context length is 128000 tokens; requested 142912", 128000, true},
		{"Input length (265330) exceeds model's maximum context length (262144)", 262144, true},
		{"prompt is too long: 210000 tokens > 200000 maximum", 200000, true},
		{"Your input exceeds the context window of this model", 0, true},
		{"context_length_exceeded", 0, true},
		{"rate_limit_exceeded: context_length_exceeded", 0, false},
		{"chat completions returned 429 Too Many Requests: prompt is too long", 0, false},
		{"too many tokens per minute", 0, false},
		{"network unavailable", 0, false},
	} {
		limit, overflow := contextOverflow(errors.New(test.message))
		if limit != test.limit || overflow != test.overflow {
			t.Errorf("%s: got (%d,%v)", test.message, limit, overflow)
		}
	}
}

func TestAutomaticRecoveryIsBoundedAndIgnoresRateLimits(t *testing.T) {
	for _, rateLimit := range []bool{false, true} {
		provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
			if request.SystemPrompt == compactionPrompt {
				return Response{Content: "checkpoint", StopReason: "stop"}, nil
			}
			if rateLimit {
				return Response{}, errors.New("429 Too Many Requests: token rate limit")
			}
			return Response{}, errors.New("context_length_exceeded")
		}}
		agent, _ := New(Config{Provider: provider, Model: "test"})
		agent.messages = compactionHistory()
		_, err := agent.Run(context.Background(), "continue", nil)
		if err == nil {
			t.Fatal("expected terminal error")
		}
		want := 5
		if rateLimit {
			want = 1
		}
		if len(provider.requests) != want {
			t.Fatalf("rate limit=%v requests=%d", rateLimit, len(provider.requests))
		}
	}
}

func TestCompactRefusesUnpairedToolCalls(t *testing.T) {
	agent, _ := New(Config{Provider: &compactionProvider{}, Model: "test"})
	agent.messages = append(compactionHistory(), Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "pending", Name: "read", Arguments: json.RawMessage(`{}`)}}})
	before := agent.Messages()
	if _, err := agent.Compact(context.Background(), "", nil); err == nil || !reflect.DeepEqual(before, agent.Messages()) {
		t.Fatalf("err=%v", err)
	}
}

func TestSessionAutoCompactionRestoresKnownLimitOnlyForSameModel(t *testing.T) {
	provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if request.SystemPrompt == compactionPrompt {
			return Response{Content: "summary", StopReason: "stop"}, nil
		}
		if !request.Messages[0].ContextSummary {
			return Response{}, errors.New("maximum context length is 16000 tokens")
		}
		return Response{Content: "done", StopReason: "stop"}, nil
	}}
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range compactionHistory() {
		if err := session.write("seed", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: cloneMessagePointer(message)}); err != nil {
			t.Fatal(err)
		}
	}
	agent, _ := New(Config{Provider: provider, Model: "test"})
	if err := session.Restore(agent); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(context.Background(), agent, "next", nil); err != nil {
		t.Fatal(err)
	}
	want := agent.Messages()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, model := range []string{"test", "another-model"} {
		fresh, _ := New(Config{Provider: &compactionProvider{}, Model: model})
		if err := restored.Restore(fresh); err != nil {
			t.Fatal(err)
		}
		limit := 0
		if model == "test" {
			limit = 16000
		}
		if fresh.contextLimitTokens != limit || !reflect.DeepEqual(fresh.Messages(), want) {
			t.Fatalf("model=%s limit=%d history restored=%v", model, fresh.contextLimitTokens, reflect.DeepEqual(fresh.Messages(), want))
		}
	}
}
