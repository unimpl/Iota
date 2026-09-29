package iota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// DefaultMaxTurns 限制单次运行的模型往返次数，避免工具循环无限持续。
const DefaultMaxTurns = 20

// 运行状态错误分别对应并发冲突、轮数限制和不完整的模型输出。
var (
	// ErrBusy 表示同一 Agent 正在运行，不能同时启动新任务或重置历史。
	ErrBusy = errors.New("agent is already running")
	// ErrMaxTurns 表示未得到最终回复就用完了单次运行的轮数。
	ErrMaxTurns = errors.New("maximum turns reached")
	// ErrOutputLength 表示模型回复因输出限制而不完整，不能执行其中的工具调用。
	ErrOutputLength = errors.New("model response was truncated by the output limit")
)

// Config 配置 Agent 的模型、工具和单次运行上限；MaxTurns 为零时使用默认值。
type Config struct {
	Provider     Provider
	Model        string
	SystemPrompt string
	Tools        []Tool
	MaxTurns     int
}

// compiledTool 缓存参数 schema 的编译结果，避免每次工具调用重复编译。
type compiledTool struct {
	tool   Tool
	schema *jsonschema.Schema
}

// Agent 持有对话历史和工具定义；同一实例一次只能执行一个 Run。
// mu 保护运行状态和消息，避免外部读取与执行并发时发生数据竞争。
type Agent struct {
	provider     Provider
	model        string
	systemPrompt string
	tools        []compiledTool
	maxTurns     int

	mu       sync.Mutex
	running  bool
	messages []Message
}

// toolNamePattern 限制发送给模型的工具名，避免服务端不接受特殊字符。
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// New 校验配置并预编译所有工具 schema，让无效工具在运行前失败。
func New(config Config) (*Agent, error) {
	if config.Provider == nil {
		return nil, errors.New("provider is required")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("model is required")
	}
	if config.MaxTurns < 0 {
		return nil, errors.New("max turns cannot be negative")
	}
	if config.MaxTurns == 0 {
		config.MaxTurns = DefaultMaxTurns
	}

	seen := make(map[string]struct{}, len(config.Tools))
	compiled := make([]compiledTool, 0, len(config.Tools))
	for _, tool := range config.Tools {
		if !toolNamePattern.MatchString(tool.Name) {
			return nil, fmt.Errorf("invalid tool name %q", tool.Name)
		}
		if _, ok := seen[tool.Name]; ok {
			return nil, fmt.Errorf("duplicate tool name %q", tool.Name)
		}
		if tool.Execute == nil {
			return nil, fmt.Errorf("tool %q has no execute function", tool.Name)
		}
		if len(tool.Schema) == 0 {
			return nil, fmt.Errorf("tool %q has no schema", tool.Name)
		}
		if err := rejectExternalReferences(tool.Schema); err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", tool.Name, err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(tool.Schema))
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", tool.Name, err)
		}
		compiler := jsonschema.NewCompiler()
		uri := "mem://tools/" + tool.Name + ".json"
		if err := compiler.AddResource(uri, document); err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", tool.Name, err)
		}
		schema, err := compiler.Compile(uri)
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", tool.Name, err)
		}
		seen[tool.Name] = struct{}{}
		compiled = append(compiled, compiledTool{tool: tool, schema: schema})
	}

	return &Agent{
		provider:     config.Provider,
		model:        config.Model,
		systemPrompt: config.SystemPrompt,
		tools:        compiled,
		maxTurns:     config.MaxTurns,
	}, nil
}

// rejectExternalReferences 禁止 schema 通过外部引用加载资源，避免校验时依赖网络或文件。
// 解析后还要确认没有额外的 JSON 内容。
func rejectExternalReferences(schema json.RawMessage) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(schema))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := ensureNoExternalReference(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("schema contains trailing data")
	}
	return nil
}

// ensureNoExternalReference 递归检查对象和数组；仅允许指向当前 schema 的 # 引用。
func ensureNoExternalReference(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("external $ref %q is not allowed", ref)
				}
			}
			if err := ensureNoExternalReference(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := ensureNoExternalReference(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// Messages 返回历史副本，防止调用方修改 Agent 内部保存的消息切片。
func (a *Agent) Messages() []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneMessages(a.messages)
}

// Reset 清空内存历史；运行期间拒绝重置，以免破坏正在构造的模型请求。
func (a *Agent) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return ErrBusy
	}
	a.messages = nil
	return nil
}

