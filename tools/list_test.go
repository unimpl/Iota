package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListExploresDirectoriesWithoutFollowingLinks(t *testing.T) {
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "README.md"), []byte("docs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src", filepath.Join(cwd, "link")); err != nil {
		t.Fatal(err)
	}
	tool := NewList(cwd)
	if !tool.ReadOnly {
		t.Fatal("directory listing must be available during planning")
	}
	result, err := tool.Execute(t.Context(), json.RawMessage(`{}`))
	if err != nil || result != "README.md\nlink@\nsrc/" {
		t.Fatalf("result=%q err=%v", result, err)
	}
	result, err = tool.Execute(t.Context(), json.RawMessage(`{"limit":1}`))
	if err != nil || !strings.HasPrefix(result, "README.md\n") || !strings.Contains(result, "output truncated") {
		t.Fatalf("result=%q err=%v", result, err)
	}
	result, err = tool.Execute(t.Context(), json.RawMessage(`{"path":"src"}`))
	if err != nil || result != "(empty directory)" {
		t.Fatalf("result=%q err=%v", result, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := tool.Execute(ctx, json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
