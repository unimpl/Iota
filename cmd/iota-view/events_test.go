package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestEventsResumeAfterLastDeliveredRecord(t *testing.T) {
	dir := t.TempDir()
	name := "2026-10-13-00000000-0000-4000-8000-000000000001.jsonl"
	lines := []string{`{"seq":1,"type":"session_start"}`, `{"seq":2,"type":"run_start"}`, `{"seq":3,"type":"turn_start"}`}
	data := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newHandler(dir))
	defer server.Close()
	request, _ := http.NewRequest("GET", server.URL+"/api/events?name="+name, nil)
	request.Header.Set("Last-Event-ID", strconv.Itoa(len(lines[0])+1))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for index := 1; index < len(lines); index++ {
		id, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		payload, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		blank, err := reader.ReadString('\n')
		if err != nil || blank != "\n" || payload != "data: "+lines[index]+"\n" {
			t.Fatalf("unexpected resumed event: %q %q %v", payload, blank, err)
		}
		end := len(strings.Join(lines[:index+1], "\n")) + 1
		if id != fmt.Sprintf("id: %d\n", end) {
			t.Fatalf("id=%q, expected cursor %d", id, end)
		}
	}
}

func TestEventsRejectInvalidCursorAndResetTruncatedCursor(t *testing.T) {
	dir := t.TempDir()
	name := "2026-10-13-00000000-0000-4000-8000-000000000001.jsonl"
	data := "{\"seq\":1,\"type\":\"session_start\"}\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{"invalid", "-1", "5"} {
		request := httptest.NewRequest("GET", "/api/events?name="+name, nil)
		request.Header.Set("Last-Event-ID", cursor)
		response := httptest.NewRecorder()
		newHandler(dir).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("cursor=%q status=%d", cursor, response.Code)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	request := httptest.NewRequest("GET", "/api/events?name="+name, nil).WithContext(ctx)
	request.Header.Set("Last-Event-ID", "9999")
	response := &cancelFlushRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, cancelAt: 2}
	newHandler(dir).ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), "id: 0\nevent: reset\ndata: {}\n\n") || !strings.Contains(response.Body.String(), "data: "+strings.TrimSpace(data)) {
		t.Fatal("truncated cursor did not reset and replay")
	}
}

type cancelFlushRecorder struct {
	*httptest.ResponseRecorder
	cancel   context.CancelFunc
	flushes  int
	cancelAt int
}

func (w *cancelFlushRecorder) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
	if w.flushes == w.cancelAt {
		w.cancel()
	}
}

func TestEventsFlushBatchesRatherThanEveryRecord(t *testing.T) {
	dir := t.TempDir()
	name := "2026-10-13-00000000-0000-4000-8000-000000000001.jsonl"
	var data strings.Builder
	for seq := 1; seq <= 300; seq++ {
		fmt.Fprintf(&data, "{\"seq\":%d,\"type\":\"text_delta\"}\n", seq)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest("GET", "/api/events?name="+name, nil).WithContext(ctx)
	response := &cancelFlushRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, cancelAt: 4}
	newHandler(dir).ServeHTTP(response, request)
	if response.flushes != 4 || strings.Count(response.Body.String(), "data: ") != 300 {
		t.Fatalf("flushes=%d records=%d", response.flushes, strings.Count(response.Body.String(), "data: "))
	}
}

func TestEventsEmptyFileConnectsWithoutWaitingForFirstRecord(t *testing.T) {
	dir := t.TempDir()
	name := "2026-10-13-00000000-0000-4000-8000-000000000001.jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest("GET", "/api/events?name="+name, nil).WithContext(ctx)
	response := &cancelFlushRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, cancelAt: 1}
	newHandler(dir).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.flushes != 1 {
		t.Fatal("empty file did not establish the SSE connection")
	}
	if body, err := io.ReadAll(response.Result().Body); err != nil || len(body) != 0 {
		t.Fatal("empty file produced fabricated events")
	}
}
