package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestStreamTextAndFragmentedToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"hi \"}}]}\n\n")
		io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"re\",\"arguments\":\"{\\\"pa\"}}]}}]}\n\n")
		io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"ad\",\"arguments\":\"th\\\":\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
		io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	provider, err := New(Config{BaseURL: server.URL + "/v1", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	response, err := provider.Stream(context.Background(), iota.Request{Model: "test", Messages: []iota.Message{{Role: iota.RoleUser, Content: "go"}}}, func(delta iota.Delta) {
		deltas.WriteString(delta.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "hi " || deltas.String() != "hi " || response.StopReason != "tool_calls" {
		t.Fatalf("unexpected response: %+v, deltas=%q", response, deltas.String())
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "read" || string(response.ToolCalls[0].Arguments) != `{"path":"x"}` {
		t.Fatalf("tool calls = %+v", response.ToolCalls)
	}
	if response.Usage == nil || response.Usage.TotalTokens != 3 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

func TestStreamRejectsUnexpectedEOFAndHTTPError(t *testing.T) {
	t.Run("eof", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		}))
		defer server.Close()
		provider, _ := New(Config{BaseURL: server.URL})
		_, err := provider.Stream(context.Background(), iota.Request{Model: "test"}, nil)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("http", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "bad key", http.StatusUnauthorized)
		}))
		defer server.Close()
		provider, _ := New(Config{BaseURL: server.URL})
		_, err := provider.Stream(context.Background(), iota.Request{Model: "test"}, nil)
		if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestStreamAllowsMissingUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	provider, _ := New(Config{BaseURL: server.URL})
	response, err := provider.Stream(context.Background(), iota.Request{Model: "test"}, nil)
	if err != nil || response.Usage != nil || response.Content != "ok" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
