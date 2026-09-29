package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	iota "github.com/unimpl/Iota"
)

// editSchema 要求提供路径和替换文本，并拒绝未知字段；new_text 允许为空以支持删除。
var editSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1},
    "old_text":{"type":"string","minLength":1},
    "new_text":{"type":"string"}
  },
  "required":["path","old_text","new_text"],
  "additionalProperties":false
}`)

// NewEdit 创建精确文本替换工具；要求原文本只出现一次，避免误改其他位置。
// 目标路径可为绝对路径，写入会覆盖该文件的原有内容。
func NewEdit(cwd string) iota.Tool {
	return iota.Tool{
		Name:        "edit",
		Description: "Replace exactly one literal text occurrence in a UTF-8 file. Fails if the text is absent or appears more than once.",
		Schema:      editSchema,
		// 先确认唯一匹配，再在写入前检查取消状态，降低误改风险。
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			// args 只保存本次替换的路径及文本，不在调用之间共享。
			var args struct {
				Path    string `json:"path"`
				OldText string `json:"old_text"`
				NewText string `json:"new_text"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			path := resolvePath(cwd, args.Path)
			if err := ctx.Err(); err != nil {
				return "", err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			count := bytes.Count(data, []byte(args.OldText))
			if count == 0 {
				return "", errors.New("old_text was not found")
			}
			if count > 1 {
				return "", fmt.Errorf("old_text matched %d locations; expected exactly one", count)
			}
			updated := bytes.Replace(data, []byte(args.OldText), []byte(args.NewText), 1)
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := os.WriteFile(path, updated, 0o644); err != nil {
				return "", err
			}
			return "updated " + args.Path, nil
		},
	}
}
