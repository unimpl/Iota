package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fullOutputPath 从工具结果中提取完整输出文件路径，确保截断提示可供调用方使用。
func fullOutputPath(t *testing.T, result string) string {
	t.Helper()
	_, remainder, found := strings.Cut(result, "[Full output: ")
	if !found {
		t.Fatalf("missing full output path in %q", result)
	}
	path, _, found := strings.Cut(remainder, "]")
	if !found || path == "" {
		t.Fatalf("invalid full output path in %q", result)
	}
	return path
}

// TestWriteReadAndEdit 验证三个文件工具能连续操作同一路径，且按行读取会提示剩余内容。
func TestWriteReadAndEdit(t *testing.T) {
	cwd := t.TempDir()
	write := NewWrite(cwd)
	result, err := write.Execute(context.Background(), json.RawMessage(`{"path":"sub/file.txt","content":"one\ntwo\nthree\n"}`))
	if err != nil || !strings.Contains(result, "wrote") {
		t.Fatalf("write result=%q err=%v", result, err)
	}

	read := NewRead(cwd)
	result, err = read.Execute(context.Background(), json.RawMessage(`{"path":"sub/file.txt","offset":2,"limit":1}`))
	if err != nil || result != "2\ttwo\n\n[output truncated at 2000 lines or 51200 bytes]" {
		t.Fatalf("read result=%q err=%v", result, err)
	}

	edit := NewEdit(cwd)
	result, err = edit.Execute(context.Background(), json.RawMessage(`{"path":"sub/file.txt","old_text":"two","new_text":"second"}`))
	if err != nil || result != "updated sub/file.txt" {
		t.Fatalf("edit result=%q err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "sub/file.txt"))
	if err != nil || string(data) != "one\nsecond\nthree\n" {
		t.Fatalf("file=%q err=%v", data, err)
	}
}

// TestEditRejectsAmbiguousMatch 验证重复文本不会被任意选中并替换。
func TestEditRejectsAmbiguousMatch(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "file"), []byte("same same"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := NewEdit(cwd)
	_, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"file","old_text":"same","new_text":"new"}`))
	if err == nil || !strings.Contains(err.Error(), "2 locations") {
		t.Fatalf("got %v", err)
	}
}

// TestReadRejectsInvalidUTF8AndTruncates 验证无效编码会被拒绝，长文本截断后仍是有效 UTF-8。
func TestReadRejectsInvalidUTF8AndTruncates(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "bad"), []byte{0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	read := NewRead(cwd)
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":"bad"}`)); err == nil {
		t.Fatal("expected invalid UTF-8 error")
	}
	long := strings.Repeat("界", DefaultMaxBytes)
	if err := os.WriteFile(filepath.Join(cwd, "long"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := read.Execute(context.Background(), json.RawMessage(`{"path":"long"}`))
	if err != nil || !strings.Contains(result, "output truncated") || !utf8.ValidString(result) {
		t.Fatalf("result valid/truncated=%t/%t err=%v", utf8.ValidString(result), strings.Contains(result, "output truncated"), err)
	}
}

// TestBashSuccessFailureTimeoutAndTruncation 验证失败和超时可辨识，长输出仍可从临时文件恢复。
func TestBashSuccessFailureTimeoutAndTruncation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cwd := t.TempDir()
	bash := NewBash(cwd, time.Second)
	result, err := bash.Execute(context.Background(), json.RawMessage(`{"command":"printf hello"}`))
	if err != nil || result != "hello" {
		t.Fatalf("result=%q err=%v", result, err)
	}
	_, err = bash.Execute(context.Background(), json.RawMessage(`{"command":"printf failure; exit 7"}`))
	if err == nil || !strings.Contains(err.Error(), "failure") || !strings.Contains(err.Error(), "code 7") {
		t.Fatalf("got %v", err)
	}
	timed := NewBash(cwd, 20*time.Millisecond)
	_, err = timed.Execute(context.Background(), json.RawMessage(`{"command":"sleep 2"}`))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	result, err = bash.Execute(context.Background(), json.RawMessage(`{"command":"yes 界 | head -c 70000"}`))
	if err != nil || !strings.Contains(result, "earlier output truncated") || !utf8.ValidString(result) {
		t.Fatalf("truncation result length=%d err=%v", len(result), err)
	}
	fullOutput, err := os.ReadFile(fullOutputPath(t, result))
	if err != nil || !bytes.Equal(fullOutput, bytes.Repeat([]byte("界\n"), 17500)) {
		t.Fatalf("full output length=%d err=%v", len(fullOutput), err)
	}
}

// TestBashFailurePreservesLineTruncatedOutput 验证失败命令超过行数上限时仍保存完整输出。
func TestBashFailurePreservesLineTruncatedOutput(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	bash := NewBash(t.TempDir(), time.Second)
	_, err := bash.Execute(context.Background(), json.RawMessage(`{"command":"yes x | head -n 2100; exit 7"}`))
	if err == nil || !strings.Contains(err.Error(), "code 7") || !strings.Contains(err.Error(), "earlier output truncated") {
		t.Fatalf("got %v", err)
	}
	fullOutput, readErr := os.ReadFile(fullOutputPath(t, err.Error()))
	if readErr != nil || !bytes.Equal(fullOutput, bytes.Repeat([]byte("x\n"), 2100)) {
		t.Fatalf("full output length=%d err=%v", len(fullOutput), readErr)
	}
}

// TestBashCancellation 验证已取消的调用不会启动命令，并返回可识别的取消错误。
func TestBashCancellation(t *testing.T) {
	bash := NewBash(t.TempDir(), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := bash.Execute(ctx, json.RawMessage(`{"command":"sleep 2"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
