package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	iota "github.com/unimpl/Iota"
)

// writeSchema 要求明确提供路径和内容；content 允许为空，以便创建空文件。
var writeSchema = json.RawMessage(`{
  "type":"object",
  "properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"}},
  "required":["path","content"],
  "additionalProperties":false
}`)

// NewWrite 创建文件写入工具；自动创建父目录以支持新路径。
// 已存在的文件会被覆盖，绝对路径不受 cwd 限制。
func NewWrite(cwd string) iota.Tool {
	return iota.Tool{
		Name:        "write",
		Description: "Create or overwrite a UTF-8 text file, creating parent directories when needed.",
		Schema:      writeSchema,
		// 写入前再次检查取消状态，避免在已取消的调用中覆盖文件。
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			// args 仅保存本次调用的目标路径和内容。
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
