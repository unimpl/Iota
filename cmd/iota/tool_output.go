package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/chzyer/readline"
	"github.com/rivo/uniseg"
	iota "github.com/unimpl/Iota"
)

func (o *eventOutput) toolCallLine(call iota.ToolCall) string {
	arguments := string(call.Arguments)
	if o.stderrStyle.terminal {
		var compact bytes.Buffer
		if err := json.Compact(&compact, call.Arguments); err == nil {
			arguments = compact.String()
		}
	}
	if call.Name == "bash" {
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err == nil && args.Command != "" {
			arguments = args.Command
		}
	}
	line := fmt.Sprintf("[%s] %s", call.Name, arguments)
	if !o.stderrStyle.terminal {
		return line
	}
	width := 80
	if o.terminalWidth != nil {
		width = o.terminalWidth()
	} else if file, ok := o.stderr.(*os.File); ok {
		if columns, _, err := readline.GetSize(int(file.Fd())); err == nil && columns > 0 {
			width = columns
		}
	}
	// Leave the rightmost cell unused to prevent terminal automatic wrapping.
	return terminalToolCallLine(line, max(1, width-1))
}

func terminalToolCallLine(text string, columns int) string {
	text = strings.NewReplacer("\r", `\r`, "\n", `\n`, "\t", `\t`).Replace(text)
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, text)
	if uniseg.StringWidth(text) <= columns {
		return text
	}
	var prefix strings.Builder
	width := 0
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		if width+graphemes.Width() > columns-1 {
			break
		}
		prefix.WriteString(graphemes.Str())
		width += graphemes.Width()
	}
	return prefix.String() + "…"
}
