package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/internal/app"
	"github.com/unimpl/Iota/tools"
)

type testProvider struct {
	stream func(context.Context, iota.Request, func(iota.Delta)) (iota.Response, error)
}

func (p testProvider) Stream(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
	return p.stream(ctx, request, emit)
}

func testServer(t *testing.T, provider iota.Provider) (*Server, *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	settings, err := app.SettingsFromConfig(app.FileConfig{Model: "test", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Home: home, Settings: settings, NewAgent: func(settings app.Settings, home string, handler iota.UserInputHandler, status func(string)) (*iota.Agent, error) {
		return iota.New(iota.Config{Provider: provider, Model: settings.Model, Mode: iota.Mode(settings.Mode), DisableSkills: true, PlansDir: filepath.Join(home, ".iota", "plans"), UserInputHandler: handler, Tools: []iota.Tool{tools.NewRead(settings.CWD), iota.NewRequestUserInputTool(), iota.NewSavePlanTool(), iota.NewUpdateProgressTool()}})
	}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); httpServer.Close() })
	return s, httpServer
}

func request(t *testing.T, server *httptest.Server, method, path string, body interface{}, status int) []byte {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, server.URL+"/api/"+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Iota-Client", "web")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, response.StatusCode, status, data)
	}
	return data
}

func createSession(t *testing.T, server *httptest.Server, cwd string) string {
	t.Helper()
	data := request(t, server, "POST", "sessions", struct {
		CWD string `json:"cwd"`
	}{cwd}, 201)
	var result struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result.Name
}

func waitState(t *testing.T, server *httptest.Server, name, status string) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data := request(t, server, "GET", "sessions/"+name+"/state", nil, 200)
		var state State
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		if state.Status == status {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session did not reach %s", status)
	return State{}
}

func TestCommandsContinueAfterDisconnectAndDeduplicate(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	resume := make(chan struct{})
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		calls.Add(1)
		close(started)
		select {
		case <-resume:
		case <-ctx.Done():
			return iota.Response{}, ctx.Err()
		}
		emit(iota.Delta{Text: "answer"})
		return iota.Response{Content: "answer"}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	command := commandRequest{ID: "request-1", Action: "run", Prompt: "question"}
	request(t, httpServer, "POST", "sessions/"+name+"/commands", command, 202)
	<-started
	// The command HTTP response has closed; its lifetime must not cancel the Run.
	request(t, httpServer, "POST", "sessions/"+name+"/commands", command, 200)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "request-2", Action: "run", Prompt: "other"}, 409)
	request(t, httpServer, "POST", "sessions/"+name+"/close", struct{}{}, 409)
	request(t, httpServer, "DELETE", "sessions/"+name, nil, 409)
	close(resume)
	waitState(t, httpServer, name, "completed")
	request(t, httpServer, "POST", "sessions/"+name+"/close", struct{}{}, 204)
	request(t, httpServer, "POST", "sessions/"+name+"/open", struct{}{}, 200)
	data := request(t, httpServer, "POST", "sessions/"+name+"/commands", command, 200)
	if !bytes.Contains(data, []byte(`"status":"completed"`)) || calls.Load() != 1 {
		t.Fatalf("calls=%d response=%s", calls.Load(), data)
	}
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: command.ID, Action: "run", Prompt: "different"}, 409)
}

func TestCLIWebOwnershipAndWorkingDirectory(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		return iota.Response{Content: "done"}, nil
	}}
	s, httpServer := testServer(t, provider)
	cwd := t.TempDir()
	session, err := iota.NewSession(s.folder, iota.SessionInfo{Model: "test", CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(session.Path())
	request(t, httpServer, "POST", "sessions/"+name+"/open", struct{}{}, 409)
	// Read-only preview succeeds without opening or repairing a CLI-owned session.
	request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
	request(t, httpServer, "DELETE", "sessions/"+name, nil, 409)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	request(t, httpServer, "POST", "sessions/"+name+"/open", struct{}{}, 200)
	state := waitState(t, httpServer, name, "idle")
	if state.CWD != cwd {
		t.Fatalf("cwd=%s want=%s", state.CWD, cwd)
	}
	if _, err := iota.OpenSession(session.Path()); !errors.Is(err, iota.ErrSessionOccupied) {
		t.Fatalf("CLI opened web session: %v", err)
	}
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "plan", Action: "run", Prompt: "/plan"}, 202)
	waitState(t, httpServer, name, "completed")
	request(t, httpServer, "POST", "sessions/"+name+"/close", struct{}{}, 204)
	cli, err := iota.OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test", DisableSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Restore(agent); err != nil {
		t.Fatal(err)
	}
	if agent.Mode() != iota.ModePlan {
		t.Fatal("Web mode did not restore in CLI")
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	request(t, httpServer, "DELETE", "sessions/"+name, nil, 204)
}

