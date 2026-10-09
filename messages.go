package iota

import (
	"encoding/json"
)

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
	a.collaboration.Plan = nil
	a.collaboration.Progress = nil
	a.collaboration.Approval = nil
	return nil
}

// cloneMessages 复制消息及其工具调用，防止外部修改内部历史。
func cloneMessages(messages []Message) []Message {
	result := make([]Message, len(messages))
	for i, message := range messages {
		result[i] = *cloneMessagePointer(message)
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
	if message.Source != nil {
		source := *message.Source
		copy.Source = &source
	}
	copy.Sources = append([]SourceRange(nil), message.Sources...)
	return &copy
}
