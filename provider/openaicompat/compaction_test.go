package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestStreamPreservesContextOverflowCode(t *testing.T) {
	stream := strings.NewReader("data: {\"error\":{\"type\":\"invalid_request_error\",\"code\":\"context_length_exceeded\",\"message\":\"capacity reached\"}}\n\n")
	_, err := consumeStream(stream, nil)
	if err == nil || !strings.Contains(err.Error(), "context_length_exceeded") || !strings.Contains(err.Error(), "capacity reached") {
		t.Fatalf("error=%v", err)
	}
}

func TestAgentRecoversFromHTTPAndStreamCapacityErrors(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "stream"}[streaming], func(t *testing.T) {
			var mu sync.Mutex
			var requests []requestBody
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request requestBody
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				requests = append(requests, request)
				call := len(requests)
				mu.Unlock()
				if call == 5 {
					body := `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"maximum context length is 128000 tokens"}}`
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: "+body+"\n\n")
					} else {
						w.WriteHeader(http.StatusBadRequest)
						io.WriteString(w, body)
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"checkpoint or answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			provider, err := New(Config{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := iota.New(iota.Config{Provider: provider, Model: "test", SystemPrompt: "project instructions"})
			if err != nil {
				t.Fatal(err)
			}
			for _, prompt := range []string{strings.Repeat("old context ", 1000), "one", "two", "three", "next"} {
				if _, err := agent.Run(context.Background(), prompt, nil); err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 7 || !strings.Contains(requests[5].Messages[0].Content, "CONTEXT CHECKPOINT COMPACTION") || len(requests[5].Tools) != 0 || !strings.Contains(requests[6].Messages[0].Content, "project instructions") || !strings.Contains(requests[6].Messages[1].Content, "<summary>") {
				t.Fatalf("incorrect recovery request sequence: %d calls", len(requests))
			}
		})
	}
}
