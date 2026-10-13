package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iota "github.com/unimpl/Iota"
)

func TestEventStreamResumesAtNextSequence(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		emit(iota.Delta{Text: "answer"})
		return iota.Response{Content: "answer"}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "run", Action: "run", Prompt: "question"}, 202)
	waitState(t, httpServer, name, "completed")
	var cursor uint64
	for _, after := range []string{"1", ""} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		url := httpServer.URL + "/api/sessions/" + name + "/events"
		if after != "" {
			url += "?after=" + after
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if after == "" {
			req.Header.Set("Last-Event-ID", fmt.Sprint(cursor))
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("X-Accel-Buffering") != "no" {
			t.Fatal("gateway buffering was not disabled")
		}
		reader := bufio.NewReader(response.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var record Record
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &record) != nil || record.Sequence == 0 {
				continue
			}
			want := uint64(2)
			if after == "" {
				want = cursor + 1
			}
			if record.Sequence != want {
				t.Fatalf("sequence=%d want=%d", record.Sequence, want)
			}
			cursor = record.Sequence
			break
		}
		response.Body.Close()
		cancel()
	}
}

func TestSessionsUseIndependentToolDirectories(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		var last iota.Message
		for _, message := range request.Messages {
			if !message.RuntimeContext {
				last = message
			}
		}
		if last.Role == iota.RoleTool {
			return iota.Response{Content: last.Content}, nil
		}
		return iota.Response{ToolCalls: []iota.ToolCall{{ID: "read", Name: "read", Arguments: json.RawMessage(`{"path":"identity.txt"}`)}}}, nil
	}}
	_, httpServer := testServer(t, provider)
	for _, identity := range []string{"project A", "project B"} {
		cwd := t.TempDir()
		if err := os.WriteFile(filepath.Join(cwd, "identity.txt"), []byte(identity), 0o600); err != nil {
			t.Fatal(err)
		}
		name := createSession(t, httpServer, cwd)
		request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "read", Action: "run", Prompt: "read identity"}, 202)
		waitState(t, httpServer, name, "completed")
		data := request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
		if !strings.Contains(string(data), identity) {
			t.Fatalf("tool used the wrong directory: %s", data)
		}
	}
}

func TestUnfinishedRequestIsNeverReexecutedOnReopen(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		t.Error("restoration called provider")
		return iota.Response{}, nil
	}}
	s, httpServer := testServer(t, provider)
	session, err := iota.NewSession(s.folder, iota.SessionInfo{Model: "test", CWD: s.config.Settings.CWD})
	if err != nil {
		t.Fatal(err)
	}
	command := commandRequest{ID: "interrupted", Action: "run", Prompt: "may have changed files"}
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMetadata("client_request", payload); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(session.Path())
	request(t, httpServer, "POST", "sessions/"+name+"/open", struct{}{}, 200)
	state := waitState(t, httpServer, name, "interrupted")
	if state.Error == "" {
		t.Fatal("interruption was hidden")
	}
	data := request(t, httpServer, "POST", "sessions/"+name+"/commands", command, 200)
	if !strings.Contains(string(data), `"status":"interrupted"`) {
		t.Fatalf("result=%s", data)
	}
	request(t, httpServer, "POST", "sessions/"+name+"/close", struct{}{}, 204)
	before, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	request(t, httpServer, "POST", "sessions/"+name+"/open", struct{}{}, 200)
	after, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("opening repeated interruption metadata")
	}
}

func TestMutationRejectsMalformedBodies(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		return iota.Response{}, nil
	}}
	_, httpServer := testServer(t, provider)
	for _, body := range []string{`{} {}`, `{"unknown":true}`, strings.Repeat(" ", 1024*1024) + `{}`} {
		req, err := http.NewRequest("POST", httpServer.URL+"/api/sessions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Iota-Client", "web")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("malformed body status=%d", response.StatusCode)
		}
	}
}

func TestHistoryKeepsConversationWithoutRepeatedProviderData(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		emit(iota.Delta{Text: "answer", RawChunk: "provider-only-marker"})
		return iota.Response{Content: "answer"}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "one", Action: "run", Prompt: "question"}, 202)
	waitState(t, httpServer, name, "completed")
	history := request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
	if strings.Contains(string(history), "provider-only-marker") || strings.Contains(string(history), `"request":`) || !strings.Contains(string(history), "answer") {
		t.Fatalf("chat projection=%s", history)
	}
	original, err := os.ReadFile(filepath.Join(s.folder, name))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(original), "provider-only-marker") || !strings.Contains(string(original), `"request":`) {
		t.Fatal("original SDK trace was changed")
	}
}
