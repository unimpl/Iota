package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Byte-offset event IDs resume a reconnect at the next complete JSONL record.
// Flush bounded batches instead of one HTTP chunk per token-sized event.
func eventsHandler(folder string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if !sessionFilename.MatchString(name) {
			http.Error(w, "invalid session name", http.StatusBadRequest)
			return
		}
		path := filepath.Join(folder, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		file, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()
		var offset int64
		reset := false
		if id := r.Header.Get("Last-Event-ID"); id != "" {
			offset, err = strconv.ParseInt(id, 10, 64)
			if err != nil || offset < 0 {
				http.Error(w, "invalid event cursor", http.StatusBadRequest)
				return
			}
			stat, err := file.Stat()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if offset > stat.Size() {
				offset, reset = 0, true
			} else if offset > 0 {
				var end [1]byte
				if _, err := file.ReadAt(end[:], offset-1); err != nil || end[0] != '\n' {
					http.Error(w, "event cursor must end at a complete record", http.StatusBadRequest)
					return
				}
			}
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if reset {
			fmt.Fprint(w, "id: 0\nevent: reset\ndata: {}\n\n")
		}
		// An empty or fully resumed file must still establish the SSE connection.
		flusher.Flush()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		reader := bufio.NewReaderSize(file, 64*1024)
		for {
			stat, err := file.Stat()
			if err != nil {
				return
			}
			if stat.Size() < offset {
				offset = 0
				if _, err := fmt.Fprint(w, "id: 0\nevent: reset\ndata: {}\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
			if stat.Size() == offset {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					continue
				}
			}
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				return
			}
			reader.Reset(file)
			pending, bytes := 0, 0
			for r.Context().Err() == nil {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					break // A partial tail stays at offset until its newline arrives.
				}
				offset += int64(len(line))
				if !json.Valid(line) {
					continue
				}
				if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", offset, line[:len(line)-1]); err != nil {
					return
				}
				pending++
				bytes += len(line)
				if pending >= 128 || bytes >= 64*1024 {
					flusher.Flush()
					pending, bytes = 0, 0
				}
			}
			if pending > 0 {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	}
}
