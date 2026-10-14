package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	path, ok := s.path(w, r)
	if !ok {
		return
	}
	values := r.URL.Query()
	for key, value := range values {
		if (key != "before" && key != "run" && key != "at") || len(value) != 1 || value[0] == "" {
			fail(w, 400, errors.New("history accepts before or run with a run ID and an optional at cursor"))
			return
		}
	}
	query := historyQuery{Before: values.Get("before"), Run: values.Get("run")}
	if value := values.Get("at"); value != "" {
		var err error
		query.At, err = strconv.ParseUint(value, 10, 64)
		if err != nil || query.At == 0 {
			fail(w, 400, errors.New("at must be a positive history cursor"))
			return
		}
	}
	if query.Before != "" && query.Run != "" {
		fail(w, 400, errors.New("before and run cannot be combined"))
		return
	}
	snapshot, err := readHistory(r.Context(), path, query)
	if err != nil {
		fail(w, 400, err)
		return
	}
	sendJSON(w, 200, snapshot)
}

// Each connection follows the durable log independently; slow subscribers never
// block SDK callbacks. Sequence IDs make history + streaming gap-free.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	path, ok := s.path(w, r)
	if !ok {
		return
	}
	id := r.Header.Get("Last-Event-ID")
	if id == "" {
		id = r.URL.Query().Get("after")
	}
	var after uint64
	if id != "" {
		var err error
		after, err = strconv.ParseUint(id, 10, 64)
		if err != nil {
			fail(w, 400, errors.New("invalid event cursor"))
			return
		}
	}
	interval := time.Second
	if value := r.URL.Query().Get("interval_ms"); value != "" {
		ms, err := strconv.ParseUint(value, 10, 64)
		if err != nil || ms < 50 || ms > 10000 {
			fail(w, 400, errors.New("interval_ms must be an integer from 50 to 10000"))
			return
		}
		interval = time.Duration(ms) * time.Millisecond
	}
	file, err := os.Open(path)
	if err != nil {
		fail(w, 404, err)
		return
	}
	defer file.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, errors.New("streaming unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	wake := s.subscribeEvents(path)
	defer s.unsubscribeEvents(path, wake)
	flusher.Flush()
	// CLI-owned sessions have no callback here; polling remains a fallback.
	ticker := time.NewTicker(min(interval, 250*time.Millisecond))
	defer ticker.Stop()
	timer := time.NewTimer(interval)
	timer.Stop()
	defer timer.Stop()
	reader := bufio.NewReader(file)
	var offset int64
	lastState := ""
	heartbeat := time.Now()
	lastSend := time.Now()
	sentCursor := after
	buffer := eventBuffer{cursor: after}
	flush := func() error {
		if buffer.cursor <= sentCursor {
			return nil
		}
		batch := buffer.take()
		data, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: records\ndata: %s\n\n", batch.Cursor, data); err != nil {
			return err
		}
		flusher.Flush()
		sentCursor, lastSend = batch.Cursor, time.Now()
		return nil
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		default:
		}
		count := 0
		for count < 2048 {
			line, err := reader.ReadBytes('\n')
			if errors.Is(err, io.EOF) {
				if _, err := file.Seek(offset, io.SeekStart); err != nil {
					return
				}
				reader.Reset(file)
				break
			}
			if err != nil {
				return
			}
			offset += int64(len(line))
			count++
			var record Record
			if json.Unmarshal(line, &record) != nil {
				return
			}
			if record.Sequence <= after {
				continue
			}
			immediate, err := buffer.add(record)
			if err != nil {
				return
			}
			after = record.Sequence
			if immediate {
				if err := flush(); err != nil {
					return
				}
			}
		}
		if time.Since(lastSend) >= interval {
			if err := flush(); err != nil {
				return
			}
		}
		if count == 2048 {
			continue
		}
		state, _ := json.Marshal(s.snapshot(r.PathValue("name"), path))
		if string(state) != lastState {
			// Pending questions, cancellation and failures bypass the interval,
			// after delivering all buffered content that precedes the state.
			if err := flush(); err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", state); err != nil {
				return
			}
			lastState = string(state)
			lastSend = time.Now()
			flusher.Flush()
		}
		if time.Since(heartbeat) > 15*time.Second {
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			heartbeat = time.Now()
			flusher.Flush()
		}
		var due <-chan time.Time
		if buffer.cursor > sentCursor {
			timer.Reset(max(0, time.Until(lastSend.Add(interval))))
			due = timer.C
		} else {
			timer.Stop()
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		case <-due:
		}
	}
}
