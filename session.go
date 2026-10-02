package iota

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

const sessionFormatVersion = 1

// SessionInfo 记录会话来源；恢复时 Provider、模型、系统提示和工具仍由调用方配置。
type SessionInfo struct {
	Model string `json:"model"`
	CWD   string `json:"cwd"`
}

// Session 保存执行事件并恢复对话。目录由调用方指定，同一文件只能有一个写入者。
// 使用 Agent.Run 只保留内存历史，使用 Session.Run 才会写入会话文件。
type Session struct {
	operation sync.Mutex
	mu        sync.Mutex
	file      *os.File
	id        string
	path      string
	seq       uint64
	err       error
	closed    bool
	messages  []Message
}

// sessionRecord 是单行 JSONL 的固定外壳；保留查看器使用的事件结构。
type sessionRecord struct {
	Version   int             `json:"version"`
	Sequence  uint64          `json:"seq"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// NewSession 在指定目录创建仅当前用户可读写的会话文件。
func NewSession(dir string, info SessionInfo) (*Session, error) {
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
	s := &Session{file: file, id: id, path: path}
	if err := s.write("", "session_start", info); err != nil {
		file.Close()
		return nil, err
	}
	return s, nil
}

// Path 返回会话文件路径，不替调用方选择用户目录。
func (s *Session) Path() string { return s.path }

// Run 运行 Agent 并保存所有事件；nil Session 表示关闭持久化。
// Agent 的历史必须与该会话一致；打开已有文件后先调用 Restore。
func (s *Session) Run(ctx context.Context, agent *Agent, prompt string, emit EmitFunc) (RunResult, error) {
	if s == nil {
		return agent.Run(ctx, prompt, emit)
	}
	if !s.operation.TryLock() {
		return RunResult{}, ErrBusy
	}
	defer s.operation.Unlock()
	s.mu.Lock()
	err := s.checkOpen()
	messages := cloneMessages(s.messages)
	s.mu.Unlock()
	if err != nil {
		return RunResult{}, err
	}
	if !reflect.DeepEqual(agent.Messages(), messages) {
		return RunResult{}, errors.New("agent history differs from session; call Session.Restore first")
	}
	runID, err := newUUID()
	if err != nil {
		return RunResult{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result, runErr := agent.Run(ctx, prompt, func(event Event) {
		if err := s.write(runID, string(event.Type), event); err != nil {
			cancel()
		}
		emitEvent(emit, event)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = s.file.Sync()
	}
	return result, errors.Join(runErr, s.err)
}

// Restore 用会话中的完整消息替换 Agent 历史；运行中拒绝恢复。
func (s *Session) Restore(agent *Agent) error {
	if !s.operation.TryLock() {
		return ErrBusy
	}
	defer s.operation.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.running {
		return ErrBusy
	}
	agent.messages = cloneMessages(s.messages)
	return nil
}

// Reset 清空 Agent 历史并记录重置，保证下次恢复不会带回旧消息。
func (s *Session) Reset(agent *Agent) error {
	if s == nil {
		return agent.Reset()
	}
	if !s.operation.TryLock() {
		return ErrBusy
	}
	defer s.operation.Unlock()
	s.mu.Lock()
	err := s.checkOpen()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.running {
		return ErrBusy
	}
	if err := s.write("", "session_reset", nil); err != nil {
		return err
	}
	agent.messages = nil
	return nil
}

// Close 记录结束并关闭文件；重复关闭安全，运行中返回 ErrBusy。
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	if !s.operation.TryLock() {
		return ErrBusy
	}
	defer s.operation.Unlock()
	s.mu.Lock()
	alreadyClosed := s.closed
	s.mu.Unlock()
	if alreadyClosed {
		return nil
	}
	writeErr := s.write("", "session_end", nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return errors.Join(writeErr, s.file.Sync(), s.file.Close())
}

// checkOpen 由持锁的调用方检查文件状态，保存失败后禁止继续追加。
func (s *Session) checkOpen() error {
	if s.closed {
		return errors.New("session is closed")
	}
	return s.err
}

// write 顺序保存事件，同时维护可恢复的消息；首个写入错误会被保留。
func (s *Session) write(runID, kind string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	var raw json.RawMessage
	var err error
	if payload != nil {
		raw, err = json.Marshal(payload)
	}
	var line []byte
	if err == nil {
		line, err = json.Marshal(sessionRecord{
			Version: sessionFormatVersion, Sequence: s.seq + 1,
			Timestamp: time.Now().Format(time.RFC3339Nano), SessionID: s.id,
			RunID: runID, Type: kind, Payload: raw,
		})
	}
	if err == nil {
		line = append(line, '\n')
		var written int
		written, err = s.file.Write(line)
		if err == nil && written != len(line) {
			err = fmt.Errorf("short session write: %d of %d bytes", written, len(line))
		}
	}
	if err != nil {
		s.err = err
		return err
	}
	s.seq++
	if kind == "session_reset" {
		s.messages = nil
	} else if event, ok := payload.(Event); ok && event.Type == EventMessageAdded && event.Message != nil {
		s.messages = append(s.messages, *cloneMessagePointer(*event.Message))
	}
	return nil
}

// newUUID 为会话与每次运行生成随机 v4 UUID。
func newUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("create UUID: %w", err)
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[:4], data[4:6], data[6:8], data[8:10], data[10:]), nil
}
