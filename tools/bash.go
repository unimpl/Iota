package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	iota "github.com/unimpl/Iota"
)

// bashSchema 约束命令参数，避免调用方传入空命令或未知字段；超时为可选的正数秒。
var bashSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "command":{"type":"string","minLength":1},
    "timeout_seconds":{"type":"number","exclusiveMinimum":0}
  },
  "required":["command"],
  "additionalProperties":false
}`)

// NewBash 创建命令工具；未提供正的默认超时时间时使用 120 秒，避免命令无限运行。
// cwd 只指定执行目录，不限制命令可访问的路径。
func NewBash(cwd string, defaultTimeout time.Duration) iota.Tool {
	if defaultTimeout <= 0 {
		defaultTimeout = 120 * time.Second
	}
	return iota.Tool{
		Name:        "bash",
		Description: "Execute a bash command in the working directory. Returns combined stdout and stderr, retaining the end of long output. Truncated output is saved to a temporary file whose path is returned.",
		Schema:      bashSchema,
		// 每次调用单独解析参数；显式传入的超时时间覆盖工具的默认值。
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			// args 只保存本次调用的参数，避免并发调用之间共享状态。
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

// tailBuffer 在内存中只保留输出末尾，超限后将完整输出写入临时文件。
// stdout 和 stderr 共用它，因此写入及文件创建必须加锁以维持相同的输出顺序。
type tailBuffer struct {
	mu         sync.Mutex // 保护缓冲区和临时文件，防止两个输出流并发写入。
	data       []byte     // 仅保存尚未被截断的输出末尾。
	truncated  bool       // 记录是否丢弃过内容，供工具结果添加提示。
	fullOutput *os.File   // 首次截断时创建，保存未经截断的原始输出。
	writeErr   error      // 保存临时文件写入失败的原因，避免返回不完整文件路径。
}

// Write 同时写入完整输出文件和有界的内存缓冲区；首次截断时先将已有内容写入文件。
// 截断后跳过不完整的 UTF-8 字符；文件写入失败时返回错误，不能承诺保留完整输出。
func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(data)
	if b.writeErr != nil {
		return 0, b.writeErr
	}
	if b.fullOutput != nil {
		if _, err := b.fullOutput.Write(data); err != nil {
			b.writeErr = err
			return 0, err
		}
	}
	b.data = append(b.data, data...)
	if b.fullOutput == nil && (len(b.data) > DefaultMaxBytes || bytes.Count(b.data, []byte{'\n'}) > DefaultMaxLines || !utf8.Valid(b.data)) {
		file, err := os.CreateTemp("", "iota-bash-*.log")
		if err != nil {
			b.writeErr = err
			return 0, err
		}
		if _, err := file.Write(b.data); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			b.writeErr = err
			return 0, err
		}
		b.fullOutput = file
	}
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

// finish 关闭完整输出文件并返回输出末尾及文件路径。
// 只有命令已结束、不会再调用 Write 时才能调用；关闭失败时删除不可靠的文件。
func (b *tailBuffer) finish() (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	path := ""
	if b.fullOutput != nil {
		path = b.fullOutput.Name()
		if err := b.fullOutput.Close(); err != nil && b.writeErr == nil {
			b.writeErr = err
		}
	}
	if b.writeErr != nil {
		if path != "" {
			_ = os.Remove(path)
		}
		return "", "", fmt.Errorf("save full command output: %w", b.writeErr)
	}
	text := string(b.data)
	if b.truncated {
		text = "[earlier output truncated]\n" + text
	}
	return text, path, nil
}

// executeBash 在工作目录运行命令，同时收集 stdout 和 stderr 的末尾。
// 取消或超时时杀死整个进程组，避免 shell 启动的子进程继续运行。
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
	// 单独等待命令结束，主协程才能同时监听取消和超时信号。
	go func() { wait <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var err error
	var canceled, timedOut bool
	select {
	case err = <-wait:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-wait
		canceled = true
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-wait
		timedOut = true
	}
	text, fullOutputPath, outputErr := output.finish()
	if outputErr != nil {
		return "", outputErr
	}
	if canceled {
		if fullOutputPath != "" {
			_ = os.Remove(fullOutputPath)
		}
		return "", fmt.Errorf("command canceled: %w", ctx.Err())
	}
	text = strings.TrimSuffix(text, "\n")
	if fullOutputPath != "" {
		text += "\n\n[Full output: " + fullOutputPath + "]"
	}
	if timedOut {
		if text != "" {
			return "", fmt.Errorf("%s\n\ncommand timed out after %s", text, timeout)
		}
		return "", fmt.Errorf("command timed out after %s", timeout)
	}
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
