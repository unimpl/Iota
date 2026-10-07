package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	iota "github.com/unimpl/Iota"
)

// NewList lets planning inspect directories without invoking a general shell.
func NewList(cwd string) iota.Tool {
	return iota.Tool{
		Name: "list", ReadOnly: true,
		Description: "List a directory's immediate children in name order. Directories end in /; symbolic links end in @. Defaults to the working directory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":2000}},"additionalProperties":false}`),
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var input struct {
				Path  string `json:"path"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return "", err
			}
			if input.Path == "" {
				input.Path = "."
			}
			if input.Limit == 0 {
				input.Limit = DefaultMaxLines
			}
			if input.Limit < 1 || input.Limit > DefaultMaxLines {
				return "", fmt.Errorf("limit must be between 1 and %d", DefaultMaxLines)
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			entries, err := os.ReadDir(resolvePath(cwd, input.Path))
			if err != nil {
				return "", err
			}
			if len(entries) == 0 {
				return "(empty directory)", nil
			}
			var output strings.Builder
			for _, entry := range entries[:min(len(entries), input.Limit)] {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				name := entry.Name()
				if entry.IsDir() {
					name += "/"
				} else if entry.Type()&os.ModeSymlink != 0 {
					name += "@"
				}
				fmt.Fprintln(&output, name)
			}
			text, truncated := truncateHead(strings.TrimSuffix(output.String(), "\n"), DefaultMaxLines, DefaultMaxBytes)
			if truncated || len(entries) > input.Limit {
				text = truncationNote(text)
			}
			return text, nil
		},
	}
}
