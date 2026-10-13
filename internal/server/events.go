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
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		after = 0
		err = nil
	}
	if err != nil {
		fail(w, 400, errors.New("invalid history cursor"))
		return
	}
	records := make([]Record, 0, 200)
	var cursor uint64 = after
	more := false
	err = walkRecords(path, func(record Record) bool {
		if record.Sequence <= after {
			return true
		}
		if len(records) == 200 {
			more = true
			return false
		}
		records = append(records, chatRecord(record))
		cursor = record.Sequence
		return true
	})
	if err != nil {
		fail(w, 400, err)
		return
	}
	sendJSON(w, 200, struct {
		Records []Record `json:"records"`
		Cursor  uint64   `json:"cursor"`
		More    bool     `json:"more"`
	}{records, cursor, more})
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
	flusher.Flush()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	reader := bufio.NewReader(file)
	var offset int64
	lastState := ""
	heartbeat := time.Now()
	for {
		count := 0
		for count < 128 {
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
			var record Record
			if json.Unmarshal(line, &record) != nil {
				return
			}
			if record.Sequence <= after {
				continue
			}
			data,_:=json.Marshal(chatRecord(record))
			if _, err := fmt.Fprintf(w, "id: %d\nevent: record\ndata: %s\n\n", record.Sequence, data); err != nil {
				return
			}
			after = record.Sequence
			count++
		}
		state, _ := json.Marshal(s.snapshot(r.PathValue("name"), path))
		if string(state) != lastState {
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", state); err != nil {
				return
			}
			lastState = string(state)
		}
		if time.Since(heartbeat) > 15*time.Second {
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			heartbeat = time.Now()
		}
		flusher.Flush()
		if count == 128 {
			select {
			case <-r.Context().Done():
				return
			case <-s.ctx.Done():
				return
			default:
				continue
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
