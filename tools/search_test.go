package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadSearchPagePaginationAndTextClipping(t *testing.T) {
	var input strings.Builder
	input.WriteString("{\"type\":\"begin\"}\n")
	for line := 1; line <= 4; line++ {
		text := "match\n"
		if line == 2 {
			text = strings.Repeat("界", 2000) + "\n"
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&input, "{\"type\":\"match\",\"data\":{\"path\":{\"text\":\"a.go\"},\"lines\":{\"text\":%s},\"line_number\":%d}}\n", encoded, line)
	}
	page, err := readSearchPage(strings.NewReader(input.String()), searchInput{Offset: 2, Limit: 2}, false)
	if err != nil || len(page.Matches) != 2 || page.Matches[0].Line != 2 || !page.Matches[0].TextTruncated || !page.Truncated || page.NextOffset != 4 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	encoded, err := json.Marshal(page)
	if err != nil || len(encoded) > DefaultMaxBytes {
		t.Fatalf("length=%d err=%v", len(encoded), err)
	}
	page, err = readSearchPage(strings.NewReader(input.String()), searchInput{Offset: 4, Limit: 2}, false)
	if err != nil || len(page.Matches) != 1 || page.Truncated {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestReadFilePagePreservesSpecialPaths(t *testing.T) {
	page, err := readSearchPage(strings.NewReader("line\nname.go\x00tab\tname.go\x00third.go\x00"), searchInput{Offset: 1, Limit: 2}, true)
	if err != nil || len(page.Files) != 2 || page.Files[0] != "line\nname.go" || page.Files[1] != "tab\tname.go" || !page.Truncated || page.NextOffset != 3 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err := readSearchPage(strings.NewReader("incomplete"), searchInput{Offset: 1, Limit: 2}, true); err == nil {
		t.Fatal("incomplete file record accepted")
	}
}

func TestReadSearchPageByteLimit(t *testing.T) {
	var input strings.Builder
	for range 100 {
		fmt.Fprintf(&input, "{\"type\":\"match\",\"data\":{\"path\":{\"text\":\"a\"},\"lines\":{\"text\":%q},\"line_number\":1}}\n", strings.Repeat("x", 4096))
	}
	page, err := readSearchPage(strings.NewReader(input.String()), searchInput{Offset: 1, Limit: 100}, false)
	if err != nil || !page.Truncated || page.NextOffset != len(page.Matches)+1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	data, err := json.Marshal(page)
	if err != nil || len(data) > DefaultMaxBytes {
		t.Fatalf("length=%d err=%v", len(data), err)
	}
}

func TestSearchValidationAndCancellation(t *testing.T) {
	tool := NewSearch(t.TempDir(), RipgrepConfig{Path: "/missing/rg"})
	for _, raw := range []string{`{"pattern":""}`, `{"pattern":"x","limit":-1}`, `{"pattern":"x","offset":-1}`, `{"pattern":"x","unknown":true}`} {
		if _, err := tool.Execute(t.Context(), json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := tool.Execute(ctx, json.RawMessage(`{"pattern":"x"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

// Fake executables keep error/exit handling covered even on machines without rg.
func TestSearchProcessErrorsAndNoMatches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX shell")
	}
	for _, fixture := range []struct{ body, want string }{
		{"exit 1\n", "No results"},
		{"printf 'bad regex' >&2; exit 2\n", "bad regex"},
		{"exit 7\n", "exit status 7"},
		{"printf '{broken json}\\n'\n", "read ripgrep results"},
	} {
		binary := filepath.Join(t.TempDir(), "rg")
		if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+fixture.body), 0o700); err != nil {
			t.Fatal(err)
		}
		tool := NewSearch(t.TempDir(), RipgrepConfig{Path: binary})
		result, err := tool.Execute(t.Context(), json.RawMessage(`{"pattern":"x"}`))
		if fixture.want == "No results" {
			if err != nil || !strings.Contains(result, fixture.want) {
				t.Fatalf("result=%q err=%v", result, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), fixture.want) {
			t.Fatalf("result=%q err=%v", result, err)
		}
	}
}

func TestSearchAndFindWithRipgrep(t *testing.T) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("optional integration test requires rg; dependency resolution is tested separately")
	}
	cwd := t.TempDir()
	for name, text := range map[string]string{
		"a.go": "literal.a\nLITERAL.A\nliteralXa\n", "b.go": "literal.a\n", "note.txt": "literal.a\n",
		".hidden.go": "literal.a\n", "ignored.go": "literal.a\n", ".gitignore": "ignored.go\n",
	} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	search := NewSearch(cwd, RipgrepConfig{Path: binary})
	find := NewFindFiles(cwd, RipgrepConfig{Path: binary})
	if !search.ReadOnly || !find.ReadOnly {
		t.Fatal("search tools must be read-only")
	}
	for _, test := range []struct {
		raw       string
		count     int
		files     bool
		truncated bool
	}{
		{raw: `{"pattern":"literal.a","glob":"*.go"}`, count: 2},
		{raw: `{"pattern":"literal.a","glob":"*.go","ignore_case":true}`, count: 3},
		{raw: `{"pattern":"literal.a","glob":"*.go","regex":true}`, count: 3},
		{raw: `{"pattern":"literal.a","glob":"*.go","hidden":true}`, count: 3},
		{raw: `{"pattern":"literal.a","glob":"*.go","limit":1}`, count: 1, truncated: true},
		{raw: `{"pattern":"literal.a","glob":"*.go","offset":2,"limit":1}`, count: 1},
		{raw: `{"pattern":"*.go"}`, files: true, count: 2},
		{raw: `{"pattern":"*.go","hidden":true}`, files: true, count: 3},
	} {
		tool := search
		if test.files {
			tool = find
		}
		result, err := tool.Execute(t.Context(), json.RawMessage(test.raw))
		if err != nil {
			t.Fatalf("%s: %v", test.raw, err)
		}
		var page searchPage
		if err := json.Unmarshal([]byte(result), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Matches)+len(page.Files) != test.count || page.Truncated != test.truncated {
			t.Fatalf("%s: page=%+v", test.raw, page)
		}
	}
	for _, raw := range []string{`{"pattern":"[","regex":true}`, `{"pattern":"x","path":"absent"}`} {
		if _, err := search.Execute(t.Context(), json.RawMessage(raw)); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
	// User rg config must not silently change literal matching or ignore behavior.
	configPath := filepath.Join(t.TempDir(), "rg-config")
	if err := os.WriteFile(configPath, []byte("--ignore-case\n--hidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPGREP_CONFIG_PATH", configPath)
	result, err := search.Execute(t.Context(), json.RawMessage(`{"pattern":"literal.a","glob":"*.go"}`))
	if err != nil || bytes.Count([]byte(result), []byte(`"line":`)) != 2 {
		t.Fatalf("result=%q err=%v", result, err)
	}
}
