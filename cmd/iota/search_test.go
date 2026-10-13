package main

import (
	"strings"
	"testing"
	"time"
)

func TestSearchToolsAreDefaultReadOnlyAndLazy(t *testing.T) {
	tools, err := createTools(t.TempDir(), defaultTools, time.Second, func(message string) {
		t.Errorf("tool registration downloaded a dependency: %s", message)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"search", "find_files"} {
		found := false
		for _, tool := range tools {
			if tool.Name == name {
				found = true
				if !tool.ReadOnly {
					t.Fatalf("%s must be read-only", name)
				}
			}
		}
		if !found {
			t.Fatalf("missing default tool %s", name)
		}
	}
	selected, err := createTools(t.TempDir(), "search,find_files,search", time.Second)
	if err != nil || len(selected) != 2 || selected[0].Name != "search" || selected[1].Name != "find_files" {
		t.Fatalf("tools=%v err=%v", selected, err)
	}
	if !strings.Contains(defaultTools, "search,find_files") {
		t.Fatal("search tools missing from default configuration")
	}
}
