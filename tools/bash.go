package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	iota "github.com/unimpl/Iota"
)

var bashSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "command":{"type":"string","minLength":1},
    "timeout_seconds":{"type":"number","exclusiveMinimum":0}
  },
  "required":["command"],
  "additionalProperties":false
}`)

func NewBash(cwd string, defaultTimeout time.Duration) iota.Tool {
	if defaultTimeout <= 0 {
		defaultTimeout = 120 * time.Second
	}
	return iota.Tool{
		Name:        "bash",
		Description: "Execute a bash command in the working directory. Returns combined stdout and stderr, retaining the end of long output.",
		Schema:      bashSchema,
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Command        string  `json:"command"`
				TimeoutSeconds float64 `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			timeout := defaultTimeout
			if args.TimeoutSeconds > 0 {
				timeout = time.Duration(args.TimeoutSeconds * float64(time.Second))
			}
			return executeBash(ctx, cwd, args.Command, timeout)
		},
	}
}

type tailBuffer struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(data)
	b.data = append(b.data, data...)
	if len(b.data) > DefaultMaxBytes {
		b.data = append([]byte(nil), b.data[len(b.data)-DefaultMaxBytes:]...)
		b.truncated = true
	}
	for bytes.Count(b.data, []byte{'\n'}) > DefaultMaxLines {
		index := bytes.IndexByte(b.data, '\n')
		b.data = append([]byte(nil), b.data[index+1:]...)
		b.truncated = true
	}
	for len(b.data) > 0 && !utf8.Valid(b.data) {
		b.data = b.data[1:]
		b.truncated = true
	}
	return written, nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := string(b.data)
	if b.truncated {
		return "[earlier output truncated]\n" + text
	}
	return text
}

func executeBash(ctx context.Context, cwd, command string, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &tailBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return "", err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var err error
	select {
	case err = <-wait:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-wait
		return "", fmt.Errorf("command canceled: %w", ctx.Err())
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-wait
		text := strings.TrimSuffix(output.String(), "\n")
		if text != "" {
			return "", fmt.Errorf("%s\n\ncommand timed out after %s", text, timeout)
		}
		return "", fmt.Errorf("command timed out after %s", timeout)
	}
	text := strings.TrimSuffix(output.String(), "\n")
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			if text == "" {
				text = "(no output)"
			}
			return "", fmt.Errorf("%s\n\ncommand exited with code %d", text, exitError.ExitCode())
		}
		return "", err
	}
	if text == "" {
		text = "(no output)"
	}
	return text, nil
}
