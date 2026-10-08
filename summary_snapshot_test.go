package iota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func snapshotSession(t *testing.T, provider Provider) (*Session, *Agent) {
	t.Helper()
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	for _, message := range compactionHistory() {
		if err := session.write("seed", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: cloneMessagePointer(message)}); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := New(Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Restore(agent); err != nil {
		t.Fatal(err)
	}
	return session, agent
}

func TestSummarySnapshotsUseCheckpointSequenceAndSkipRepeat(t *testing.T) {
	provider := &compactionProvider{}
	session, agent := snapshotSession(t, provider)
	dir := t.TempDir()
	if err := session.SetSummaryDir(dir); err != nil {
		t.Fatal(err)
	}
	agent.contextLimitTokens = 128000
	first, err := session.Compact(context.Background(), agent, "保留未完成事项", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, strings.TrimSuffix(filepath.Base(session.Path()), ".jsonl")+fmt.Sprintf(".summary.%d.md", first.EventID))
	if first.EventID == 0 || first.SummaryPath != want {
		t.Fatalf("path=%s want=%s id=%d", first.SummaryPath, want, first.EventID)
	}
	content, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{first.Summary, "保留未完成事项", "- Trigger: manual", fmt.Sprintf("- Checkpoint event: %d", first.EventID), "approximately", "- Reported capacity: 128000 tokens"} {
		if !strings.Contains(string(content), text) {
			t.Fatalf("snapshot missing %q", text)
		}
	}
	info, err := os.Stat(want)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions=%v err=%v", info, err)
	}
	journal, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	var checkpointFound bool
	for _, line := range strings.Split(strings.TrimSpace(string(journal)), "\n") {
		var record sessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Type != string(EventContextCompacted) {
			continue
		}
		var event Event
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if record.Sequence != event.Compaction.EventID || event.Compaction.SummaryPath != want {
			t.Fatal("snapshot ID differs from checkpoint sequence")
		}
		checkpointFound = true
	}
	if !checkpointFound {
		t.Fatal("checkpoint missing")
	}
	if _, err := session.Compact(context.Background(), agent, "", nil); !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("repeat err=%v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatal("unchanged context was summarized again")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.summary.*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if _, err := session.Run(context.Background(), agent, "next user request", nil); err != nil {
		t.Fatal(err)
	}
	second, err := session.Compact(context.Background(), agent, "保留新的进度", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.EventID <= first.EventID || second.SummaryPath == first.SummaryPath {
		t.Fatal("new summary overwrote the earlier snapshot")
	}
	unchanged, err := os.ReadFile(first.SummaryPath)
	if err != nil || string(unchanged) != string(content) {
		t.Fatal("first snapshot changed")
	}
	files, _ = filepath.Glob(filepath.Join(dir, "*.summary.*.md"))
	if len(files) != 2 {
		t.Fatalf("files=%v", files)
	}
}

func TestToolPruningHasUsageButNoMarkdownSnapshot(t *testing.T) {
	session, agent := snapshotSession(t, &compactionProvider{})
	agent.contextLimitTokens = 80000
	var start *CompactionState
	state, err := session.Compact(context.Background(), agent, "", func(event Event) {
		if event.Type == EventCompactionStart {
			start = event.Compaction
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != "tool_results" || state.SummaryPath != "" || state.EventID == 0 {
		t.Fatalf("stage=%s path=%s id=%d", state.Stage, state.SummaryPath, state.EventID)
	}
	if start == nil || start.BeforeUsage.Tokens <= 0 || start.AfterUsage.Tokens != 0 {
		t.Fatal("start metrics mutated after publication")
	}
	if state.BeforeUsage.Tokens <= state.AfterUsage.Tokens || state.AfterUsage.RemainingPercent == nil || *state.AfterUsage.RemainingPercent <= *state.BeforeUsage.RemainingPercent {
		t.Fatalf("usage=%+v → %+v", state.BeforeUsage, state.AfterUsage)
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(session.Path()), "*.summary.*.md"))
	if len(files) != 0 {
		t.Fatalf("pruning wrote summaries: %v", files)
	}
}

func TestAutomaticSummarySnapshotRecordsTriggeringRun(t *testing.T) {
	provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if request.SystemPrompt == compactionPrompt {
			return Response{Content: "Old history checkpoint", StopReason: "stop"}, nil
		}
		if !request.Messages[0].ContextSummary {
			return Response{}, errors.New("maximum context length is 16000 tokens")
		}
		return Response{Content: "recovered", StopReason: "stop"}, nil
	}}
	session, agent := snapshotSession(t, provider)
	var state *CompactionState
	if _, err := session.Run(context.Background(), agent, "continue", func(event Event) {
		if event.Type == EventContextCompacted && event.Compaction.Stage == "summary" {
			state = event.Compaction
		}
	}); err != nil {
		t.Fatal(err)
	}
	if state == nil || state.SummaryPath == "" || state.AfterUsage.RemainingPercent == nil {
		t.Fatal("automatic summary missing file or usage")
	}
	data, err := os.ReadFile(state.SummaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- Trigger: overflow") || !strings.Contains(string(data), "- Triggering run:") {
		t.Fatal("automatic snapshot omitted trigger metadata")
	}
}

func TestSnapshotWriteFailurePreservesContext(t *testing.T) {
	session, agent := snapshotSession(t, &compactionProvider{})
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := session.SetSummaryDir(blocked); err != nil {
		t.Fatal(err)
	}
	before := agent.Messages()
	if _, err := session.Compact(context.Background(), agent, "focus", nil); err == nil {
		t.Fatal("expected export failure")
	}
	if !reflect.DeepEqual(before, agent.Messages()) || !reflect.DeepEqual(before, session.messages) {
		t.Fatal("failed snapshot replaced context")
	}
	journal, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), `"type":"context_compacted"`) {
		t.Fatal("failed snapshot committed a checkpoint")
	}
}