// Run 执行一次用户输入，逐轮请求模型并按顺序执行工具，直到模型给出最终回复。
// 同一 Agent 不允许并发 Run；取消时仍为已发出的工具调用补齐结果消息。
func (a *Agent) Run(ctx context.Context, prompt string, emit EmitFunc) (result RunResult, runErr error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return RunResult{}, ErrBusy
	}
	a.running = true
	userMessage := Message{Role: RoleUser, Content: prompt}
	a.messages = append(a.messages, userMessage)
	start := len(a.messages) - 1
	a.mu.Unlock()

	reason := "error"
	emitEvent(emit, Event{Type: EventRunStart})
	emitEvent(emit, Event{Type: EventMessageAdded, Message: cloneMessagePointer(userMessage)})
	// 无论正常结束还是中途报错，都释放运行锁并发出结束事件。
	defer func() {
		a.mu.Lock()
		result.Messages = cloneMessages(a.messages[start:])
		a.running = false
		a.mu.Unlock()
		if runErr == nil && result.StopReason != "" {
			reason = result.StopReason
		} else if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			reason = "aborted"
		}
		end := Event{Type: EventRunEnd, Turn: result.Turns, Reason: reason, IsError: runErr != nil}
		if runErr != nil {
			end.Error = runErr.Error()
		}
		emitEvent(emit, end)
	}()

	definitions := make([]ToolDefinition, len(a.tools))
	for i, tool := range a.tools {
		definitions[i] = ToolDefinition{Name: tool.tool.Name, Description: tool.tool.Description, Schema: append(json.RawMessage(nil), tool.tool.Schema...)}
	}

	for turn := 1; turn <= a.maxTurns; turn++ {
		result.Turns = turn
		if err := ctx.Err(); err != nil {
			return result, err
		}
		emitEvent(emit, Event{Type: EventTurnStart, Turn: turn})
		request := Request{
			Model:        a.model,
			SystemPrompt: a.systemPrompt,
			Messages:     a.Messages(),
			Tools:        definitions,
		}
		// 事件记录使用独立副本，避免观察者意外改动实际发送的请求。
		requestEvent := request
		requestEvent.Messages = cloneMessages(request.Messages)
		requestEvent.Tools = append([]ToolDefinition(nil), request.Tools...)
		for i := range requestEvent.Tools {
			requestEvent.Tools[i].Schema = append(json.RawMessage(nil), request.Tools[i].Schema...)
		}
		requestRecord := Event{Type: EventModelRequest, Turn: turn, Request: &requestEvent}
		if encoder, ok := a.provider.(RequestEncoder); ok {
			body, err := encoder.EncodeRequest(request)
			if err != nil {
				return result, err
			}
			requestRecord.RawRequest = string(body)
		}
		emitEvent(emit, requestRecord)
		response, err := a.provider.Stream(ctx, request, func(delta Delta) {
			if delta.Reasoning != "" {
				emitEvent(emit, Event{Type: EventReasoningDelta, Turn: turn, Reasoning: delta.Reasoning, RawChunk: delta.RawChunk})
			}
			if delta.Text != "" {
				emitEvent(emit, Event{Type: EventTextDelta, Turn: turn, Text: delta.Text, RawChunk: delta.RawChunk})
			}
			if delta.ToolCall != nil {
				call := *delta.ToolCall
				emitEvent(emit, Event{Type: EventToolCallDelta, Turn: turn, ToolCallDelta: &call, RawChunk: delta.RawChunk})
			}
			if delta.FinishReason != "" {
				emitEvent(emit, Event{Type: EventStreamFinish, Turn: turn, Reason: delta.FinishReason, Usage: delta.Usage, RawChunk: delta.RawChunk})
			}
			if delta.StreamDone {
				emitEvent(emit, Event{Type: EventStreamDone, Turn: turn, RawChunk: delta.RawChunk})
			}
			if delta.RawChunk != "" && delta.Reasoning == "" && delta.Text == "" && delta.ToolCall == nil && delta.FinishReason == "" && !delta.StreamDone {
				emitEvent(emit, Event{Type: EventStreamOther, Turn: turn, RawChunk: delta.RawChunk})
			}
		})
		if err != nil {
			return result, err
		}
		if response.StopReason == "length" {
			return result, ErrOutputLength
		}
		// 先检查整批调用，再执行其中任何一个，避免部分执行后才发现无效调用。
		if err := validateToolCalls(response.ToolCalls); err != nil {
			return result, fmt.Errorf("invalid provider response: %w", err)
		}
		responseEvent := Event{Type: EventModelResponse, Turn: turn, Reason: response.StopReason}
		if response.Usage != nil {
			usage := *response.Usage
			responseEvent.Usage = &usage
		}
		emitEvent(emit, responseEvent)

		assistant := Message{Role: RoleAssistant, Content: response.Content, ToolCalls: cloneToolCalls(response.ToolCalls)}
		a.appendMessage(assistant, turn, emit)
		if len(response.ToolCalls) == 0 {
			result.Text = response.Content
			result.StopReason = response.StopReason
			if result.StopReason == "" {
				result.StopReason = "stop"
			}
			return result, nil
		}

		for index, call := range response.ToolCalls {
			if err := ctx.Err(); err != nil {
				a.appendCancelledResults(response.ToolCalls[index:], turn, emit)
				return result, err
			}
			emitEvent(emit, Event{Type: EventToolStart, Turn: turn, ToolCall: cloneToolCallPointer(call)})
			text, isError := a.executeTool(ctx, call)
			toolMessage := Message{Role: RoleTool, Content: text, ToolCallID: call.ID, ToolName: call.Name, IsError: isError}
			a.appendMessage(toolMessage, turn, emit)
			emitEvent(emit, Event{Type: EventToolEnd, Turn: turn, ToolCall: cloneToolCallPointer(call), ToolResult: text, IsError: isError})
			if err := ctx.Err(); err != nil {
				a.appendCancelledResults(response.ToolCalls[index+1:], turn, emit)
				return result, err
			}
		}
	}
	result.StopReason = "max_turns"
	return result, ErrMaxTurns
}

