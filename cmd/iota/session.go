package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	iota "github.com/unimpl/Iota"
)

const sessionFormatVersion = 1

type sessionLog struct {
	mu   sync.Mutex
	file *os.File
	id   string
	path string
	seq  uint64
	err  error
}

type sessionRecord struct {
	Version   int    `json:"version"`
	Sequence  uint64 `json:"seq"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	Type      string `json:"type"`
	Payload   any    `json:"payload,omitempty"`
}

func newSessionLog(model, cwd string) (*sessionLog, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	dir := filepath.Join(home, ".iota", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create sessions directory: %w", err)
	}
	id, err := newUUID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, time.Now().Format("2006-01-02")+"-"+id+".jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create session file: %w", err)
	}
	log := &sessionLog{file: file, id: id, path: path}
	if err := log.write("", "session_start", map[string]string{"model": model, "cwd": cwd}); err != nil {
		file.Close()
		return nil, err
	}
	return log, nil
}

func newUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("create UUID: %w", err)
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[:4], data[4:6], data[6:8], data[8:10], data[10:]), nil
}

func (l *sessionLog) write(runID, kind string, payload any) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	line, err := json.Marshal(sessionRecord{
		Version: sessionFormatVersion, Sequence: l.seq + 1,
		Timestamp: time.Now().Format(time.RFC3339Nano), SessionID: l.id,
		RunID: runID, Type: kind, Payload: payload,
	})
	if err == nil {
		line = append(line, '\n')
		var written int
		written, err = l.file.Write(line)
		if err == nil && written != len(line) {
			err = fmt.Errorf("short session write: %d of %d bytes", written, len(line))
		}
	}
	if err != nil {
		l.err = err
		return err
	}
	l.seq++
	return nil
}

func (l *sessionLog) event(runID string, event iota.Event) error {
	return l.write(runID, string(event.Type), event)
}

func (l *sessionLog) close() error {
	if l == nil {
		return nil
	}
	return l.file.Close()
}
