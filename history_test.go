package iota

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHistoryToolsReadOriginalRecordsAfterCompactionAndResume(t *testing.T) {
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, message := range compactionHistory() {
		if err := session.write("seed", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: cloneMessagePointer(message)}); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := New(Config{Provider: &compactionProvider{}, Model: "test", Tools: []Tool{NewSearchHistoryTool(), NewReadHistoryTool()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.activeToolDefinitions()) != 0 {
		t.Fatal("history tools were declared without a session")
	}
	if err := session.Restore(agent); err != nil {
		t.Fatal(err)
	}
	if len(agent.activeToolDefinitions()) != 2 {
		t.Fatal("history tools missing after restore")
	}
	pruned, err := session.Compact(t.Context(), agent, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.Stage != "tool_results" || !strings.Contains(pruned.Context.Messages[2].Content, "seq 4-4") {
		t.Fatal("pruned tool result omitted its source pointer")
	}
	state, err := session.Compact(t.Context(), agent, "focus", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sources) == 0 || len(state.Units) == 0 || !strings.Contains(state.Context.Messages[0].Content, "<source-index>") {
		t.Fatal("summary source index missing")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if err := resumed.Restore(agent); err != nil {
		t.Fatal(err)
	}
	text, err := agent.executeHistoryTool(t.Context(), ToolCall{Name: "search_history", Arguments: json.RawMessage(`{"query":"IMPORTANT TAIL"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var search historyPage
	if err := json.Unmarshal([]byte(text), &search); err != nil {
		t.Fatal(err)
	}
	if len(search.Records) != 1 || search.Records[0].Source.Seq != 4 || search.Records[0].Role != RoleTool {
		t.Fatal("search did not return the original tool result")
	}
	text, err = agent.executeHistoryTool(t.Context(), ToolCall{Name: "read_history", Arguments: json.RawMessage(`{"start_seq":3,"end_seq":3}`)})
	if err != nil {
		t.Fatal(err)
	}
	var read historyPage
	if err := json.Unmarshal([]byte(text), &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Records) != 1 || !strings.Contains(read.Records[0].Content, `"path":"old.go"`) {
		t.Fatal("original tool call arguments missing")
	}
}

func TestHistoryPaginationReturnsWholeUnicodeRecordsWithinByteBudget(t *testing.T) {
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	original := strings.Repeat("中文\x01\"\\", 16000)
	if err := session.write("run", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: &Message{Role: RoleUser, Content: original}}); err != nil {
		t.Fatal(err)
	}
	args := historyArguments{StartSeq: 2, EndSeq: 2}
	var reconstructed strings.Builder
	for pages := 0; pages < 100; pages++ {
		text, err := session.queryHistory(t.Context(), args, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(text) > historyMaxBytes {
			t.Fatalf("response exceeds byte budget: %d", len(text))
		}
		var page historyPage
		if err := json.Unmarshal([]byte(text), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 1 || page.Records[0].Offset != args.Offset {
			t.Fatal("invalid continuation offset")
		}
		reconstructed.WriteString(page.Records[0].Content)
		if page.NextSeq == 0 {
			break
		}
		if page.NextOffset <= args.Offset {
			t.Fatal("pagination did not advance")
		}
		args.StartSeq, args.Offset = page.NextSeq, page.NextOffset
	}
	if reconstructed.String() != original {
		t.Fatal("paged history lost or repeated Unicode characters")
	}
}

func TestHistoryResetScopeAndSearchCursor(t *testing.T) {
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, word := range []string{"needle old", "needle second"} {
		if err := session.write("before", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: &Message{Role: RoleUser, Content: word}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.write("", "session_reset", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.write("after", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: &Message{Role: RoleUser, Content: "needle new"}}); err != nil {
		t.Fatal(err)
	}
	for _, include := range []bool{false, true} {
		args := historyArguments{Query: "needle", Limit: 1, IncludeBeforeReset: include}
		count := 0
		for {
			text, err := session.queryHistory(context.Background(), args, true)
			if err != nil {
				t.Fatal(err)
			}
			var page historyPage
			if err := json.Unmarshal([]byte(text), &page); err != nil {
				t.Fatal(err)
			}
			count += len(page.Records)
			if page.NextSeq == 0 {
				break
			}
			args.StartSeq = page.NextSeq
		}
		want := 1
		if include {
			want = 3
		}
		if count != want {
			t.Fatalf("include_before_reset=%v returned %d records", include, count)
		}
	}
}
