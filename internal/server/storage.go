package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	iota "github.com/unimpl/Iota"
)

var sessionName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jsonl$`)

// Record preserves SDK sequence numbers for history snapshots and SSE resumption.
type Record struct {
	Version   int             `json:"version"`
	Sequence  uint64          `json:"seq"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Keep sequence IDs while excluding repeated model requests and raw provider
// chunks from the chat transport. The original JSONL remains unchanged.
func chatRecord(record Record) Record {
	switch record.Type {
	case "session_start", "run_start", "turn_start", "session_reset", "context_compacted", "tool_start", "tool_end", "run_end", "mode_changed", "plan_saved", "plan_approved", "progress_updated", "client_result":
		return record
	case "client_request":
		var command commandRequest
		if json.Unmarshal(record.Payload, &command) != nil {
			record.Payload = nil
			return record
		}
		action := command.Action
		if action == "run" && strings.TrimSpace(command.Prompt) == "/execute" {
			action = "execute"
		}
		record.Payload, _ = json.Marshal(struct {
			Action string `json:"action"`
		}{action})
		return record
	case "text_delta", "reasoning_delta":
		var event iota.Event
		if json.Unmarshal(record.Payload, &event) != nil {
			record.Payload = nil
			return record
		}
		event.RawChunk = ""
		record.Payload, _ = json.Marshal(event)
		return record
	case "message_added":
		var event iota.Event
		if json.Unmarshal(record.Payload, &event) != nil || event.Message == nil {
			record.Payload = nil
			return record
		}
		if event.Message.Skill != nil || event.Message.ContextSummary || event.Message.RuntimeContext {
			record.Payload = nil
			return record
		}
		event.Message.ToolCalls = nil
		event.Message.Sources = nil
		event.Message.Checkpoint = nil
		record.Payload, _ = json.Marshal(event)
		return record
	default:
		record.Payload = nil
		return record
	}
}

func sessionPath(folder, name string) (string, error) {
	if !sessionName.MatchString(name) {
		return "", errors.New("invalid session name")
	}
	path := filepath.Join(folder, name)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("session is not a regular file")
	}
	return path, nil
}

// A reader never calls OpenSession: reading must not repair an active tool call.
// An incomplete final line is withheld until the writer finishes it.
func walkRecords(path string, visit func(Record) bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	// Bound the read so an active writer cannot extend this snapshot indefinitely.
	reader := bufio.NewReader(io.LimitReader(file, info.Size()))
	var sequence uint64
	var id string
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			return fmt.Errorf("invalid record %d: %w", sequence+1, err)
		}
		if record.Version != 1 || record.Sequence != sequence+1 || record.SessionID == "" {
			return fmt.Errorf("invalid session record %d", sequence+1)
		}
		if sequence == 0 {
			if record.Type != "session_start" {
				return errors.New("missing session header")
			}
			id = record.SessionID
		} else if record.SessionID != id {
			return errors.New("session ID changed")
		}
		sequence = record.Sequence
		if !visit(record) {
			return nil
		}
	}
}

type sessionSummary struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	Model     string `json:"model"`
	CWD       string `json:"cwd"`
	Occupied  bool   `json:"occupied"`
	Open      bool   `json:"open"`
}

func readSummary(path string) (sessionSummary, error) {
	summary := sessionSummary{Name: filepath.Base(path), Title: "新对话"}
	err := walkRecords(path, func(record Record) bool {
		if record.Type == "session_start" {
			var info iota.SessionInfo
			if json.Unmarshal(record.Payload, &info) == nil {
				summary.Model, summary.CWD = info.Model, info.CWD
			}
			summary.CreatedAt = record.Timestamp
		}
		if record.Type == "message_added" {
			var event iota.Event
			if json.Unmarshal(record.Payload, &event) == nil && event.Message != nil && event.Message.Role == iota.RoleUser {
				text := []rune(event.Message.Content)
				if len(text) > 40 {
					text = text[:40]
				}
				summary.Title = string(text)
				return false
			}
		}
		return true
	})
	return summary, err
}
