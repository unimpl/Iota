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

// TestSessionRoundTrip 验证保存、恢复、继续追加，以及恢复时不重复执行工具。
func TestSessionRoundTrip(t *testing.T) {
	executions := 0
	tool := Tool{Name: "echo", Schema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) {
		executions++
		return "tool result", nil
	}}
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "call", Name: "echo", Arguments: json.RawMessage(`{}`)}}, StopReason: "tool_calls"},
		{Content: "answer", StopReason: "stop"},
	}}
	agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test", CWD: "/project"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Run(context.Background(), agent, "question", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(session.Path())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("info=%v err=%v", info, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	nextProvider := &fakeProvider{responses: []Response{{Content: "next answer", StopReason: "stop"}}}
	next, err := New(Config{Provider: nextProvider, Model: "test", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Restore(next); err != nil {
		t.Fatal(err)
	}
	if executions != 1 || !reflect.DeepEqual(next.Messages(), agent.Messages()) {
		t.Fatalf("executions=%d restored=%+v", executions, next.Messages())
	}
	if _, err := resumed.Run(context.Background(), next, "follow up", nil); err != nil {
		t.Fatal(err)
	}
	want := cloneMessages(append(agent.Messages(), Message{Role: RoleUser, Content: "follow up"}))
	if !reflect.DeepEqual(nextProvider.requests[0].Messages, want) {
		t.Fatalf("request=%+v, want %+v", nextProvider.requests[0].Messages, want)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	for index, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record sessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Sequence != uint64(index+1) || record.SessionID != session.id {
			t.Fatalf("record=%+v", record)
		}
		if record.Type == string(EventRunStart) && record.RunID == "" {
			t.Fatal("run ID is missing")
		}
	}
}

// TestSessionReset 验证重置后的历史不会在重新打开时恢复。
func TestSessionReset(t *testing.T) {
	agent, err := New(Config{Provider: &fakeProvider{responses: []Response{{Content: "answer"}}}, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Run(context.Background(), agent, "question", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Reset(agent); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if err := resumed.Restore(agent); err != nil || len(agent.Messages()) != 0 {
		t.Fatalf("history=%+v err=%v", agent.Messages(), err)
	}
}

// TestSessionInterruptedTools 验证完整结果保留，缺失结果补为错误，流式文本不变成历史。
func TestSessionInterruptedTools(t *testing.T) {
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	messages := []Message{
		{Role: RoleUser, Content: "question"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "one", Name: "echo", Arguments: json.RawMessage(`{}`)},
			{ID: "two", Name: "echo", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: RoleTool, ToolCallID: "one", ToolName: "echo", Content: "saved result"},
	}
	for _, message := range messages {
		if err := session.write("run", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: &message}); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.write("run", string(EventTextDelta), Event{Type: EventTextDelta, Text: "partial"}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if len(resumed.messages) != 4 || resumed.messages[2].Content != "saved result" ||
		resumed.messages[3].ToolCallID != "two" || !resumed.messages[3].IsError {
		t.Fatalf("history=%+v", resumed.messages)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !reflect.DeepEqual(cloneMessages(again.messages), cloneMessages(resumed.messages)) {
		t.Fatal("interrupted tool repair was not saved")
	}
}

// TestOpenSessionRejectsCorruption 验证无效日志报错且原文件保持原样。
func TestOpenSessionRejectsCorruption(t *testing.T) {
	header := `{"version":1,"seq":1,"session_id":"id","type":"session_start"}` + "\n"
	for name, data := range map[string]string{
		"empty":           "",
		"truncated":       header + `{"version":1`,
		"version":         strings.Replace(header, `"version":1`, `"version":2`, 1),
		"sequence":        strings.Replace(header, `"seq":1`, `"seq":2`, 1),
		"changed ID":      header + `{"version":1,"seq":2,"session_id":"other","type":"session_end"}` + "\n",
		"missing message": header + `{"version":1,"seq":2,"session_id":"id","type":"message_added","payload":{"type":"message_added"}}` + "\n",
		"invalid role":    header + `{"version":1,"seq":2,"session_id":"id","type":"message_added","payload":{"type":"message_added","message":{"role":"invalid"}}}` + "\n",
		"orphan result":   header + `{"version":1,"seq":2,"session_id":"id","type":"message_added","payload":{"type":"message_added","message":{"role":"tool","tool_call_id":"missing"}}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := t.TempDir() + "/session.jsonl"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if session, err := OpenSession(path); err == nil {
				session.Close()
				t.Fatal("expected invalid session error")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatalf("original file changed: %q err=%v", got, err)
			}
		})
	}
}

// TestSessionBusyAndClosed 验证运行中不能恢复、重置或关闭，关闭后不能调用模型。
func TestSessionBusyAndClosed(t *testing.T) {
	block := make(chan struct{})
	provider := &fakeProvider{block: block}
	agent, err := New(Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := session.Run(ctx, agent, "question", func(event Event) {
			if event.Type == EventRunStart {
				close(started)
			}
		})
		done <- err
	}()
	<-started
	for _, operation := range []func() error{func() error { return session.Restore(agent) }, func() error { return session.Reset(agent) }, session.Close} {
		if err := operation(); !errors.Is(err, ErrBusy) {
			t.Fatalf("got %v, want ErrBusy", err)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(context.Background(), agent, "later", nil); err == nil {
		t.Fatal("closed session accepted run")
	}
}

// TestSessionReportsWriteFailure 验证保存失败作为运行错误返回给 SDK 调用方。
func TestSessionReportsWriteFailure(t *testing.T) {
	provider := &fakeProvider{responses: []Response{{Content: "answer"}}}
	agent, err := New(Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(context.Background(), agent, "question", nil); err == nil {
		t.Fatal("write failure was ignored")
	}
	if len(provider.requests) != 0 {
		t.Fatal("provider was called after saving failed")
	}
}
