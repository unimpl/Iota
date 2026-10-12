package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	iota "github.com/unimpl/Iota"
)

func TestStreamCanContinueLongerThanIdleTimeout(t *testing.T) {
	const timeout = 350 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 16; i++ {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"thinking\"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Millisecond):
			}
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	provider, err := New(Config{BaseURL: server.URL, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started := time.Now()
	var reasoning strings.Builder
	response, err := provider.Stream(ctx, iota.Request{Model: "test"}, func(delta iota.Delta) { reasoning.WriteString(delta.Reasoning) })
	if err != nil || response.Content != "ok" || response.StopReason != "stop" || reasoning.String() != strings.Repeat("thinking", 16) {
		t.Fatalf("active stream failed: response=%+v err=%v", response, err)
	}
	if time.Since(started) <= timeout {
		t.Fatal("fixture did not exceed the old total request timeout")
	}
}

func TestStreamIdleTimeoutAbortsIncompleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	provider, err := New(Config{BaseURL: server.URL, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var partial string
	response, err := provider.Stream(ctx, iota.Request{Model: "test"}, func(delta iota.Delta) { partial += delta.Text })
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "response idle timeout") || partial != "partial" || response.Content != "" || len(response.ToolCalls) != 0 {
		t.Fatalf("idle timeout accepted incomplete response: %+v partial=%q err=%v", response, partial, err)
	}
}

func TestStreamBoundsResponseHeadersAndFirstBodyWait(t *testing.T) {
	for _, flushHeaders := range []bool{false, true} {
		t.Run(fmt.Sprint(flushHeaders), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if flushHeaders {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			provider, err := New(Config{BaseURL: server.URL, Timeout: 100 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			_, err = provider.Stream(ctx, iota.Request{Model: "test"}, nil)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "response idle timeout") {
				t.Fatalf("response wait was unbounded: %v", err)
			}
		})
	}
}

func TestStreamHeartbeatsAndPartialEventsResetIdleTimeout(t *testing.T) {
	for _, kind := range []string{"heartbeats", "partial_event"} {
		t.Run(kind, func(t *testing.T) {
			const timeout = 250 * time.Millisecond
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				text := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"
				var fragments []string
				if kind == "heartbeats" {
					for i := 0; i < 15; i++ {
						fragments = append(fragments, ": heartbeat\n\n")
					}
				} else {
					for i := 0; i < len(text); i += 4 {
						fragments = append(fragments, text[i:min(i+4, len(text))])
					}
				}
				for _, fragment := range fragments {
					io.WriteString(w, fragment)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(25 * time.Millisecond):
					}
				}
				if kind == "heartbeats" {
					io.WriteString(w, text)
				}
				io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			provider, err := New(Config{BaseURL: server.URL, Timeout: timeout})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			started := time.Now()
			response, err := provider.Stream(ctx, iota.Request{Model: "test"}, nil)
			if err != nil || response.Content != "ok" || time.Since(started) <= timeout {
				t.Fatalf("received bytes did not keep stream alive: response=%+v err=%v", response, err)
			}
		})
	}
}

func TestStreamStillRespectsCallerCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			provider, err := New(Config{BaseURL: server.URL, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			_, err = provider.Stream(ctx, iota.Request{Model: "test"}, func(delta iota.Delta) {
				if !deadline && delta.Text != "" {
					cancel()
				}
			})
			want := error(context.Canceled)
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || strings.Contains(err.Error(), "response idle timeout") {
				t.Fatalf("caller context was ignored or mislabeled: %v", err)
			}
		})
	}
}

func TestIdleTimerRechecksDeadlineForStaleCallbackAndStopsCleanly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		idle := newResponseIdleTimer(time.Second, cancel)
		defer idle.stop()
		time.Sleep(500 * time.Millisecond)
		idle.touch()
		idle.expire() // Simulate an old AfterFunc callback racing with a successful read.
		if ctx.Err() != nil {
			t.Fatal("stale callback canceled an active stream")
		}
		time.Sleep(750 * time.Millisecond)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatal("timer did not reset on activity")
		}
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			t.Fatal("silence did not cancel request")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		idle := newResponseIdleTimer(time.Second, cancel)
		idle.stop()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatal("stopped timer canceled a completed request")
		}
	})
}

func TestHTTP2IdleTimeoutCancelsOnlyStalledRequest(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("fixture did not use HTTP/2")
		}
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		if body.Model == "stalled" {
			<-r.Context().Done()
			return
		}
		for i := 0; i < 16; i++ {
			io.WriteString(w, ": heartbeat\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Millisecond):
			}
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	provider, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client(), Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	type outcome struct {
		model    string
		response iota.Response
		err      error
	}
	results := make(chan outcome, 2)
	for _, model := range []string{"stalled", "active"} {
		go func() {
			response, err := provider.Stream(ctx, iota.Request{Model: model}, nil)
			results <- outcome{model, response, err}
		}()
	}
	first, second := <-results, <-results
	if first.model != "stalled" || !errors.Is(first.err, context.DeadlineExceeded) || !strings.Contains(first.err.Error(), "response idle timeout") || second.model != "active" || second.err != nil || second.response.Content != "ok" {
		t.Fatalf("shared connection or timer affected another request: first=%+v second=%+v", first, second)
	}
}
