package iota

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeSummarySnapshot creates a private, immutable checkpoint snapshot.
// Existing files are never overwritten; JSONL remains the source of truth.
func writeSummarySnapshot(dir, sessionPath, sessionID, runID string, eventID uint64, state CompactionState) (path string, writeErr error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create summary directory: %w", err)
	}
	name := strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl") + fmt.Sprintf(".summary.%d.md", eventID)
	path, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create summary snapshot: %w", err)
	}
	defer func() {
		if writeErr != nil {
			file.Close()
			writeErr = errors.Join(writeErr, os.Remove(path))
		}
	}()
	remaining := "unknown (capacity unavailable)"
	if state.AfterUsage.RemainingPercent != nil {
		remaining = fmt.Sprintf("approximately %.1f%%", *state.AfterUsage.RemainingPercent)
	}
	body := fmt.Sprintf("# Context summary\n\n- Session: %s\n- Session file: %s\n- Checkpoint event: %d\n- Trigger: %s\n- Created: %s\n- Context before: approximately %d tokens (%d characters)\n- Context after: approximately %d tokens (%d characters)\n- Remaining capacity: %s\n",
		sessionID, sessionPath, eventID, state.Trigger, time.Now().Format(time.RFC3339Nano), state.BeforeUsage.Tokens, state.BeforeChars, state.AfterUsage.Tokens, state.AfterChars, remaining)
	if runID != "" {
		body += "- Triggering run: " + runID + "\n"
	}
	if state.ContextLimitTokens > 0 {
		body += fmt.Sprintf("- Reported capacity: %d tokens\n", state.ContextLimitTokens)
	}
	if state.Instructions != "" {
		body += "\n## Summary focus\n\n" + state.Instructions + "\n"
	}
	body += "\n## Checkpoint\n\n" + state.Summary + "\n"
	if len(state.Sources) > 0 {
		body += "\n## Original history sources\n" + sourceIndex(state.Sources) + "\n"
	}
	if _, err := io.WriteString(file, body); err != nil {
		return path, err
	}
	if err := file.Sync(); err != nil {
		return path, err
	}
	if err := file.Close(); err != nil {
		return path, err
	}
	return path, nil
}
