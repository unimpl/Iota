package tools

import (
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

func TestBashSuccessFailureTimeoutAndTruncation(t *testing.T) {
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
}

func TestBashCancellation(t *testing.T) {
	bash := NewBash(t.TempDir(), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := bash.Execute(ctx, json.RawMessage(`{"command":"sleep 2"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