func TestWebQuestionSurvivesRefreshAndRejectsStaleAnswers(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		for _, message := range request.Messages {
			if message.Role == iota.RoleTool {
				return iota.Response{Content: "answered"}, nil
			}
		}
		return iota.Response{ToolCalls: []iota.ToolCall{{ID: "question", Name: "request_user_input", Arguments: json.RawMessage(`{"format":"freeform","questions":[{"id":"q","question":"What should I change?"}]}`)}}}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "ask", Action: "run", Prompt: "ask me"}, 202)
	first := waitState(t, httpServer, name, "waiting_input")
	request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
	refreshed := waitState(t, httpServer, name, "waiting_input")
	if first.Pending == nil || refreshed.Pending == nil || first.Pending.ID != refreshed.Pending.ID {
		t.Fatal("pending question lost on refresh")
	}
	answer := struct {
		ID       string                 `json:"id"`
		Response iota.UserInputResponse `json:"response"`
	}{ID: refreshed.Pending.ID, Response: iota.UserInputResponse{Answers: map[string]iota.UserInputAnswer{"q": {Source: "custom", Value: "fix tests"}}}}
	invalid := answer
	invalid.Response = iota.UserInputResponse{}
	request(t, httpServer, "POST", "sessions/"+name+"/answers", invalid, 400)
	request(t, httpServer, "POST", "sessions/"+name+"/answers", answer, 204)
	waitState(t, httpServer, name, "completed")
	request(t, httpServer, "POST", "sessions/"+name+"/answers", answer, 409)
	data := request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
	if !bytes.Contains(data, []byte("fix tests")) {
		t.Fatal("answer was not saved in history")
	}
}

func TestStopAndShutdownReleaseSessions(t *testing.T) {
	started := make(chan struct{}, 2)
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return iota.Response{}, ctx.Err()
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "one", Action: "run", Prompt: "wait"}, 202)
	<-started
	request(t, httpServer, "POST", "sessions/"+name+"/stop", struct{}{}, 204)
	waitState(t, httpServer, name, "aborted")
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "two", Action: "run", Prompt: "wait"}, 202)
	<-started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	session, err := iota.OpenSession(filepath.Join(s.folder, name))
	if err != nil {
		t.Fatalf("shutdown retained lock: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayOriginAndCredentialsStayServerSide(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		return iota.Response{}, nil
	}}
	s, httpServer := testServer(t, provider)
	s.config.PublicOrigin = "https://iota.example"
	s.config.Settings.APIKey = "secret-never-send"
	data := request(t, httpServer, "GET", "config", nil, 200)
	if bytes.Contains(data, []byte("secret")) {
		t.Fatal("configuration exposed credentials")
	}
	for _, origin := range []string{"https://iota.example", "https://other.example"} {
		req, err := http.NewRequest("POST", httpServer.URL+"/api/sessions", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Iota-Client", "web")
		req.Header.Set("Origin", origin)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := 201
		if origin != "https://iota.example" {
			want = 403
		}
		if response.StatusCode != want {
			t.Fatalf("origin %s status=%d", origin, response.StatusCode)
		}
	}
	req, err := http.NewRequest("POST", httpServer.URL+"/api/sessions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("missing client header was accepted")
	}
}

func TestHistorySnapshotSkipsMetadataAndIncompleteTail(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		return iota.Response{}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	owned := s.sessions[name]
	for index := 0; index < 210; index++ {
		if err := owned.session.AppendMetadata("client_request", json.RawMessage(`{"request_id":"unused"}`)); err != nil {
			t.Fatal(err)
		}
	}
	var snapshot historySnapshot
	data := request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 || snapshot.Cursor != 211 || strings.Contains(string(data), `"records"`) {
		t.Fatalf("snapshot=%s", data)
	}
	request(t, httpServer, "GET", "sessions/"+name+"/history?after=oops", nil, 400)
	// A complete header followed by a partial record stays invisible to readers.
	path := filepath.Join(s.folder, name)
	if err := owned.session.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	data = request(t, httpServer, "GET", "sessions/"+name+"/history", nil, 200)
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor != 212 {
		t.Fatalf("incomplete tail advanced cursor: %d", snapshot.Cursor)
	}
}
