package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	iota "github.com/unimpl/Iota"
)

var readSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","minLength":1},
    "offset":{"type":"integer","minimum":1},
    "limit":{"type":"integer","minimum":1}
  },
  "required":["path"],
  "additionalProperties":false
}`)

func NewRead(cwd string) iota.Tool {
	return iota.Tool{
		Name:        "read",
		Description: "Read a UTF-8 text file. offset is a 1-based line number and limit is a maximum number of lines.",
		Schema:      readSchema,
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if args.Offset == 0 {
				args.Offset = 1
			}
			if args.Limit == 0 || args.Limit > DefaultMaxLines {
				args.Limit = DefaultMaxLines
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			data, err := os.ReadFile(resolvePath(cwd, args.Path))
			if err != nil {
				return "", err
			}
			if !utf8.Valid(data) {
				return "", fmt.Errorf("file is not valid UTF-8: %s", args.Path)
			}
			lines := strings.Split(string(data), "\n")
			if len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			start := args.Offset - 1
			if start >= len(lines) {
				return fmt.Sprintf("[offset %d is beyond end of file; file has %d lines]", args.Offset, len(lines)), nil
			}
			end := min(start+args.Limit, len(lines))
			var builder strings.Builder
			for index := start; index < end; index++ {
				fmt.Fprintf(&builder, "%d\t%s", index+1, lines[index])
				if index+1 < end {
					builder.WriteByte('\n')
				}
			}
			output, byteTruncated := truncateHead(builder.String(), DefaultMaxLines, DefaultMaxBytes)
			if byteTruncated || end < len(lines) {
				output = truncationNote(output)
			}
			return output, nil
		},
	}
}
