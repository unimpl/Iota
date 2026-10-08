package iota

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// OpenSession 读取已有会话并准备追加；损坏文件会报错，不覆盖原文件。
// 只恢复完整消息。中断后未完成的工具调用补为错误结果，不重新执行工具。
func OpenSession(path string) (*Session, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	s := &Session{file: file, path: path, collaboration: CollaborationState{Mode: ModeDefault}}
	if err := s.read(); err != nil {
		file.Close()
		return nil, fmt.Errorf("load session: %w", err)
	}
	pending, err := pendingSessionCalls(s.messages)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("load session: %w", err)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		file.Close()
		return nil, err
	}
	for _, call := range pending {
		message := Message{Role: RoleTool, ToolCallID: call.ID, ToolName: call.Name,
			Content: "tool execution interrupted before its result was saved", IsError: true}
		if err := s.write("", string(EventMessageAdded), Event{Type: EventMessageAdded, Message: &message}); err != nil {
			file.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Session) read() error {
	reader := bufio.NewReader(s.file)
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil {
			return fmt.Errorf("incomplete session record %d: %w", s.seq+1, err)
		}
		var record sessionRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return fmt.Errorf("record %d: %w", s.seq+1, err)
		}
		if record.Version != sessionFormatVersion || record.Sequence != s.seq+1 || record.SessionID == "" {
			return fmt.Errorf("invalid version, sequence or session ID at record %d", s.seq+1)
		}
		if s.seq == 0 {
			if record.Type != "session_start" {
				return errors.New("first record must be session_start")
			}
			s.id = record.SessionID
		} else if record.SessionID != s.id {
			return errors.New("session ID changed inside file")
		}
		s.seq = record.Sequence
		switch record.Type {
		case "session_reset":
			s.messages = nil
			s.collaboration.Plan = nil
			s.collaboration.Progress = nil
		case string(EventModeChanged), string(EventPlanSaved), string(EventPlanUpdated), string(EventPlanApproved):
			var event Event
			if err := json.Unmarshal(record.Payload, &event); err != nil || string(event.Type) != record.Type {
				return fmt.Errorf("invalid collaboration event at record %d", s.seq)
			}
			if err := validateCollaborationEvent(event); err != nil {
				return fmt.Errorf("invalid collaboration event at record %d: %w", s.seq, err)
			}
			s.collaboration = cloneCollaboration(*event.Collaboration)
		case string(EventMessageAdded):
			var event Event
			if err := json.Unmarshal(record.Payload, &event); err != nil || event.Message == nil || event.Type != EventMessageAdded {
				return fmt.Errorf("invalid message at record %d", s.seq)
			}
			s.messages = append(s.messages, *event.Message)
		case string(EventContextCompacted):
			var event Event
			if err := json.Unmarshal(record.Payload, &event); err != nil || event.Type != EventContextCompacted || event.Compaction == nil {
				return fmt.Errorf("invalid compaction event at record %d", s.seq)
			}
			if err := validateCompaction(*event.Compaction); err != nil {
				return fmt.Errorf("invalid compaction event at record %d: %w", s.seq, err)
			}
			s.messages = cloneMessages(event.Compaction.Context.Messages)
			s.contextLimitTokens = event.Compaction.ContextLimitTokens
			s.contextLimitModel = event.Compaction.Context.Model
		}
	}
	if s.seq == 0 {
		return errors.New("session file is empty")
	}
	return nil
}

// pendingSessionCalls 检查消息顺序和工具结果配对，返回中断后缺失结果的调用。
func pendingSessionCalls(messages []Message) ([]ToolCall, error) {
	var pending []ToolCall
	for _, message := range messages {
		switch message.Role {
		case RoleUser, RoleAssistant:
			if len(pending) > 0 {
				return nil, errors.New("message appears before pending tool results")
			}
			if message.Role == RoleAssistant {
				if err := validateToolCalls(message.ToolCalls); err != nil {
					return nil, err
				}
				pending = append(pending, message.ToolCalls...)
			} else if len(message.ToolCalls) > 0 {
				return nil, errors.New("user message contains tool calls")
			}
		case RoleTool:
			if len(pending) == 0 || message.ToolCallID != pending[0].ID || len(message.ToolCalls) > 0 {
				return nil, errors.New("tool result does not match its call")
			}
			pending = pending[1:]
		default:
			return nil, fmt.Errorf("unknown message role %q", message.Role)
		}
	}
	return pending, nil
}
