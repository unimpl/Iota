package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	iota "github.com/unimpl/Iota"
)

type runIndex struct {
	ID        string `json:"id"`
	Start     uint64 `json:"start_seq"`
	End       uint64 `json:"end_seq"`
	StartedAt string `json:"started_at"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	Status    string `json:"status"`
}

type historyQuery struct {
	Before string
	Run    string
	At     uint64
}

type historyIndex struct {
	historySnapshot
	actions map[string]string
}

// Index all runs without retaining reasoning, tool results or provider traces.
// The second pass projects only the requested runs, bounded by this cursor even
// if an active writer appends more records between the two reads.
func readHistoryIndex(ctx context.Context, path string, at uint64) (historyIndex, error) {
	index := historyIndex{historySnapshot: historySnapshot{Runs: []runIndex{}}, actions: make(map[string]string)}
	positions := make(map[string]int)
	var indexErr error
	err := walkRecords(path, func(record Record) bool {
		if at != 0 && record.Sequence > at {
			return false
		}
		if indexErr = ctx.Err(); indexErr != nil {
			return false
		}
		index.Cursor = record.Sequence
		if record.RunID != "" {
			index.Run = record.RunID
			if _, exists := positions[record.RunID]; !exists {
				index.actions[record.RunID] = index.Action
				positions[record.RunID] = len(index.Runs)
				index.Runs = append(index.Runs, runIndex{ID: record.RunID, Start: record.Sequence, StartedAt: record.Timestamp, Title: "未命名任务", Status: "running"})
			}
		}
		var run *runIndex
		if position, exists := positions[index.Run]; exists {
			run = &index.Runs[position]
			run.End = record.Sequence
		}
		if record.Type == "client_request" {
			var command commandRequest
			indexErr = json.Unmarshal(record.Payload, &command)
			index.Action = command.Action
			if command.Action == "run" && strings.TrimSpace(command.Prompt) == "/execute" {
				index.Action = "execute"
			}
			return indexErr == nil
		}
		switch record.Type {
		case "run_start":
			index.Turn = 0
		case "turn_start", "message_added", "run_end", "session_reset", "mode_changed", "plan_saved", "plan_approved", "progress_updated", "context_compacted":
		default:
			return true
		}
		// Decode only index fields, even for large complete assistant messages.
		var event struct {
			Turn          int                      `json:"turn"`
			Reason        string                   `json:"reason"`
			Collaboration *iota.CollaborationState `json:"collaboration"`
			Message       *struct {
				Role           string          `json:"role"`
				Content        string          `json:"content"`
				Skill          json.RawMessage `json:"skill"`
				ContextSummary bool            `json:"context_summary"`
				RuntimeContext bool            `json:"runtime_context"`
			} `json:"message"`
		}
		if len(record.Payload) > 0 {
			if indexErr = json.Unmarshal(record.Payload, &event); indexErr != nil {
				return false
			}
		}
		if event.Collaboration != nil {
			index.Collaboration = event.Collaboration
		}
		switch record.Type {
		case "turn_start":
			index.Turn++
			if event.Turn != 0 {
				index.Turn = event.Turn
			}
		case "message_added":
			message := event.Message
			if message == nil || (len(message.Skill) > 0 && string(message.Skill) != "null") || message.ContextSummary || message.RuntimeContext {
				break
			}
			if message.Role == "user" {
				if run != nil && run.Title == "未命名任务" {
					run.Title = historyExcerpt(message.Content, 80)
					if index.Action == "execute" {
						run.Title = "批准并执行当前计划"
					}
				}
				index.Action = ""
			} else if message.Role == "assistant" && run != nil && strings.TrimSpace(message.Content) != "" {
				run.Preview = historyExcerpt(message.Content, 160)
			}
		case "run_end":
			if run != nil {
				run.Status = "completed"
				if event.Reason == "aborted" {
					run.Status = "aborted"
				} else if event.Reason == "error" {
					run.Status = "failed"
				}
			}
		case "session_reset":
			mode := iota.ModeDefault
			if index.Collaboration != nil {
				mode = index.Collaboration.Mode
			}
			index.Collaboration = &iota.CollaborationState{Mode: mode}
		}
		return true
	})
	if err != nil {
		return historyIndex{}, err
	}
	if at > index.Cursor && indexErr == nil {
		return historyIndex{}, fmt.Errorf("history cursor %d is beyond the log", at)
	}
	return index, indexErr
}

func historyExcerpt(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > limit {
		return string([]rune(text)[:limit]) + "…"
	}
	return text
}

func (index historyIndex) page(query historyQuery) (uint64, uint64, error) {
	end := len(index.Runs)
	if query.Before != "" || query.Run != "" {
		id := query.Before
		if query.Run != "" {
			id = query.Run
		}
		end = -1
		for position, run := range index.Runs {
			if run.ID == id {
				end = position
				break
			}
		}
		if end < 0 {
			return 0, 0, fmt.Errorf("unknown history run %q", id)
		}
		if query.Run != "" {
			run := index.Runs[end]
			return run.Start, run.End, nil
		}
	}
	if end == 0 {
		if len(index.Runs) == 0 {
			return 0, index.Cursor, nil
		}
		return 0, 0, nil
	}
	start := max(0, end-3)
	first := index.Runs[start].Start
	if start == 0 {
		first = 0 // Include notices before the first run.
	}
	return first, index.Runs[end-1].End, nil
}
