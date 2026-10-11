package iota

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestInvalidStructuredSummaryPreservesHistory(t *testing.T) {
	for name, content := range map[string]string{
		"not_json":         `plain summary`,
		"missing":          `{"entries":[]}`,
		"unknown_ref":      `{"entries":[{"id":"cache","section":"context","text":"cache","status":"active","refs":["turn:invented"]}]}`,
		"duplicate_id":     `{"entries":[{"id":"cache","section":"context","text":"one","status":"active","refs":["turn:t0"]},{"id":"cache","section":"context","text":"two","status":"active","refs":["turn:t1"]}]}`,
		"blank_text":       `{"entries":[{"id":"cache","section":"context","text":" ","status":"active","refs":["turn:t0"]}]}`,
		"invented_address": `{"entries":[{"id":"cache","section":"context","text":"cache","status":"active","refs":["turn:t0"],"start_seq":999}]}`,
		"trailing_data":    `{"entries":[{"id":"cache","section":"context","text":"cache","status":"active","refs":["turn:t0"]}]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			provider := &compactionProvider{rawSummary: true, respond: func(context.Context, Request, int) (Response, error) {
				return Response{Content: content, StopReason: "stop"}, nil
			}}
			session, agent := snapshotSession(t, provider)
			before := agent.Messages()
			if _, err := session.Compact(t.Context(), agent, "focus", nil); err == nil {
				t.Fatal("invalid response was accepted")
			}
			if !reflect.DeepEqual(agent.Messages(), before) || !reflect.DeepEqual(session.messages, before) {
				t.Fatal("invalid metadata replaced active history")
			}
		})
	}
}

func TestDescribedSourcesSurviveRepeatedCompactionAndHistoryLookup(t *testing.T) {
	const description = "read old.go: previous source contents and important tail; inspect original output before implementing."
	provider := &compactionProvider{rawSummary: true, respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if request.SystemPrompt != compactionPrompt {
			return Response{Content: "done", StopReason: "stop"}, nil
		}
		_, rest, _ := strings.Cut(request.Messages[0].Content, "<turns>\n")
		mapping, _, _ := strings.Cut(rest, "\n</turns>")
		var targets []turnInput
		if err := json.Unmarshal([]byte(mapping), &targets); err != nil {
			t.Fatal(err)
		}
		result := structuredOutput{}
		_, rest, _ = strings.Cut(request.Messages[0].Content, "<previous-summary>\n")
		prior, _, _ := strings.Cut(rest, "\n</previous-summary>")
		var previous []SummaryEntry
		if err := json.Unmarshal([]byte(prior), &previous); err != nil {
			t.Fatal(err)
		}
		for _, entry := range previous {
			result.Entries = append(result.Entries, entryOutput{ID: entry.ID, Section: entry.Section, Text: entry.Text, Status: entry.Status, Refs: []string{"summary:" + entry.ID}})
		}
		refs := make([]string, len(targets))
		for i, target := range targets {
			refs[i] = "turn:" + target.ID
		}
		result.Entries = append(result.Entries, entryOutput{ID: fmt.Sprintf("lookup-%d", len(previous)), Section: "context", Text: description, Status: "active", Refs: refs})
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return Response{Content: string(data), StopReason: "stop"}, nil
	}}
	session, agent := snapshotSession(t, provider)
	first, err := session.Compact(t.Context(), agent, "focus", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agent.Messages()[0].Content, description) {
		t.Fatal("first summary did not expose the lookup hint")
	}
	if _, err := session.Run(t.Context(), agent, strings.Repeat("later context ", 300), nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := session.Run(t.Context(), agent, fmt.Sprintf("later %d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	second, err := session.Compact(t.Context(), agent, "summarize new turns", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Sources) <= len(first.Sources) || !strings.Contains(agent.Messages()[0].Content, description) {
		t.Fatal("later compaction lost earlier turn sources")
	}
	for _, original := range first.Sources {
		found := false
		for _, source := range second.Sources {
			if source == original {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("later compaction lost an original source address")
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
	fresh, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{NewReadHistoryTool()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Restore(fresh); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Messages()[0].Sources, second.Sources) {
		t.Fatal("descriptions did not survive session restore")
	}
	source := fresh.Messages()[0].Sources[0]
	arguments, err := json.Marshal(historyArguments{StartSeq: source.StartSeq, EndSeq: source.EndSeq, RunID: source.RunID})
	if err != nil {
		t.Fatal(err)
	}
	text, err := fresh.executeHistoryTool(t.Context(), ToolCall{Name: "read_history", Arguments: arguments})
	if err != nil || !strings.Contains(text, "old.go") {
		t.Fatalf("described address did not locate the original records: %v", err)
	}
	markdown, err := os.ReadFile(second.SummaryPath)
	if err != nil || !strings.Contains(string(markdown), description) {
		t.Fatalf("summary snapshot omitted descriptions: %v", err)
	}
}

func TestCheckpointSourcesBoundVisibleIndexWithoutDroppingStoredDescriptions(t *testing.T) {
	var sources []SourceRange
	for i := 0; i < 40; i++ {
		sources = append(sources, SourceRange{StartSeq: uint64(i*2 + 1), EndSeq: uint64(i*2 + 2), RunID: fmt.Sprint(i), Description: fmt.Sprintf("topic-%d", i)})
	}
	visible := checkpointSources(sources)
	if len(visible) != 32 || visible[0].StartSeq != 1 || visible[0].EndSeq != 18 || !strings.Contains(visible[0].Description, "search_history") {
		t.Fatalf("invalid bounded lookup index: %+v", visible)
	}
	message := Message{Role: RoleUser, ContextSummary: true, Sources: sources, Content: sourceIndex(visible)}
	if !reflect.DeepEqual(sourceRanges([]Message{message}), sources) || !strings.Contains(message.Content, "topic-39") {
		t.Fatal("visible index compaction discarded inherited descriptions")
	}
}

func TestSourceRangesDoNotMergeDifferentLookupTopics(t *testing.T) {
	first := SourceRange{StartSeq: 1, EndSeq: 4, RunID: "run", StepID: "step", Description: "User selected cache location A."}
	second := SourceRange{StartSeq: 5, EndSeq: 9, RunID: "run", StepID: "step", Description: "read cache.go; migration rules and verification pending."}
	got := sourceRanges([]Message{{Sources: []SourceRange{first, second}}})
	if !reflect.DeepEqual(got, []SourceRange{first, second}) {
		t.Fatal("different topics were collapsed into an ambiguous address")
	}
}
