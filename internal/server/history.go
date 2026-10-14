package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	iota "github.com/unimpl/Iota"
)

type toolActivity struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
	Status    string `json:"status"`
}

type chatItem struct {
	ID        string         `json:"id"`
	Sequence  uint64         `json:"seq"`
	RunID     string         `json:"run_id"`
	Role      string         `json:"role"`
	Text      string         `json:"text"`
	Reasoning string         `json:"reasoning"`
	Tools     []toolActivity `json:"tools"`
	Complete  bool           `json:"complete"`
	Error     string         `json:"error,omitempty"`
}

// Run, Turn and Action resume the live reducer even if a snapshot ends midway
// through a reply, tool call or command. Cursor covers omitted records as well.
type historySnapshot struct {
	Items         []chatItem               `json:"items"`
	Runs          []runIndex               `json:"runs"`
	LoadedRuns    []string                 `json:"loaded_runs"`
	Cursor        uint64                   `json:"cursor"`
	Run           string                   `json:"run"`
	Turn          int                      `json:"turn"`
	Collaboration *iota.CollaborationState `json:"collaboration,omitempty"`
	Action        string                   `json:"action,omitempty"`
}

type historyItem struct {
	chatItem
	text      strings.Builder
	reasoning strings.Builder
}

type historyBuilder struct {
	historySnapshot
	items      []*historyItem
	assistants map[string]*historyItem
	runItems   map[string][]*historyItem
}

// Decode only display fields; provider traces and model context can be large.
type historyEvent struct {
	Turn          int                      `json:"turn,omitempty"`
	Text          string                   `json:"text,omitempty"`
	Reasoning     string                   `json:"reasoning,omitempty"`
	Message       *iota.Message            `json:"message,omitempty"`
	ToolCall      *iota.ToolCall           `json:"tool_call,omitempty"`
	ToolResult    string                   `json:"tool_result,omitempty"`
	IsError       bool                     `json:"is_error,omitempty"`
	Reason        string                   `json:"reason,omitempty"`
	Error         string                   `json:"error,omitempty"`
	Collaboration *iota.CollaborationState `json:"collaboration,omitempty"`
}

func readHistory(ctx context.Context, path string, query historyQuery) (historySnapshot, error) {
	index, err := readHistoryIndex(ctx, path, query.At)
	if err != nil {
		return historySnapshot{}, err
	}
	first, last, err := index.page(query)
	if err != nil {
		return historySnapshot{}, err
	}
	b := historyBuilder{assistants: make(map[string]*historyItem), runItems: make(map[string][]*historyItem)}
	var projectionErr error
	err = walkRecords(path, func(record Record) bool {
		if record.Sequence > index.Cursor {
			return false
		}
		if projectionErr = ctx.Err(); projectionErr != nil {
			return false
		}
		if record.Sequence < first || record.Sequence > last {
			return true
		}
		if record.RunID != "" && record.RunID != b.Run {
			// Restore the command at this run's boundary, including CLI runs
			// after an omitted execute whose action was already consumed.
			b.Action = index.actions[record.RunID]
		}
		projectionErr = b.apply(record)
		return projectionErr == nil
	})
	if err != nil {
		return historySnapshot{}, err
	}
	if projectionErr != nil {
		return historySnapshot{}, projectionErr
	}
	b.Items = make([]chatItem, 0, len(b.items))
	for _, item := range b.items {
		item.Text, item.Reasoning = item.text.String(), item.reasoning.String()
		b.Items = append(b.Items, item.chatItem)
	}
	b.Cursor, b.Run, b.Turn, b.Action, b.Collaboration = index.Cursor, index.Run, index.Turn, index.Action, index.Collaboration
	b.Runs, b.LoadedRuns = index.Runs, []string{}
	for _, run := range index.Runs {
		if run.Start >= first && run.Start <= last {
			b.LoadedRuns = append(b.LoadedRuns, run.ID)
		}
	}
	return b.historySnapshot, nil
}

func (b *historyBuilder) assistant(turn int, seq uint64) *historyItem {
	id := fmt.Sprintf("assistant-%s-%d", b.Run, turn)
	if item := b.assistants[id]; item != nil {
		return item
	}
	item := &historyItem{chatItem: chatItem{ID: id, Sequence: seq, RunID: b.Run, Role: "assistant", Tools: []toolActivity{}}}
	b.assistants[id] = item
	b.items = append(b.items, item)
	b.runItems[b.Run] = append(b.runItems[b.Run], item)
	return item
}

func (b *historyBuilder) notice(seq uint64, text, errorText string) {
	item := &historyItem{chatItem: chatItem{ID: fmt.Sprintf("notice-%d", seq), Sequence: seq, RunID: b.Run, Role: "notice", Tools: []toolActivity{}, Complete: true, Error: errorText}}
	item.text.WriteString(text)
	b.items = append(b.items, item)
}

