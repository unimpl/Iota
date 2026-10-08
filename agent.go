package iota

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// 运行状态错误分别对应并发冲突、轮数限制和不完整的模型输出。
var (
	// ErrBusy 表示同一 Agent 正在运行，不能同时启动新任务或重置历史。
	ErrBusy = errors.New("agent is already running")
	// ErrMaxTurns 表示未得到最终回复就用完了单次运行的轮数。
	ErrMaxTurns = errors.New("maximum turns reached")
	// ErrOutputLength 表示模型回复因输出限制而不完整，不能执行其中的工具调用。
	ErrOutputLength = errors.New("model response was truncated by the output limit")
)

// Agent 持有对话历史和工具定义；同一实例一次只能执行一个 Run。
// mu 保护运行状态和消息，避免外部读取与执行并发时发生数据竞争。
type Agent struct {
	provider           Provider
	model              string
	systemPrompt       string
	tools              []compiledTool
	maxTurns           int
	plansDir           string
	planTemplatePath   string
	keepRecentTurns    int
	systemPromptLoader func(context.Context) (string, error)
	contextLimitTokens int

	mu            sync.Mutex
	running       bool
	messages      []Message
	collaboration CollaborationState
}

// Run 执行一次用户输入，逐轮请求模型并按顺序执行工具，直到模型给出最终回复。
// 同一 Agent 不允许并发 Run；取消时仍为已发出的工具调用补齐结果消息。
func (a *Agent) Run(ctx context.Context, prompt string, emit EmitFunc) (result RunResult, runErr error) {
	return a.run(ctx, prompt, emit, nil)
}

// run accepts a durable state writer when a Session owns this run.
func (a *Agent) run(ctx context.Context, prompt string, emit EmitFunc, record func(Event) error) (result RunResult, runErr error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return RunResult{}, ErrBusy
	}
	a.running = true
	userMessage := Message{Role: RoleUser, Content: prompt}
	a.messages = append(a.messages, userMessage)
	a.mu.Unlock()
	var runMessages []Message
	observer := emit
	emit = func(event Event) {
		if event.Type == EventMessageAdded && event.Message != nil {
			runMessages = append(runMessages, *cloneMessagePointer(*event.Message))
		}
		emitEvent(observer, event)
	}

	reason := "error"
	emitEvent(emit, Event{Type: EventRunStart})
	emitEvent(emit, Event{Type: EventMessageAdded, Message: cloneMessagePointer(userMessage)})
	// 无论正常结束还是中途报错，都释放运行锁并发出结束事件。
	defer func() {
		a.mu.Lock()
		result.Messages = cloneMessages(runMessages)
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

	for turn := 1; turn <= a.maxTurns; turn++ {
		result.Turns = turn
		if err := ctx.Err(); err != nil {
			return result, err
		}
		emitEvent(emit, Event{Type: EventTurnStart, Turn: turn})
		response, err := a.requestWithCompaction(ctx, turn, emit, record)
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
			text, isError := a.executeTool(ctx, call, turn, emit, record)
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
