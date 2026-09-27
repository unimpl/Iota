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
	receivedBody := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		receivedBody <- string(body)
		if !json.Valid(body) {
			t.Errorf("invalid request body: %s", body)
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
	var toolCallDeltas []iota.ToolCallDelta
	var rawChunks []string
	response, err := provider.Stream(context.Background(), iota.Request{Model: "test", Messages: []iota.Message{{Role: iota.RoleUser, Content: "go"}}}, func(delta iota.Delta) {
		deltas.WriteString(delta.Text)
		if delta.RawChunk != "" {
			rawChunks = append(rawChunks, delta.RawChunk)
		}
		if delta.ToolCall != nil {
			toolCallDeltas = append(toolCallDeltas, *delta.ToolCall)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "hi " || deltas.String() != "hi " || response.StopReason != "tool_calls" {
		t.Fatalf("unexpected response: %+v, deltas=%q", response, deltas.String())
	}
	encodedBody, err := provider.EncodeRequest(iota.Request{Model: "test", Messages: []iota.Message{{Role: iota.RoleUser, Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if sentBody := <-receivedBody; string(encodedBody) != sentBody || !strings.Contains(sentBody, `"stream":true`) {
		t.Fatalf("raw request differs from sent body: %q / %q", encodedBody, sentBody)
	}
	if len(rawChunks) != 5 || !strings.Contains(rawChunks[0], `"content":"hi "`) ||
		!strings.Contains(rawChunks[3], `"usage"`) || rawChunks[4] != "data: [DONE]" {
		t.Fatalf("raw chunks = %q", rawChunks)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "read" || string(response.ToolCalls[0].Arguments) != `{"path":"x"}` {
		t.Fatalf("tool calls = %+v", response.ToolCalls)
	}
	if len(toolCallDeltas) != 2 || toolCallDeltas[0].Index != 0 || toolCallDeltas[0].ID != "call-1" ||
		toolCallDeltas[0].Name != "re" || toolCallDeltas[0].Arguments != `{"pa` ||
		toolCallDeltas[1].Name != "ad" || toolCallDeltas[1].Arguments != `th":"x"}` {
		t.Fatalf("tool call deltas = %+v", toolCallDeltas)
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
