package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	iota "github.com/unimpl/Iota"
)

var writeSchema = json.RawMessage(`{
  "type":"object",
  "properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"}},
  "required":["path","content"],
  "additionalProperties":false
}`)

func NewWrite(cwd string) iota.Tool {
	return iota.Tool{
		Name:        "write",
		Description: "Create or overwrite a UTF-8 text file, creating parent directories when needed.",
		Schema:      writeSchema,
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			path := resolvePath(cwd, args.Path)
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return "", err
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := os.WriteFile(path, []byte(args.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), nil
		},
	}
}