func (b *historyBuilder) apply(record Record) error {
	b.Cursor = record.Sequence
	if record.RunID != "" {
		b.Run = record.RunID
	}
	if record.Type == "client_request" {
		var command commandRequest
		if err := json.Unmarshal(record.Payload, &command); err != nil {
			return err
		}
		b.Action = command.Action
		if b.Action == "run" && strings.TrimSpace(command.Prompt) == "/execute" {
			b.Action = "execute"
		}
		return nil
	}
	if record.Type == "client_result" {
		var result commandResult
		if err := json.Unmarshal(record.Payload, &result); err != nil {
			return err
		}
		if result.Output != "" {
			b.notice(record.Sequence, result.Output, "")
		}
		if result.Status == "failed" && result.Error != "" && (len(b.items) == 0 || b.items[len(b.items)-1].Error != result.Error) {
			b.notice(record.Sequence, "操作失败", result.Error)
		}
		return nil
	}
	switch record.Type {
	case "run_start", "turn_start", "message_added", "text_delta", "reasoning_delta", "tool_start", "tool_end", "run_end", "session_reset", "context_compacted", "mode_changed", "plan_saved", "plan_approved", "progress_updated":
	default:
		return nil // Do not decode model requests, responses or raw provider chunks.
	}
	var event historyEvent
	if len(record.Payload) > 0 {
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			return fmt.Errorf("invalid history event %d: %w", record.Sequence, err)
		}
	}
	if event.Collaboration != nil {
		b.Collaboration = event.Collaboration
	}
	turn := event.Turn
	if turn == 0 {
		turn = b.Turn
	}
	switch record.Type {
	case "run_start":
		b.Turn = 0
	case "turn_start":
		b.Turn = event.Turn
		if b.Turn == 0 {
			b.Turn = turn + 1
		}
	case "message_added":
		message := event.Message
		if message == nil || message.Skill != nil || message.ContextSummary || message.RuntimeContext {
			break
		}
		switch message.Role {
		case iota.RoleUser:
			text := message.Content
			if b.Action == "execute" {
				text = "批准并执行当前计划"
			}
			item := &historyItem{chatItem: chatItem{ID: fmt.Sprintf("user-%d", record.Sequence), Sequence: record.Sequence, RunID: b.Run, Role: "user", Tools: []toolActivity{}, Complete: true}}
			item.text.WriteString(text)
			b.items = append(b.items, item)
			b.Action = ""
		case iota.RoleAssistant:
			item := b.assistant(turn, record.Sequence)
			item.text.Reset()
			item.text.WriteString(message.Content)
			item.Complete = true
		case iota.RoleTool:
			item := b.assistants[fmt.Sprintf("assistant-%s-%d", b.Run, turn)]
			if record.RunID == "" {
				item = nil
				// Crash repair has no run/turn IDs: use the latest pending call.
				for index := len(b.items) - 1; index >= 0 && item == nil; index-- {
					for _, tool := range b.items[index].Tools {
						if tool.ID == message.ToolCallID && tool.Status == "running" {
							item = b.items[index]
							break
						}
					}
				}
			}
			if item != nil {
				item.finishTool(message.ToolCallID, message.Content, message.IsError)
			}
		}
	case "text_delta":
		b.assistant(turn, record.Sequence).text.WriteString(event.Text)
	case "reasoning_delta":
		b.assistant(turn, record.Sequence).reasoning.WriteString(event.Reasoning)
	case "tool_start":
		if call := event.ToolCall; call != nil {
			var arguments bytes.Buffer
			if err := json.Indent(&arguments, call.Arguments, "", "  "); err != nil {
				return err
			}
			item := b.assistant(turn, record.Sequence)
			item.Tools = append(item.Tools, toolActivity{ID: call.ID, Name: call.Name, Arguments: arguments.String(), Status: "running"})
		}
	case "tool_end":
		if call := event.ToolCall; call != nil {
			b.assistant(turn, record.Sequence).finishTool(call.ID, event.ToolResult, event.IsError)
		}
	case "run_end":
		for _, item := range b.runItems[b.Run] {
			if !item.Complete {
				item.Error = event.Error
				if event.Reason == "aborted" {
					item.Error = "回复已中断"
				}
			}
			item.Complete = true
			for index := range item.Tools {
				if item.Tools[index].Status == "running" {
					item.Tools[index].Status = "interrupted"
				}
			}
		}
		if event.Error != "" {
			if event.Reason == "aborted" {
				b.notice(record.Sequence, "已停止本次任务", "")
			} else {
				b.notice(record.Sequence, "本次任务失败", event.Error)
			}
		}
	case "session_reset":
		mode := iota.ModeDefault
		if b.Collaboration != nil {
			mode = b.Collaboration.Mode
		}
		b.Collaboration = &iota.CollaborationState{Mode: mode}
		b.notice(record.Sequence, "已重置上下文。之前的对话保留在历史中。 ", "")
	case "context_compacted":
		b.notice(record.Sequence, "已压缩模型上下文，完整对话仍保留。 ", "")
	}
	return nil
}

func (item *historyItem) finishTool(id, result string, failed bool) {
	for index := range item.Tools {
		if item.Tools[index].ID == id {
			item.Tools[index].Result = result
			item.Tools[index].Status = "completed"
			if failed {
				item.Tools[index].Status = "failed"
			}
		}
	}
}
