package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummaryPreviewValidatesNamesAndServesPlainMarkdown(t *testing.T) {
	const name = "2026-10-08-00000000-0000-4000-8000-000000000001.jsonl"
	for _, test := range []struct {
		name, session, event, kind string
		status                     int
	}{
		{"regular", name, "42", "file", 200},
		{"missing", name, "42", "missing", 404},
		{"symlink", name, "42", "symlink", 400},
		{"directory", name, "42", "directory", 400},
		{"oversized", name, "42", "oversized", 413},
		{"path traversal", "../" + name, "42", "file", 400},
		{"bad sequence", name, "../42", "file", 400},
		{"zero", name, "0", "file", 400},
		{"unknown session", "2026-10-08-00000000-0000-4000-8000-000000000002.jsonl", "42", "file", 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, strings.TrimSuffix(name, ".jsonl")+".summary.42.md")
			const body = "# Summary\n\n<script>alert('text only')</script>\n"
			switch test.kind {
			case "file":
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := filepath.Join(t.TempDir(), "private.md")
				if err := os.WriteFile(outside, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, []byte(strings.Repeat("a", (4<<20)+1)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequest("GET", "/api/summary?name="+url.QueryEscape(test.session)+"&event="+url.QueryEscape(test.event), nil)
			response := httptest.NewRecorder()
			newHandler(dir).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.status == 200 {
				if response.Body.String() != body || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Fatalf("response=%v %q", response.Header(), response.Body.String())
				}
			}
		})
	}
}

func TestSummaryForExternalSessionCanBeReadFromHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	name := "2026-10-08-00000000-0000-4000-8000-000000000001.jsonl"
	if err := os.WriteFile(filepath.Join(root, name), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".iota", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	filename := strings.TrimSuffix(name, ".jsonl") + ".summary.7.md"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("# Home summary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", fmt.Sprintf("/api/summary?name=%s&event=7", name), nil)
	response := httptest.NewRecorder()
	newHandler(root).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "# Home summary\n" {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}