func TestSnapshotRollbackAndExistingFileProtection(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprint(collision), func(t *testing.T) {
			session, agent := snapshotSession(t, &compactionProvider{})
			state, err := agent.Compact(context.Background(), "focus", nil)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(filepath.Dir(session.Path()), strings.TrimSuffix(filepath.Base(session.Path()), ".jsonl")+fmt.Sprintf(".summary.%d.md", session.seq+1))
			if collision {
				if err := os.WriteFile(path, []byte("existing snapshot"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				session.file.Close()
			}
			if err := session.recordState("", Event{Type: EventContextCompacted, Compaction: &state}); err == nil {
				t.Fatal("expected persistence error")
			}
			data, err := os.ReadFile(path)
			if collision {
				if err != nil || string(data) != "existing snapshot" {
					t.Fatal("existing snapshot overwritten or removed")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("uncommitted snapshot was not removed")
			}
		})
	}
}

func TestContextUsageUnknownCapacityAndOverLimit(t *testing.T) {
	request := Request{Model: "test", Mode: ModeDefault, SystemPrompt: "项目规则", Messages: []Message{{Role: RoleUser, Content: strings.Repeat("中文", 100)}}}
	unknown := estimateContextUsage(request, 0)
	if unknown.Tokens <= 0 || !unknown.Estimated || unknown.RemainingPercent != nil {
		t.Fatalf("unknown=%+v", unknown)
	}
	full := estimateContextUsage(request, 1)
	if full.RemainingPercent == nil || *full.RemainingPercent != 0 {
		t.Fatalf("full=%+v", full)
	}
	available := estimateContextUsage(request, 10000)
	if available.RemainingPercent == nil || *available.RemainingPercent <= 0 || *available.RemainingPercent >= 100 {
		t.Fatalf("available=%+v", available)
	}
}
