package iota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const historyMaxBytes = 32 * 1024

type historyArguments struct {
	Query              string `json:"query"`
	StartSeq           uint64 `json:"start_seq"`
	EndSeq             uint64 `json:"end_seq"`
	Offset             int    `json:"offset"`
	Limit              int    `json:"limit"`
	RunID              string `json:"run_id"`
	StepID             string `json:"step_id"`
	IncludeBeforeReset bool   `json:"include_before_reset"`
}

type historyItem struct {
	Source     MessageSource `json:"source"`
	Role       Role          `json:"role"`
	ToolName   string        `json:"tool_name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	IsError    bool          `json:"is_error,omitempty"`
	Content    string        `json:"content"`
	Offset     int           `json:"offset"`
	TotalChars int           `json:"total_chars"`
}

type historyPage struct {
	Records    []historyItem `json:"records"`
	ResetSeq   uint64        `json:"reset_seq,omitempty"`
	NextSeq    uint64        `json:"next_seq,omitempty"`
	NextOffset int           `json:"next_offset,omitempty"`
}

func NewSearchHistoryTool() Tool {
	return Tool{Name: "search_history", ReadOnly: true, historyTool: true,
		Description: "Search original messages and tool calls/results in this session with a literal, case-insensitive query. Returns snippets and source seq/run/step IDs. Defaults to records after the latest reset. Continue with start_seq=next_seq. Does not search request copies or stream deltas.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":1000},"start_seq":{"type":"integer","minimum":1},"end_seq":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1,"maximum":50},"run_id":{"type":"string"},"step_id":{"type":"string"},"include_before_reset":{"type":"boolean"}},"required":["query"],"additionalProperties":false}`)}
}

func NewReadHistoryTool() Tool {
	return Tool{Name: "read_history", ReadOnly: true, historyTool: true,
		Description: "Read original messages and tool calls/results within an inclusive seq range in this session. Output is bounded to 32 KiB. For a continuation, use start_seq=next_seq and offset=next_offset; offsets count Unicode characters within that record. Defaults to records after the latest reset.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"start_seq":{"type":"integer","minimum":1},"end_seq":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":50},"run_id":{"type":"string"},"step_id":{"type":"string"},"include_before_reset":{"type":"boolean"}},"required":["start_seq","end_seq"],"additionalProperties":false}`)}
}

func historyText(message Message) string {
	text := message.Content
	for _, call := range message.ToolCalls {
		text += "\nTool call " + call.ID + " (" + call.Name + "):\n" + string(call.Arguments)
	}
	return text
}

func (a *Agent) executeHistoryTool(ctx context.Context, call ToolCall) (string, error) {
	var args historyArguments
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return "", err
	}
	a.mu.Lock()
	session := a.history
	a.mu.Unlock()
	if session == nil {
		return "", errors.New("history tools require a persisted session")
	}
	return session.queryHistory(ctx, args, call.Name == "search_history")
}

func (s *Session) queryHistory(ctx context.Context, args historyArguments, search bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return "", err
	}
	if args.StartSeq == 0 {
		args.StartSeq = 1
	}
	if args.EndSeq == 0 {
		args.EndSeq = s.seq
	}
	if args.StartSeq > args.EndSeq || args.EndSeq > s.seq {
		return "", errors.New("invalid history seq range")
	}
	if !args.IncludeBeforeReset {
		args.StartSeq = max(args.StartSeq, s.resetSeq+1)
	}
	if args.Limit == 0 {
		args.Limit = 20
	}
	page := historyPage{Records: []historyItem{}, ResetSeq: s.resetSeq}
	query := strings.ToLower(args.Query)
	if search && query == "" {
		return "", errors.New("history search query cannot be empty")
	}
	for _, message := range s.history {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		source := message.Source
		if source == nil || source.Seq < args.StartSeq || source.Seq > args.EndSeq || (args.RunID != "" && source.RunID != args.RunID) || (args.StepID != "" && source.StepID != args.StepID) {
			continue
		}
		text := []rune(historyText(message))
		start, end := 0, len(text)
		if search {
			lower := strings.ToLower(string(text))
			byteIndex := strings.Index(lower, query)
			if byteIndex < 0 {
				continue
			}
			match := len([]rune(lower[:byteIndex]))
			start, end = max(0, match-120), min(len(text), match+len([]rune(query))+240)
		} else if source.Seq == args.StartSeq {
			start = args.Offset
			if start > len(text) {
				return "", fmt.Errorf("offset exceeds record %d", source.Seq)
			}
		}
		if len(page.Records) >= args.Limit {
			page.NextSeq = source.Seq
			break
		}
		item := historyItem{Source: *source, Role: message.Role, ToolName: message.ToolName, ToolCallID: message.ToolCallID, IsError: message.IsError, Offset: start, TotalChars: len(text)}
		// Reserve enough room for page metadata even when text contains JSON escapes.
		for {
			item.Content = string(text[start:end])
			candidate := append(page.Records, item)
			data, err := json.Marshal(candidate)
			if err != nil {
				return "", err
			}
			if len(data) <= historyMaxBytes-1024 {
				page.Records = candidate
				break
			}
			if len(page.Records) > 0 {
				page.NextSeq, page.NextOffset = source.Seq, start
				break
			}
			if end-start <= 1 {
				return "", errors.New("history metadata exceeds output limit")
			}
			end = start + (end-start)/2
		}
		if page.NextSeq != 0 {
			break
		}
		if !search && end < len(text) {
			page.NextSeq, page.NextOffset = source.Seq, end
			break
		}
	}
	data, err := json.Marshal(page)
	return string(data), err
}
