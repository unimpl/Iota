package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestHistoryRunPagesAndIndex(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		for n := len(request.Messages) - 1; n >= 0; n-- {
			message := request.Messages[n]
			if message.Role == iota.RoleUser && !message.RuntimeContext {
				return iota.Response{Content: "answer " + message.Content}, nil
			}
		}
		return iota.Response{}, fmt.Errorf("missing user message")
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	for n := 1; n <= 8; n++ {
		request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: fmt.Sprint(n), Action: "run", Prompt: fmt.Sprintf("question %d", n)}, 202)
		waitState(t, httpServer, name, "completed")
	}
	read := func(query string) historySnapshot {
		t.Helper()
		data := request(t, httpServer, "GET", "sessions/"+name+"/history"+query, nil, 200)
		var snapshot historySnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Runs) != 8 {
			t.Fatalf("index has %d runs", len(snapshot.Runs))
		}
		return snapshot
	}
	latest := read("")
	if len(latest.Items) != 6 || len(latest.LoadedRuns) != 3 || latest.Items[0].Text != "question 6" {
		t.Fatalf("latest page=%+v", latest)
	}
	for n, run := range latest.Runs {
		if run.Title != fmt.Sprintf("question %d", n+1) || run.Preview != "answer "+run.Title || run.Status != "completed" || run.StartedAt == "" || run.Start > run.End {
			t.Fatalf("index entry=%+v", run)
		}
	}
	middle := read("?before=" + latest.LoadedRuns[0])
	first := read("?before=" + middle.LoadedRuns[0])
	if len(middle.Items) != 6 || middle.Items[0].Text != "question 3" || len(first.Items) != 4 || first.Items[0].Text != "question 1" {
		t.Fatalf("middle=%+v first=%+v", middle, first)
	}
	jump := read("?run=" + latest.Runs[1].ID)
	if len(jump.Items) != 2 || len(jump.LoadedRuns) != 1 || jump.Items[0].Text != "question 2" {
		t.Fatalf("jump=%+v", jump)
	}
	empty := read("?before=" + latest.Runs[0].ID)
	if len(empty.Items) != 0 || len(empty.LoadedRuns) != 0 {
		t.Fatalf("oldest boundary=%+v", empty)
	}
	for _, page := range []historySnapshot{middle, first, jump, empty} {
		if page.Cursor != latest.Cursor || page.Run != latest.Run || page.Turn != latest.Turn || !reflect.DeepEqual(page.Runs, latest.Runs) {
			t.Fatal("paging changed the live checkpoint or full index")
		}
	}
	for _, query := range []string{"?before=missing", "?run=missing", "?before=x&run=y", "?before=", "?run=x&run=y", "?limit=3", "?at=0", "?at=oops", "?at=999999"} {
		request(t, httpServer, "GET", "sessions/"+name+"/history"+query, nil, 400)
	}
	// Freeze a page before run_end even though the durable log is now complete.
	cut := latest.Runs[7].Start + 1 // The user message precedes turn/model events.
	data := request(t, httpServer, "GET", fmt.Sprintf("sessions/%s/history?run=%s&at=%d", name, latest.Runs[7].ID, cut), nil, 200)
	var frozen historySnapshot
	if err := json.Unmarshal(data, &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.Cursor != cut || frozen.Runs[7].Status != "running" || len(frozen.Items) != 1 || frozen.Items[0].Role != "user" {
		t.Fatalf("page exceeded stream checkpoint: %s", data)
	}
}

func TestHistoryIndexExcerpts(t *testing.T) {
	text := "  " + strings.Repeat("你", 170) + "\n\t end"
	if excerpt := historyExcerpt(text, 80); excerpt != strings.Repeat("你", 80)+"…" {
		t.Fatalf("Unicode excerpt=%q", excerpt)
	}
	if excerpt := historyExcerpt("a\n b\t c", 10); excerpt != "a b c" {
		t.Fatalf("whitespace excerpt=%q", excerpt)
	}
}

func TestHistoryOmittedExecuteDoesNotRelabelLaterCLIRuns(t *testing.T) {
	var log bytes.Buffer
	encoder := json.NewEncoder(&log)
	seq := uint64(0)
	appendRecord := func(run, kind, payload string) {
		t.Helper()
		seq++
		if err := encoder.Encode(Record{Version: 1, SessionID: "test", Sequence: seq, RunID: run, Type: kind, Payload: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	appendRecord("", "session_start", `{}`)
	appendRecord("", "client_request", `{"action":"execute"}`)
	for n := 1; n <= 5; n++ {
		run := fmt.Sprint(n)
		appendRecord(run, "run_start", `{}`)
		appendRecord(run, "message_added", fmt.Sprintf(`{"message":{"role":"user","content":"CLI task %d"}}`, n))
		appendRecord(run, "run_end", `{"reason":"stop"}`)
	}
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, log.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := readHistory(context.Background(), path, historyQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || page.Items[0].Text != "CLI task 3" || page.Runs[0].Title != "批准并执行当前计划" {
		t.Fatalf("stale execute action leaked into a CLI page: %+v", page)
	}
	jump, err := readHistory(context.Background(), path, historyQuery{Run: "1"})
	if err != nil || jump.Items[0].Text != "批准并执行当前计划" {
		t.Fatalf("execute page lost its action: %+v %v", jump, err)
	}
}
