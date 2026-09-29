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

// webFiles 将静态页面嵌入可执行文件，查看器启动时不依赖外部资源目录。
//
//go:embed web/*
var webFiles embed.FS

// sessionFilename 只允许本程序生成的会话文件名，防止查询参数访问任意路径。
var sessionFilename = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jsonl$`)

// defaultPort 是查看器首选本地端口；被占用时会顺延查找。
const defaultPort = 9280

// sessionInfo 是会话列表返回给页面的摘要，不包含完整事件内容。
type sessionInfo struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Model     string `json:"model,omitempty"`
	Title     string `json:"title,omitempty"`
}

// sessionTitle 从首条用户消息提取标题，按字符而非字节截取中文。
func sessionTitle(content string) string {
	text := strings.Join(strings.Fields(content), " ")
	characters := []rune(text)
	if len(characters) > 20 {
		return string(characters[:20]) + "…"
	}
	return text
}

// main 只在本机监听查看器，默认读取用户目录下的会话日志。
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

// listenAvailablePort 只跳过已占用的端口；其他监听错误应立即暴露。
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

// newHandler 提供嵌入页面、会话列表和持续跟随的事件流。
// 事件接口只接受匹配文件名且确认为普通文件的日志。
func newHandler(folder string) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	// 列表只读取每份日志的开头和首条用户消息，避免一次加载全部事件。
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
			// 只解码首行需要的字段，避免读取无关的大型事件内容。
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
				// 后续记录只需事件类型和首条用户消息，其他字段跳过。
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
		// 时间相同时用文件名确定顺序，避免列表刷新时条目跳动。
		sort.Slice(sessions, func(i, j int) bool {
			if sessions[i].CreatedAt == sessions[j].CreatedAt {
				return sessions[i].Name > sessions[j].Name
			}
			return sessions[i].CreatedAt > sessions[j].CreatedAt
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sessions)
	})
	// 事件流只发送完整 JSONL 行；未写完的尾行留到下次轮询。
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
					break // 未写完的一行留到下次读取，避免页面收到不完整 JSON。
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
