package iota

import (
	"context"
	"encoding/json"
)

// Role 标识消息来源，决定 Provider 如何把消息编码为模型请求。
type Role string

// 三种角色分别对应用户输入、模型回复和工具结果；工具结果必须关联调用 ID。
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 保存模型请求的完整工具调用；Arguments 必须是有效 JSON。
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message 是对话历史中的一项；工具消息通过 ToolCallID 对应助手发起的调用。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
	// ContextSummary marks a checkpoint, not a new user turn.
	ContextSummary bool `json:"context_summary,omitempty"`
	// RuntimeContext exists only in a request, never in persisted conversation history.
	RuntimeContext bool           `json:"runtime_context,omitempty"`
	Source         *MessageSource `json:"source,omitempty"`
	Sources        []SourceRange  `json:"sources,omitempty"`
}

// MessageSource points to an original message_added event in this session.
type MessageSource struct {
	Seq    uint64 `json:"seq"`
	RunID  string `json:"run_id,omitempty"`
	StepID string `json:"step_id,omitempty"`
}

// SourceRange is inclusive; history tools return semantic records inside it.
type SourceRange struct {
	StartSeq uint64 `json:"start_seq"`
	EndSeq   uint64 `json:"end_seq"`
	RunID    string `json:"run_id,omitempty"`
	StepID   string `json:"step_id,omitempty"`
}

// ToolDefinition 是发送给模型的工具元数据；Schema 用 JSON Schema 约束参数。
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Tool 结合模型可见的定义与本地执行函数；执行错误会作为工具结果返回给模型。
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Execute     func(context.Context, json.RawMessage) (string, error)
	// ReadOnly allows this tool during planning; the caller owns this guarantee.
	ReadOnly    bool
	planTool    bool
	historyTool bool
}

// Request 汇集单轮模型请求；Messages 应保持对话和工具结果的原始顺序。
type Request struct {
	Mode         Mode             `json:"mode,omitempty"`
	Model        string           `json:"model"`
	SystemPrompt string           `json:"system_prompt"`
	Messages     []Message        `json:"messages"`
	Tools        []ToolDefinition `json:"tools"`
}

// Usage 记录服务端报告的令牌消耗；服务端可能不返回该信息。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response 是 Provider 汇总后的完整回复，工具调用在执行前还会再次校验。
type Response struct {
	Content    string
	ToolCalls  []ToolCall
	StopReason string
	Usage      *Usage
}

// Delta 表示一段流式更新；RawChunk 留给事件记录器保存原始服务端数据。
type Delta struct {
	Text         string
	Reasoning    string
	ToolCall     *ToolCallDelta
	FinishReason string
	StreamDone   bool
	Usage        *Usage
	RawChunk     string
}

// ToolCallDelta 是工具调用的片段，同一 Index 的名称和参数可能分多次到达。
type ToolCallDelta struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Provider 抽象模型服务；Stream 既返回完整回复，也可逐段通知观察者。
type Provider interface {
	Stream(context.Context, Request, func(Delta)) (Response, error)
}

// RequestEncoder 可选地暴露实际请求体，供会话日志记录发送给服务端的内容。
type RequestEncoder interface {
	EncodeRequest(Request) ([]byte, error)
}

// EventType 区分执行生命周期、模型流和工具调用事件。
type EventType string

// 事件顺序用于还原一次运行；流结束标记与一轮回复完成是不同事件。
const (
	EventRunStart           EventType = "run_start"
	EventTurnStart          EventType = "turn_start"
	EventMessageAdded       EventType = "message_added"
	EventModelRequest       EventType = "model_request"
	EventModelResponse      EventType = "model_response"
	EventModelError         EventType = "model_error"
	EventStreamOther        EventType = "model_stream_other"
	EventStreamFinish       EventType = "model_stream_finish"
	EventStreamDone         EventType = "model_stream_done"
	EventTextDelta          EventType = "text_delta"
	EventReasoningDelta     EventType = "reasoning_delta"
	EventToolCallDelta      EventType = "tool_call_delta"
	EventToolStart          EventType = "tool_start"
	EventToolEnd            EventType = "tool_end"
	EventRunEnd             EventType = "run_end"
	EventModeChanged        EventType = "mode_changed"
	EventPlanSaved          EventType = "plan_saved"
	EventProgressUpdated    EventType = "progress_updated"
	EventPlanApproved       EventType = "plan_approved"
	EventCompactionStart    EventType = "compaction_start"
	EventCompactionPrepared EventType = "compaction_prepared"
	EventCompactionRequest  EventType = "compaction_request"
	EventCompactionDelta    EventType = "compaction_delta"
	EventCompactionEnd      EventType = "compaction_end"
	EventContextCompacted   EventType = "context_compacted"
)

// Event 是观察和会话日志的统一记录；字段按 Type 选择性填充。
type Event struct {
	Type            EventType           `json:"type"`
	Turn            int                 `json:"turn,omitempty"`
	Text            string              `json:"text,omitempty"`
	Reasoning       string              `json:"reasoning,omitempty"`
	RawRequest      string              `json:"raw_request,omitempty"`
	RawChunk        string              `json:"raw_chunk,omitempty"`
	ToolCallDelta   *ToolCallDelta      `json:"tool_call_delta,omitempty"`
	Message         *Message            `json:"message,omitempty"`
	Request         *Request            `json:"request,omitempty"`
	Usage           *Usage              `json:"usage,omitempty"`
	ToolCall        *ToolCall           `json:"tool_call,omitempty"`
	ToolResult      string              `json:"tool_result,omitempty"`
	IsError         bool                `json:"is_error,omitempty"`
	Reason          string              `json:"reason,omitempty"`
	Error           string              `json:"error,omitempty"`
	Collaboration   *CollaborationState `json:"collaboration,omitempty"`
	Compaction      *CompactionState    `json:"compaction,omitempty"`
	CompactionInput *CompactionInput    `json:"compaction_input,omitempty"`
}

// EmitFunc 接收同步发出的事件；回调应尽快返回，避免阻塞执行循环。
type EmitFunc func(Event)

// RunResult 返回本次运行的回复和消息；Messages 不包含运行前已有的历史。
type RunResult struct {
	Text       string
	Messages   []Message
	Turns      int
	StopReason string
}
