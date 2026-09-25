package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var webFiles embed.FS

var sessionFilename = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jsonl$`)

const defaultPort = 9280

type sessionInfo struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Model     string `json:"model,omitempty"`
	Title     string `json:"title,omitempty"`
}

func sessionTitle(content string) string {
	text := strings.Join(strings.Fields(content), " ")
	characters := []rune(text)
	if len(characters) > 20 {
		return string(characters[:20]) + "…"
	}
	return text
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	folder := flag.String("folder", filepath.Join(home, ".iota", "sessions"), "session JSONL directory")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected arguments")
	}
	root, err := filepath.Abs(*folder)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := listenAvailablePort(defaultPort)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	fmt.Printf("Iota session viewer: http://%s/\n", listener.Addr())
	log.Fatal(http.Serve(listener, newHandler(root)))
}

func listenAvailablePort(start int) (net.Listener, error) {
	for port := start; port <= 65535; port++ {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return listener, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no available port from %d to 65535", start)
}

func newHandler(folder string) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		entries, err := os.ReadDir(folder)
		if errors.Is(err, os.ErrNotExist) {
			entries = nil
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sessions := make([]sessionInfo, 0)
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !sessionFilename.MatchString(entry.Name()) {
				continue
			}
			file, err := os.Open(filepath.Join(folder, entry.Name()))
			if err != nil {
				continue
			}
			var first struct {
				Timestamp string `json:"timestamp"`
				Payload   struct {
					Model string `json:"model"`
				} `json:"payload"`
			}
			decoder := json.NewDecoder(bufio.NewReader(file))
			err = decoder.Decode(&first)
			if err != nil {
				file.Close()
				continue
			}
			session := sessionInfo{Name: entry.Name(), CreatedAt: first.Timestamp, Model: first.Payload.Model}
			for {
				var record struct {
					Type    string `json:"type"`
					Payload struct {
						Message struct {
							Role    string `json:"role"`
							Content string `json:"content"`
						} `json:"message"`
					} `json:"payload"`
				}
				if decoder.Decode(&record) != nil {
					break
				}
				if record.Type == "message_added" && record.Payload.Message.Role == "user" {
					session.Title = sessionTitle(record.Payload.Message.Content)
					break
				}
			}
			file.Close()
			sessions = append(sessions, session)
		}
		sort.Slice(sessions, func(i, j int) bool {
			if sessions[i].CreatedAt == sessions[j].CreatedAt {
				return sessions[i].Name > sessions[j].Name
			}
			return sessions[i].CreatedAt > sessions[j].CreatedAt
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sessions)
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unavailable", http.StatusInternalServerError)
			return
		}
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		var offset int64
		for {
			stat, err := file.Stat()
			if err != nil {
				return
			}
			if stat.Size() < offset {
				offset = 0
				fmt.Fprint(w, "event: reset\ndata: {}\n\n")
				flusher.Flush()
			}
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				return
			}
			reader := bufio.NewReader(file)
			for {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					break // Leave an unfinished line for the next read.
				}
				offset += int64(len(line))
				if json.Valid(line) {
					fmt.Fprintf(w, "data: %s\n\n", line[:len(line)-1])
					flusher.Flush()
				}
			}
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	})
	return mux
}
