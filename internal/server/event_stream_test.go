package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	iota "github.com/unimpl/Iota"
)

func connectStream(t *testing.T, httpServer *httptest.Server, name, query, lastID string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL+"/api/sessions/"+name+"/events?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != 200 {
		t.Fatalf("stream status=%d", response.StatusCode)
	}
	return response
}

func decodeBatch(t *testing.T, frame streamFrame) eventBatch {
	t.Helper()
	var batch eventBatch
	if err := json.Unmarshal(frame.Data, &batch); err != nil {
		t.Fatal(err)
	}
	if frame.ID != fmt.Sprint(batch.Cursor) {
		t.Fatalf("SSE ID=%s cursor=%d", frame.ID, batch.Cursor)
	}
	return batch
}

func TestEventStreamThrottlesDeltasButFlushesCompletionImmediately(t *testing.T) {
	chunks := make(chan struct{})
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		for {
			select {
			case _, ok := <-chunks:
				if !ok {
					return iota.Response{Content: strings.Repeat("x", 200)}, nil
				}
				for index := 0; index < 100; index++ {
					emit(iota.Delta{Text: "x", Reasoning: "思", RawChunk: "raw-provider-only"})
				}
			case <-ctx.Done():
				return iota.Response{}, ctx.Err()
			}
		}
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	response := connectStream(t, httpServer, name, "after=1&interval_ms=1000", "")
	reader := bufio.NewReader(response.Body)
	if frame := readStreamFrame(t, reader); frame.Event != "state" {
		t.Fatalf("initial event=%s", frame.Event)
	}
	started := time.Now()
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "run", Action: "run", Prompt: "question"}, 202)
	var turnSeen, runningSeen bool
	for !turnSeen || !runningSeen {
		frame := readStreamFrame(t, reader)
		if frame.Event == "state" {
			var state State
			if err := json.Unmarshal(frame.Data, &state); err != nil {
				t.Fatal(err)
			}
			runningSeen = state.Status == "running"
		} else if frame.Event == "records" {
			for _, record := range decodeBatch(t, frame).Records {
				turnSeen = turnSeen || record.Type == "turn_start"
			}
		}
	}
	if time.Since(started) >= 750*time.Millisecond {
		t.Fatal("start events waited for the ordinary interval")
	}
	t.Logf("start delivered in %s", time.Since(started))
	for burst := 0; burst < 2; burst++ {
		start := time.Now()
		chunks <- struct{}{}
		for {
			frame := readStreamFrame(t, reader)
			if frame.Event != "records" {
				continue
			}
			batch := decodeBatch(t, frame)
			if len(batch.Records) == 0 {
				continue
			}
			if elapsed := time.Since(start); elapsed < 800*time.Millisecond || elapsed > 2*time.Second {
				t.Fatalf("ordinary interval=%s", elapsed)
			}
			if len(batch.Records) != 2 {
				t.Fatalf("200 deltas were not merged: %s", frame.Data)
			}
			for _, record := range batch.Records {
				var event historyEvent
				if err := json.Unmarshal(record.Payload, &event); err != nil {
					t.Fatal(err)
				}
				if record.Type == "text_delta" && event.Text != strings.Repeat("x", 100) {
					t.Fatalf("merged text=%q", event.Text)
				}
				if record.Type == "reasoning_delta" && event.Reasoning != strings.Repeat("思", 100) {
					t.Fatalf("merged reasoning=%q", event.Reasoning)
				}
			}
			if strings.Contains(string(frame.Data), "raw-provider-only") {
				t.Fatal("provider traces leaked")
			}
			t.Logf("200 deltas -> 2 records in one frame after %s", time.Since(start))
			break
		}
	}
	finished := time.Now()
	close(chunks)
	for {
		frame := readStreamFrame(t, reader)
		if frame.Event != "records" {
			continue
		}
		for _, record := range decodeBatch(t, frame).Records {
			if record.Type == "client_result" {
				if time.Since(finished) >= 750*time.Millisecond {
					t.Fatal("completion waited for the ordinary interval")
				}
				t.Logf("completion delivered in %s", time.Since(finished))
				return
			}
		}
	}
}

