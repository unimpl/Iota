package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestSessionLogRecordsOrderedEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	log, err := newSessionLog("test-model", "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()
	if !regexp.MustCompile(`\d{4}-\d{2}-\d{2}-[0-9a-f-]{36}\.jsonl$`).MatchString(log.path) {
		t.Fatalf("unexpected path: %s", log.path)
	}
	if filepath.Dir(log.path) != filepath.Join(home, ".iota", "sessions") {
		t.Fatalf("unexpected directory: %s", log.path)
	}
	if err := log.event("run-1", iota.Event{Type: iota.EventToolEnd, Turn: 1, ToolResult: "result"}); err != nil {
		t.Fatal(err)
	}
	if err := log.write("", "session_end", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(log.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions: %o", info.Mode().Perm())
	}
	file, err := os.Open(log.path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var records []sessionRecord
	for scanner.Scan() {
		var record sessionRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0].Type != "session_start" || records[1].Type != "tool_end" || records[2].Type != "session_end" {
		t.Fatalf("records: %+v", records)
	}
	if records[0].SessionID != log.id || records[1].RunID != "run-1" || records[2].Sequence != 3 {
		t.Fatalf("records: %+v", records)
	}
}
