package iota

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// A short oldest turn can grow when summarized; the protected three turns are not summary input.
func TestCompactionScopeAndRejectedCandidateAreRecorded(t *testing.T) {
	provider := &compactionProvider{respond: func(context.Context, Request, int) (Response, error) {
		return Response{Content: strings.Repeat("Expanded summary. ", 120), StopReason: "stop"}, nil
	}}
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	messages := []Message{{Role: RoleUser, Content: "old greeting"}, {Role: RoleAssistant, Content: "old reply"}}
	for _, text := range []string{"protected one", "protected two", "protected three"} {
		messages = append(messages, Message{Role: RoleUser, Content: text}, Message{Role: RoleAssistant, Content: "keep this reply"})
	}
	for _, message := range messages {
		if err := session.write("seed", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: cloneMessagePointer(message)}); err != nil {
			t.Fatal(err)
		}
	}
	agent, _ := New(Config{Provider: provider, Model: "test"})
	if err := session.Restore(agent); err != nil {
		t.Fatal(err)
	}
	before := agent.Messages()
	if _, err := session.Compact(context.Background(), agent, "", nil); err == nil || !strings.Contains(err.Error(), "did not reduce context") {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(agent.Messages(), before) {
		t.Fatal("rejected candidate replaced active history")
	}
	data, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	var prepared *CompactionInput
	var failed *CompactionState
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record sessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		var event Event
		if record.Type == string(EventCompactionPrepared) || record.Type == string(EventCompactionEnd) {
			if err := json.Unmarshal(record.Payload, &event); err != nil {
				t.Fatal(err)
			}
		}
		if record.Type == string(EventCompactionPrepared) {
			prepared = event.CompactionInput
		}
		if record.Type == string(EventCompactionEnd) && event.IsError {
			failed = event.Compaction
		}
		if record.Type == string(EventContextCompacted) {
			t.Fatal("failed candidate became a checkpoint")
		}
	}
	if prepared == nil || !reflect.DeepEqual(cloneMessages(prepared.Messages), before[:2]) || prepared.UserTurns != 1 || prepared.KeptTurns != 3 || prepared.KeptMessages != 6 || prepared.Chars <= 0 || prepared.Usage.Tokens <= 0 {
		t.Fatalf("scope=%+v", prepared)
	}
	if failed == nil || failed.AfterChars <= failed.BeforeChars || failed.AfterUsage.Tokens <= failed.BeforeUsage.Tokens || failed.Summary == "" || failed.SummaryPath != "" || len(failed.Context.Messages) != 0 {
		t.Fatalf("diagnostics=%+v", failed)
	}
	if strings.Contains(provider.requests[0].Messages[0].Content, "protected one") {
		t.Fatal("protected messages entered summary request")
	}
}

func TestPreparedInputPreservesOriginalToolOutputAndDoesNotAliasHistory(t *testing.T) {
	agent, _ := New(Config{Provider: &compactionProvider{}, Model: "test"})
	agent.messages = compactionHistory()
	original := agent.Messages()
	var input *CompactionInput
	state, err := agent.Compact(context.Background(), "", func(event Event) {
		if event.Type == EventCompactionPrepared {
			input = event.CompactionInput
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if input == nil || !reflect.DeepEqual(input.Messages, original[:4]) || input.Messages[2].Content == state.Context.Messages[2].Content {
		t.Fatal("prepared event lost original scope or included trimmed text")
	}
	input.Messages[0].Content = "observer changed preview"
	if agent.Messages()[0].Content != original[0].Content {
		t.Fatal("preview aliases active history")
	}
}

func TestFullyProtectedHistoryReportsAnEmptyProcessingScope(t *testing.T) {
	agent, _ := New(Config{Provider: &compactionProvider{}, Model: "test"})
	agent.messages = []Message{{Role: RoleUser, Content: "one turn"}, {Role: RoleAssistant, Content: "reply"}}
	var input *CompactionInput
	_, _ = agent.Compact(context.Background(), "", func(event Event) {
		if event.Type == EventCompactionPrepared {
			input = event.CompactionInput
		}
	})
	if input == nil || len(input.Messages) != 0 || input.KeptTurns != 1 || input.KeptMessages != 2 {
		t.Fatalf("scope=%+v", input)
	}
}
