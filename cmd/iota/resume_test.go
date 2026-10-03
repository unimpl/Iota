package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseOptionalResume(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		want      string
		requested bool
		wantError bool
	}{
		{name: "default"},
		{name: "bare", args: []string{"--resume"}, requested: true},
		{name: "before flag", args: []string{"--resume", "--model", "test"}, requested: true},
		{name: "empty equals", args: []string{"--resume="}, requested: true},
		{name: "empty argument", args: []string{"--resume", ""}, requested: true},
		{name: "uuid", args: []string{"--resume", "uuid"}, want: "uuid", requested: true},
		{name: "path", args: []string{"--resume=/tmp/session.jsonl"}, want: "/tmp/session.jsonl", requested: true},
		{name: "prompt value", args: []string{"-p", "--resume"}},
		{name: "prompt equals", args: []string{"-p=--resume"}},
		{name: "repeated", args: []string{"--resume", "uuid", "--resume"}, requested: true},
		{name: "disabled", args: []string{"--no-session", "--resume"}, wantError: true},
		{name: "terminator", args: []string{"--", "--resume"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			opts, err := parseOptionsWithConfig(test.args, &stderr, fileConfig{}, func(string) (string, bool) { return "", false })
			if (err != nil) != test.wantError {
				t.Fatalf("options=%+v err=%v", opts, err)
			}
			if err == nil && (opts.resume != test.want || opts.resumeRequested != test.requested) {
				t.Fatalf("options=%+v", opts)
			}
		})
	}
	// 显式不带值的命令行参数覆盖配置及环境变量中的会话选择。
	var stderr bytes.Buffer
	opts, err := parseOptionsWithConfig([]string{"--resume"}, &stderr, fileConfig{Resume: "configured"}, func(name string) (string, bool) {
		return "environment", name == "IOTA_RESUME"
	})
	if err != nil || !opts.resumeRequested || opts.resume != "" {
		t.Fatalf("options=%+v err=%v", opts, err)
	}
}

func TestResolveResumePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".iota", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	uuid := "4c37bf7a-80b9-4ca3-9d4a-60777e3e0ec7"
	// 文件名日期与修改时间故意相反，验证按最后修改时间选择。
	for index, name := range []string{"2026-10-03-" + uuid + ".jsonl", "2026-10-01-" + uuid + ".jsonl", "2026-10-02-other.jsonl", "ignored.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		modified := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ value, want string }{
		{uuid, filepath.Join(dir, "2026-10-01-"+uuid+".jsonl")},
		{"", filepath.Join(dir, "2026-10-02-other.jsonl")},
		{filepath.Join(home, "external.jsonl"), filepath.Join(home, "external.jsonl")},
	} {
		got, err := resolveResumePath(test.value)
		if err != nil || got != test.want {
			t.Fatalf("resolve(%q)=%q err=%v, want %q", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"session.jsonl", "./sessions/file"} {
		want, err := filepath.Abs(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolveResumePath(value)
		if err != nil || got != want {
			t.Fatalf("resolve(%q)=%q err=%v, want %q", value, got, err, want)
		}
	}
	if _, err := resolveResumePath("missing-uuid"); err == nil || !strings.Contains(err.Error(), "no saved session matching") {
		t.Fatalf("missing UUID error=%v", err)
	}
}

func TestFindResumeSessionEmpty(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if _, err := findResumeSession(dir, ""); err == nil || !strings.Contains(err.Error(), "no saved sessions found") {
			t.Fatalf("empty directory error=%v", err)
		}
	}
}
