package iota

import (
	"context"
	"encoding/json"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Execute     func(context.Context, json.RawMessage) (string, error)
}

type Request struct {
	Model        string
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDefinition
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Response struct {
	Content    string
	ToolCalls  []ToolCall
	StopReason string
	Usage      *Usage
}

type Delta struct {
	Text string
}

type Provider interface {
	Stream(context.Context, Request, func(Delta)) (Response, error)
}

type EventType string

const (
	EventRunStart  EventType = "run_start"
	EventTurnStart EventType = "turn_start"
	EventTextDelta EventType = "text_delta"
	EventToolStart EventType = "tool_start"
	EventToolEnd   EventType = "tool_end"
	EventRunEnd    EventType = "run_end"
)

type Event struct {
	Type       EventType
	Turn       int
	Text       string
	ToolCall   *ToolCall
	ToolResult string
	IsError    bool
	Reason     string
}

type EmitFunc func(Event)

type RunResult struct {
	Text       string
	Messages   []Message
	Turns      int
	StopReason string
}
