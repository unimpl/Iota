package iota

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCompactionUnitsKeepBatchesTogetherAcrossStepTransitions(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: "task", Source: &MessageSource{Seq: 2, RunID: "a"}},
		{Role: RoleAssistant, Source: &MessageSource{Seq: 3, RunID: "a", StepID: "one"}, ToolCalls: []ToolCall{{ID: "read", Name: "read", Arguments: json.RawMessage(`{}`)}, {ID: "progress", Name: "update_progress", Arguments: json.RawMessage(`{}`)}}},
		{Role: RoleTool, ToolCallID: "read", Source: &MessageSource{Seq: 4, RunID: "a", StepID: "one"}},
		{Role: RoleTool, ToolCallID: "progress", Source: &MessageSource{Seq: 5, RunID: "a", StepID: "two"}},
		{Role: RoleAssistant, Content: "next step", Source: &MessageSource{Seq: 6, RunID: "a", StepID: "two"}},
		{Role: RoleUser, Content: "continue", Source: &MessageSource{Seq: 9, RunID: "b", StepID: "two"}},
		{Role: RoleAssistant, Content: "continued", Source: &MessageSource{Seq: 10, RunID: "b", StepID: "two"}},
	}
	units := compactionUnits(messages, summaryInputTokens)
	if len(units) != 4 || units[1].StepID != "one" || len(units[1].Messages) != 3 || units[3].RunID != "b" {
		t.Fatalf("incorrect run/step units: %+v", units)
	}
	for _, unit := range units {
		if pending, err := pendingSessionCalls(unit.Messages); err != nil || len(pending) != 0 {
			t.Fatalf("unit broke tool pairing: %v", err)
		}
	}
	ranges := sourceRanges(messages)
	count := 0
	for _, source := range ranges {
		if source.StepID == "two" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("resumed step lost its separate run ranges: %+v", ranges)
	}
}

func TestOverflowCompactsWithinOneRunAndPreservesLatestBatch(t *testing.T) {
	messages := []Message{{Role: RoleUser, Content: "implement cache"}}
	for i := 0; i < 6; i++ {
		id := string(rune('a' + i))
		messages = append(messages,
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: "read", Arguments: json.RawMessage(`{}`)}}},
			Message{Role: RoleTool, ToolCallID: id, Content: strings.Repeat("earlier source ", 500)},
		)
	}
	provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if request.SystemPrompt == compactionPrompt {
			return Response{Content: "Goal: implement cache. Earlier files inspected; next batch retained. Look up sources when needed.", StopReason: "stop"}, nil
		}
		if !request.Messages[0].ContextSummary {
			return Response{}, errors.New("context_length_exceeded")
		}
		return Response{Content: "continued", StopReason: "stop"}, nil
	}}
	agent, _ := New(Config{Provider: provider, Model: "test"})
	agent.messages = messages
	before := agent.Messages()
	state, err := agent.compact(t.Context(), "focus", "overflow", true, 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.SummarizedMessages <= 0 || !agent.Messages()[0].ContextSummary {
		t.Fatal("protected long run could not be compacted")
	}
	after := agent.Messages()
	if !reflect.DeepEqual(before[len(before)-2:], after[len(after)-2:]) {
		t.Fatal("latest tool batch was not retained verbatim")
	}
	if pending, err := pendingSessionCalls(after); err != nil || len(pending) != 0 {
		t.Fatalf("checkpoint has invalid pairing: %v", err)
	}
}

func TestOversizedAtomicSummaryBlockUsesValidExcerptsAndOriginalSources(t *testing.T) {
	messages := []Message{
		{Role: RoleAssistant, Source: &MessageSource{Seq: 20, RunID: "run", StepID: "step"}, ToolCalls: []ToolCall{{ID: "large", Name: "read", Arguments: json.RawMessage(`{"path":"file"}`)}}},
		{Role: RoleTool, ToolCallID: "large", Content: strings.Repeat("中间细节", 10000), Source: &MessageSource{Seq: 21, RunID: "run", StepID: "step"}},
	}
	provider := &compactionProvider{respond: func(_ context.Context, request Request, call int) (Response, error) {
		if call == 1 {
			return Response{}, errors.New("context_length_exceeded")
		}
		text := request.Messages[0].Content
		piece := strings.SplitN(strings.SplitN(text, "<conversation>\n", 2)[1], "\n</conversation>", 2)[0]
		var input []Message
		if err := json.Unmarshal([]byte(piece), &input); err != nil {
			t.Fatal(err)
		}
		if pending, err := pendingSessionCalls(input); err != nil || len(pending) != 0 {
			t.Fatal("atomic batch was split")
		}
		if !strings.Contains(text, "seq 20-21") || !strings.Contains(input[1].Content, "read_history") {
			t.Fatal("excerpt lost original source references")
		}
		return Response{Content: "Large result excerpt only; retrieve original seq 20-21 before relying on omitted details.", StopReason: "stop"}, nil
	}}
	agent, _ := New(Config{Provider: provider, Model: "test"})
	if _, err := agent.summarize(t.Context(), messages, "", 1, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(messages[1].Content, strings.Repeat("中间细节", 10000)) {
		t.Fatal("summary fallback changed original messages")
	}
}