func TestEventNotificationsBroadcastAndCoalesceWithoutBlocking(t *testing.T) {
	s := &Server{}
	first := s.subscribeEvents("session")
	second := s.subscribeEvents("session")
	unrelated := s.subscribeEvents("other")
	for index := 0; index < 1000; index++ {
		s.notifyEvents("session")
	}
	for _, wake := range []chan struct{}{first, second} {
		select {
		case <-wake:
		default:
			t.Fatal("subscriber missed the wakeup")
		}
		select {
		case <-wake:
			t.Fatal("slow reader accumulated multiple wakeups")
		default:
		}
	}
	select {
	case <-unrelated:
		t.Fatal("notified an unrelated session")
	default:
	}
	s.unsubscribeEvents("session", first)
	s.unsubscribeEvents("session", second)
	s.unsubscribeEvents("other", unrelated)
	if len(s.subscribers) != 0 {
		t.Fatal("disconnected subscribers were retained")
	}
}

func TestEventStreamQuestionAndStopBypassLongInterval(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		emit(iota.Delta{Reasoning: "need clarification"})
		return iota.Response{ToolCalls: []iota.ToolCall{{ID: "ask", Name: "request_user_input", Arguments: json.RawMessage(`{"format":"freeform","questions":[{"id":"q","question":"Which file?"}]}`)}}}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	response := connectStream(t, httpServer, name, "after=1&interval_ms=10000", "")
	reader := bufio.NewReader(response.Body)
	readStreamFrame(t, reader) // Initial idle state.
	start := time.Now()
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "run", Action: "run", Prompt: "ask me"}, 202)
	var reasoningSeen, toolSeen bool
	for {
		frame := readStreamFrame(t, reader)
		if frame.Event == "records" {
			for _, record := range decodeBatch(t, frame).Records {
				reasoningSeen = reasoningSeen || record.Type == "reasoning_delta"
				toolSeen = toolSeen || record.Type == "tool_start"
			}
		} else if frame.Event == "state" {
			var state State
			if err := json.Unmarshal(frame.Data, &state); err != nil {
				t.Fatal(err)
			}
			if state.Pending != nil {
				if !reasoningSeen || !toolSeen || time.Since(start) >= time.Second {
					t.Fatal("question was delayed or overtook buffered reasoning/tool events")
				}
				break
			}
		}
	}
	start = time.Now()
	request(t, httpServer, "POST", "sessions/"+name+"/stop", struct{}{}, 204)
	for {
		frame := readStreamFrame(t, reader)
		if frame.Event != "records" {
			continue
		}
		for _, record := range decodeBatch(t, frame).Records {
			if record.Type == "run_end" {
				if time.Since(start) >= time.Second {
					t.Fatal("stop was delayed by the ordinary interval")
				}
				return
			}
		}
	}
}

func TestEventStreamReconnectAfterMergedBatch(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		for index := 0; index < 100; index++ {
			emit(iota.Delta{Text: "x", Reasoning: "思"})
		}
		return iota.Response{Content: "finished"}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "run", Action: "run", Prompt: "question"}, 202)
	waitState(t, httpServer, name, "completed")
	response := connectStream(t, httpServer, name, "after=1&interval_ms=10000", "")
	reader := bufio.NewReader(response.Body)
	var cursor uint64
	for cursor == 0 {
		frame := readStreamFrame(t, reader)
		if frame.Event != "records" {
			continue
		}
		batch := decodeBatch(t, frame)
		for _, record := range batch.Records {
			if record.Type == "message_added" && strings.Contains(string(record.Payload), "finished") {
				if len(batch.Records) != 3 {
					t.Fatalf("expected text, reasoning and final message in one batch: %s", frame.Data)
				}
				cursor = batch.Cursor
			}
		}
	}
	response.Body.Close()
	// Last-Event-ID takes priority over a stale query parameter.
	response = connectStream(t, httpServer, name, "after=0&interval_ms=10000", fmt.Sprint(cursor))
	reader = bufio.NewReader(response.Body)
	for {
		frame := readStreamFrame(t, reader)
		if frame.Event != "records" {
			continue
		}
		batch := decodeBatch(t, frame)
		if batch.Cursor <= cursor {
			t.Fatal("replayed a committed batch")
		}
		for _, record := range batch.Records {
			if record.Sequence <= cursor || record.Type == "text_delta" || record.Type == "reasoning_delta" || record.Type == "message_added" {
				t.Fatalf("replayed merged content: %s", frame.Data)
			}
			if record.Type == "client_result" {
				return
			}
		}
		cursor = batch.Cursor
	}
}

func TestEventStreamRejectsInvalidIntervals(t *testing.T) {
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		return iota.Response{}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	for _, value := range []string{"0", "49", "10001", "-1", "NaN", "1.5", "18446744073709551615"} {
		request(t, httpServer, "GET", "sessions/"+name+"/events?interval_ms="+value, nil, 400)
	}
}
