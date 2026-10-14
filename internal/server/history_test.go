package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	iota "github.com/unimpl/Iota"
)

func TestHistoryInFlightSnapshotAndStreamHandoff(t *testing.T) {
	started := make(chan struct{})
	resume := make(chan struct{})
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		emit(iota.Delta{Text: "partial", Reasoning: "thinking"})
		close(started)
		select {
		case <-resume:
		case <-ctx.Done():
			return iota.Response{}, ctx.Err()
		}
		return iota.Response{Content: "finished"}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "run", Action: "run", Prompt: "question"}, 202)
	<-started
	path := filepath.Join(s.folder, name)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
	var snapshot historySnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 2 || snapshot.Items[1].Text != "partial" || snapshot.Items[1].Reasoning != "thinking" || snapshot.Items[1].Complete || snapshot.Run == "" || snapshot.Turn != 1 {
		t.Fatalf("active snapshot=%s", data)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reading history modified the running session")
	}
	// Records written between history and SSE must also be delivered.
	close(resume)
	waitState(t, httpServer, name, "completed")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/sessions/%s/events?after=%d", httpServer.URL, name, snapshot.Cursor), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	cursor := snapshot.Cursor
	for {
		frame := readStreamFrame(t, reader)
		if frame.Event != "records" {
			continue
		}
		var batch eventBatch
		if err := json.Unmarshal(frame.Data, &batch); err != nil {
			t.Fatal(err)
		}
		if batch.Cursor <= cursor {
			t.Fatalf("cursor moved backwards: %d after %d", batch.Cursor, cursor)
		}
		for _, record := range batch.Records {
			if record.Sequence <= cursor || record.Sequence > batch.Cursor {
				t.Fatalf("record outside handoff range: seq=%d cursor=%d", record.Sequence, cursor)
			}
			if record.Type == "message_added" {
				var event historyEvent
				if err := json.Unmarshal(record.Payload, &event); err != nil {
					t.Fatal(err)
				}
				if event.Message != nil && event.Message.Role == iota.RoleAssistant && event.Message.Content == "finished" {
					return
				}
			}
		}
		cursor = batch.Cursor
	}
}

func TestHistorySnapshotMatchesLiveReplay(t *testing.T) {
	data, err := os.ReadFile("testdata/history.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Records  []Record        `json:"records"`
		Snapshot historySnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	encoder := json.NewEncoder(&log)
	for _, record := range fixture.Records {
		record.Version, record.SessionID = 1, "test"
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, log.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readHistory(context.Background(), path, historyQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, fixture.Snapshot) {
		data, _ := json.Marshal(snapshot)
		t.Fatalf("snapshot differs from shared frontend fixture: %s", data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readHistory(ctx, path, historyQuery{}); err != context.Canceled {
		t.Fatalf("canceled history: %v", err)
	}
}

func TestHistoryMergesThousandsOfDeltasAndToolResults(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		for _, message := range request.Messages {
			if message.Role == iota.RoleTool {
				return iota.Response{Content: "done"}, nil
			}
		}
		for index := 0; index < 2000; index++ {
			emit(iota.Delta{Text: "x", Reasoning: "思", RawChunk: "provider-only-marker"})
		}
		return iota.Response{Content: "complete", ToolCalls: []iota.ToolCall{{ID: "read", Name: "read", Arguments: json.RawMessage(`{"path":"identity.txt"}`)}}}, nil
	}}
	s, httpServer := testServer(t, provider)
	if err := os.WriteFile(filepath.Join(s.config.Settings.CWD, "identity.txt"), []byte("tool result"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := createSession(t, httpServer, s.config.Settings.CWD)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "run", Action: "run", Prompt: "question"}, 202)
	waitState(t, httpServer, name, "completed")
	data := request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
	var snapshot historySnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 3 || snapshot.Items[1].Text != "complete" || snapshot.Items[1].Reasoning != strings.Repeat("思", 2000) || snapshot.Items[2].Text != "done" {
		t.Fatalf("deltas were not merged: items=%d", len(snapshot.Items))
	}
	tool := snapshot.Items[1].Tools[0]
	if tool.Status != "completed" || !strings.Contains(tool.Result, "tool result") {
		t.Fatalf("tool=%+v", tool)
	}
	for _, excluded := range []string{"provider-only-marker", "text_delta", "reasoning_delta", "tool_start", "tool_end", `"records"`} {
		if bytes.Contains(data, []byte(excluded)) {
			t.Fatalf("history transmitted %s", excluded)
		}
	}
	original, err := os.ReadFile(filepath.Join(s.folder, name))
	if err != nil {
		t.Fatal(err)
	}
	var eventBytes int
	err = walkRecords(filepath.Join(s.folder, name), func(record Record) bool {
		encoded, err := json.Marshal(chatRecord(record))
		if err != nil {
			t.Fatal(err)
		}
		eventBytes += len(encoded)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(data)*20 >= eventBytes {
		t.Fatalf("snapshot=%d bytes event responses=%d bytes", len(data), eventBytes)
	}
	t.Logf("history snapshot: %d bytes; former event responses: %d bytes; original log: %d bytes", len(data), eventBytes, len(original))
}

// Compare the former 200-record paging algorithm with a single merged snapshot.
// Both read the same on-disk trace and serialize the response.
func BenchmarkHistory(b *testing.B) {
	var log bytes.Buffer
	encoder := json.NewEncoder(&log)
	for sequence := uint64(1); sequence <= 6000; sequence++ {
		record := Record{Version: 1, Sequence: sequence, SessionID: "test", RunID: "run", Type: "reasoning_delta", Payload: json.RawMessage(`{"turn":1,"reasoning":"thinking","raw_chunk":"provider chunk"}`)}
		if sequence == 1 {
			record.Type, record.Payload = "session_start", nil
		}
		if sequence%200 == 0 {
			record.Type = "model_request"
			record.Payload = json.RawMessage(fmt.Sprintf(`{"request":{"system_prompt":%q}}`, strings.Repeat("context", 10000)))
		}
		if err := encoder.Encode(record); err != nil {
			b.Fatal(err)
		}
	}
	path := filepath.Join(b.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, log.Bytes(), 0o600); err != nil {
		b.Fatal(err)
	}
	b.Run("event-pages", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			for after := uint64(0); after < 6000; after += 200 {
				page := make([]Record, 0, 200)
				if err := walkRecords(path, func(record Record) bool {
					if record.Sequence > after {
						page = append(page, chatRecord(record))
					}
					return len(page) < 200
				}); err != nil {
					b.Fatal(err)
				}
				if _, err := json.Marshal(page); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("merged-snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			snapshot, err := readHistory(context.Background(), path, historyQuery{})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := json.Marshal(snapshot); err != nil {
				b.Fatal(err)
			}
		}
	})
}
