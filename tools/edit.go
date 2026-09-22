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

func NewEdit(cwd string) iota.Tool {
	return iota.Tool{
		Name:        "edit",
		Description: "Replace exactly one literal text occurrence in a UTF-8 file. Fails if the text is absent or appears more than once.",
		Schema:      editSchema,
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
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
