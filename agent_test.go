package iota

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type fakeProvider struct {
	mu        sync.Mutex
	responses []Response
	requests  []Request
	block     <-chan struct{}
}

func (p *fakeProvider) Stream(ctx context.Context, request Request, emit func(Delta)) (Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	if len(p.responses) == 0 {
		p.mu.Unlock()
		if p.block != nil {
			select {
			case <-p.block:
				return Response{Content: "done", StopReason: "stop"}, nil
			case <-ctx.Done():
				return Response{}, ctx.Err()
			}
		}
		return Response{}, errors.New("no fake response")
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	p.mu.Unlock()
	if response.Content != "" {
		emit(Delta{Text: response.Content})
	}
	return response, nil
}

func TestAgentDirectAnswer(t *testing.T) {
	provider := &fakeProvider{responses: []Response{{Content: "hello", StopReason: "stop"}}}
	agent, err := New(Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var events []EventType
	result, err := agent.Run(context.Background(), "hi", func(event Event) { events = append(events, event.Type) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello" || result.Turns != 1 || result.StopReason != "stop" {
		t.Fatalf("unexpected result: %+v", result)
	}
	want := []EventType{EventRunStart, EventMessageAdded, EventTurnStart, EventModelRequest, EventTextDelta, EventModelResponse, EventMessageAdded, EventRunEnd}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestAgentToolLoopAndErrors(t *testing.T) {
	calls := []ToolCall{
		{ID: "one", Name: "echo", Arguments: json.RawMessage(`{"value":"a"}`)},
		{ID: "two", Name: "missing", Arguments: json.RawMessage(`{}`)},
		{ID: "three", Name: "echo", Arguments: json.RawMessage(`{"value":3}`)},
	}
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: calls, StopReason: "tool_calls"},
		{Content: "fixed", StopReason: "stop"},
	}}
	tool := Tool{
		Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`),
		Execute: func(_ context.Context, raw json.RawMessage) (string, error) { return string(raw), nil },
	}
	agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "fixed" || result.Turns != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	messages := agent.Messages()
	if len(messages) != 6 {
		t.Fatalf("got %d messages: %+v", len(messages), messages)
	}
	for i, id := range []string{"one", "two", "three"} {
		message := messages[i+2]
		if message.ToolCallID != id {
			t.Fatalf("message %d call ID = %q", i, message.ToolCallID)
		}
	}
	if messages[2].IsError || !messages[3].IsError || !messages[4].IsError {
		t.Fatalf("unexpected error states: %+v", messages[2:5])
	}
}

func TestAgentRejectsConcurrentRun(t *testing.T) {
	block := make(chan struct{})
	provider := &fakeProvider{block: block}
	agent, err := New(Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := agent.Run(context.Background(), "first", nil)
		done <- err
	}()
	for {
		provider.mu.Lock()
		started := len(provider.requests) > 0
		provider.mu.Unlock()
		if started {
			break
		}
	}
	if _, err := agent.Run(context.Background(), "second", nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
	if err := agent.Reset(); !errors.Is(err, ErrBusy) {
		t.Fatalf("reset got %v, want ErrBusy", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAgentMaxTurnsAndLength(t *testing.T) {
	provider := &fakeProvider{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "1", Name: "missing", Arguments: json.RawMessage(`{}`)}}},
		{ToolCalls: []ToolCall{{ID: "2", Name: "missing", Arguments: json.RawMessage(`{}`)}}},
	}}
	agent, _ := New(Config{Provider: provider, Model: "test", MaxTurns: 2})
	result, err := agent.Run(context.Background(), "go", nil)
	if !errors.Is(err, ErrMaxTurns) || result.StopReason != "max_turns" {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	lengthProvider := &fakeProvider{responses: []Response{{StopReason: "length", ToolCalls: []ToolCall{{ID: "x", Name: "missing", Arguments: json.RawMessage(`{}`)}}}}}
	lengthAgent, _ := New(Config{Provider: lengthProvider, Model: "test"})
	_, err = lengthAgent.Run(context.Background(), "go", nil)
	if !errors.Is(err, ErrOutputLength) || len(lengthAgent.Messages()) != 1 {
		t.Fatalf("length error=%v messages=%+v", err, lengthAgent.Messages())
	}
}

func TestAgentCancellationCompletesToolResults(t *testing.T) {
	provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{
		{ID: "1", Name: "wait", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "wait", Arguments: json.RawMessage(`{}`)},
	}}}}
	started := make(chan struct{})
	tool := Tool{Name: "wait", Schema: json.RawMessage(`{"type":"object"}`), Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		return "", ctx.Err()
	}}
	agent, _ := New(Config{Provider: provider, Model: "test", Tools: []Tool{tool}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := agent.Run(ctx, "go", nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	messages := agent.Messages()
	if len(messages) != 4 || messages[2].ToolCallID != "1" || messages[3].ToolCallID != "2" || !messages[2].IsError || !messages[3].IsError {
		t.Fatalf("messages are not paired after cancellation: %+v", messages)
	}
}

func TestNewRejectsExternalSchemaReference(t *testing.T) {
	_, err := New(Config{Provider: &fakeProvider{}, Model: "test", Tools: []Tool{{
		Name: "bad", Schema: json.RawMessage(`{"$ref":"https://example.com/schema"}`), Execute: func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}}})
	if err == nil {
		t.Fatal("expected external reference error")
	}
}

func TestAgentRejectsMalformedToolCallsBeforeExecution(t *testing.T) {
	executed := false
	provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{
		{ID: "same", Name: "tool", Arguments: json.RawMessage(`{}`)},
		{ID: "same", Name: "tool", Arguments: json.RawMessage(`{}`)},
	}}}}
	tool := Tool{Name: "tool", Schema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) {
		executed = true
		return "", nil
	}}
	agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), "go", nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate tool call ID") {
		t.Fatalf("got %v", err)
	}
	if executed || len(agent.Messages()) != 1 {
		t.Fatalf("executed=%t messages=%+v", executed, agent.Messages())
	}
}