// validateToolCalls 检查调用 ID、名称和 JSON 参数；重复 ID 会破坏结果配对。
func validateToolCalls(calls []ToolCall) error {
	seen := make(map[string]struct{}, len(calls))
	for index, call := range calls {
		if call.ID == "" {
			return fmt.Errorf("tool call %d has no ID", index)
		}
		if _, exists := seen[call.ID]; exists {
			return fmt.Errorf("duplicate tool call ID %q", call.ID)
		}
		if call.Name == "" {
			return fmt.Errorf("tool call %q has no name", call.ID)
		}
		if !json.Valid(call.Arguments) {
			return fmt.Errorf("tool call %q has invalid JSON arguments", call.ID)
		}
		seen[call.ID] = struct{}{}
	}
	return nil
}

// executeTool 校验参数后执行指定工具，并将失败转成模型可见的错误文本。
// 返回的布尔值表示工具错误，不表示整个 Agent 运行失败。
func (a *Agent) executeTool(ctx context.Context, call ToolCall) (string, bool) {
	var selected *compiledTool
	for i := range a.tools {
		if a.tools[i].tool.Name == call.Name {
			selected = &a.tools[i]
			break
		}
	}
	if selected == nil {
		return fmt.Sprintf("tool %q not found", call.Name), true
	}
	var args any
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return "invalid tool arguments: " + err.Error(), true
	}
	if err := selected.schema.Validate(args); err != nil {
		return "invalid tool arguments: " + err.Error(), true
	}
	text, err := selected.tool.Execute(ctx, call.Arguments)
	if err != nil {
		return err.Error(), true
	}
	return text, false
}

// appendMessage 同时更新对话历史和事件流，保证日志能还原模型看到的消息顺序。
func (a *Agent) appendMessage(message Message, turn int, emit EmitFunc) {
	a.mu.Lock()
	a.messages = append(a.messages, message)
	a.mu.Unlock()
	emitEvent(emit, Event{Type: EventMessageAdded, Turn: turn, Message: cloneMessagePointer(message)})
}

// appendCancelledResults 为尚未执行的调用补上取消结果，维持调用与工具消息一一对应。
func (a *Agent) appendCancelledResults(calls []ToolCall, turn int, emit EmitFunc) {
	for _, call := range calls {
		a.appendMessage(Message{Role: RoleTool, Content: "operation canceled before tool execution", ToolCallID: call.ID, ToolName: call.Name, IsError: true}, turn, emit)
	}
}

// emitEvent 允许调用方不提供观察回调，不改变 Agent 的执行逻辑。
func emitEvent(emit EmitFunc, event Event) {
	if emit != nil {
		emit(event)
	}
}

// cloneMessages 复制消息及其工具调用，防止外部修改内部历史。
func cloneMessages(messages []Message) []Message {
	result := make([]Message, len(messages))
	for i, message := range messages {
		result[i] = message
		result[i].ToolCalls = cloneToolCalls(message.ToolCalls)
	}
	return result
}

// cloneToolCalls 还会复制原始 JSON 字节，避免仅复制切片头造成共享。
func cloneToolCalls(calls []ToolCall) []ToolCall {
	result := make([]ToolCall, len(calls))
	for i, call := range calls {
		result[i] = call
		result[i].Arguments = append(json.RawMessage(nil), call.Arguments...)
	}
	return result
}

// cloneToolCallPointer 为事件创建独立调用对象，避免观察者改动原参数。
func cloneToolCallPointer(call ToolCall) *ToolCall {
	copy := call
	copy.Arguments = append(json.RawMessage(nil), call.Arguments...)
	return &copy
}

// cloneMessagePointer 为事件创建独立消息对象，避免观察者改动历史。
func cloneMessagePointer(message Message) *Message {
	copy := message
	copy.ToolCalls = cloneToolCalls(message.ToolCalls)
	return &copy
}
