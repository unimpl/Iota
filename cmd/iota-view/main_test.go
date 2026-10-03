package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDeleteSession 验证只删除选中的普通日志，拒绝越界、跨域及符号链接。
func TestDeleteSession(t *testing.T) {
	name := "2026-10-03-00000000-0000-4000-8000-000000000001.jsonl"
	for _, test := range []struct {
		name   string
		method string
		query  string
		kind   string
		header string
		origin string
		status int
	}{
		{name: "delete", method: "DELETE", query: name, kind: "file", header: "1", status: http.StatusNoContent},
		{name: "same origin", method: "DELETE", query: name, kind: "file", header: "1", origin: "http://localhost:9280", status: http.StatusNoContent},
		{name: "missing", method: "DELETE", query: name, header: "1", status: http.StatusNotFound},
		{name: "traversal", method: "DELETE", query: "../" + name, kind: "file", header: "1", status: http.StatusBadRequest},
		{name: "directory", method: "DELETE", query: name, kind: "directory", header: "1", status: http.StatusBadRequest},
		{name: "symlink", method: "DELETE", query: name, kind: "symlink", header: "1", status: http.StatusBadRequest},
		{name: "missing header", method: "DELETE", query: name, kind: "file", status: http.StatusForbidden},
		{name: "cross origin", method: "DELETE", query: name, kind: "file", header: "1", origin: "https://example.com", status: http.StatusForbidden},
		{name: "wrong method", method: "POST", query: name, kind: "file", header: "1", status: http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			outside := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch test.kind {
			case "file":
				if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequest(test.method, "http://localhost:9280/api/sessions?name="+url.QueryEscape(test.query), nil)
			request.Header.Set("X-Iota-Delete", test.header)
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			newHandler(dir).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			_, err := os.Lstat(path)
			if test.status == http.StatusNoContent {
				if !os.IsNotExist(err) {
					t.Fatalf("file remains: %v", err)
				}
			} else if test.kind != "" && err != nil {
				t.Fatalf("rejected deletion changed file: %v", err)
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "untouched" {
				t.Fatalf("outside file changed: %q %v", data, err)
			}
		})
	}
}

// TestSessionListSortsByCreationAndStreamWaitsForCompleteLine 验证排序稳定且尾部半行不会提前推送。
func TestSessionListSortsByCreationAndStreamWaitsForCompleteLine(t *testing.T) {
	dir := t.TempDir()
	older := "2026-09-01-00000000-0000-4000-8000-000000000001.jsonl"
	newer := "2026-09-01-00000000-0000-4000-8000-000000000002.jsonl"
	for name, timestamp := range map[string]string{older: "2026-09-01T10:00:00Z", newer: "2026-09-01T11:00:00Z"} {
		data := fmt.Sprintf("{\"type\":\"session_start\",\"timestamp\":%q,\"payload\":{\"model\":\"test\"}}\n", timestamp)
		if name == newer {
			data += "{\"type\":\"message_added\",\"payload\":{\"message\":{\"role\":\"user\",\"content\":\"  请帮我检查这个项目中的会话观察功能是否正常工作\"}}}\n"
			data += "{\"type\":\"message_added\",\"payload\":{\"message\":{\"role\":\"user\",\"content\":\"第二个问题\"}}}\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(newHandler(dir))
	defer server.Close()
	response, err := http.Get(server.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var sessions []sessionInfo
	if err := json.NewDecoder(response.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Name != newer {
		t.Fatalf("sessions: %+v", sessions)
	}
	if sessions[0].Title != "请帮我检查这个项目中的会话观察功能是否正…" || sessions[1].Title != "" {
		t.Fatalf("titles: %+v", sessions)
	}

	stream, err := http.Get(server.URL + "/api/events?name=" + newer)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	lines := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(stream.Body)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data: ") {
				lines <- scanner.Text()
			}
		}
	}()
	select {
	case line := <-lines:
		if !strings.Contains(line, "session_start") {
			t.Fatalf("first line: %s", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initial event not delivered")
	}
	for range 2 {
		select {
		case <-lines:
		case <-time.After(2 * time.Second):
			t.Fatal("initial messages not delivered")
		}
	}
	file, err := os.OpenFile(filepath.Join(dir, newer), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := io.WriteString(file, `{"type":"run_start"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-lines:
		t.Fatalf("incomplete line delivered: %s", line)
	case <-time.After(400 * time.Millisecond):
	}
	if _, err := io.WriteString(file, "\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-lines:
		if !strings.Contains(line, "run_start") {
			t.Fatalf("appended line: %s", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("appended event not delivered")
	}
}

// TestListenAvailablePortSkipsOccupiedPorts 验证查看器顺延跳过已占用端口。
func TestListenAvailablePortSkipsOccupiedPorts(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	start := first.Addr().(*net.TCPAddr).Port
	if start >= 65534 {
		t.Skip("chosen port is too close to the upper limit")
	}
	second, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", start+1))
	if err != nil {
		t.Skipf("next port is already occupied: %v", err)
	}
	defer second.Close()

	listener, err := listenAvailablePort(start)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if got := listener.Addr().(*net.TCPAddr).Port; got < start+2 {
		t.Fatalf("got port %d, want at least %d", got, start+2)
	}
}
