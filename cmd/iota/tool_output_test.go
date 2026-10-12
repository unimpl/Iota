package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/rivo/uniseg"
	iota "github.com/unimpl/Iota"
)

func TestTerminalToolCallFitsWidthAndKeepsArguments(t *testing.T) {
	for _, colored := range []bool{false, true} {
		for _, call := range []iota.ToolCall{
			{ID: "shell", Name: "bash", Arguments: json.RawMessage(`{"command":"printf '甲乙，ábc'\necho done\tmore"}`)},
			{ID: "write", Name: "write", Arguments: json.RawMessage("{\n  \"path\": \"file.go\",\n  \"content\": \"" + strings.Repeat("long content", 50) + "\"\n}")},
		} {
			var output bytes.Buffer
			columns := 30
			printer := eventOutput{stderr: &output, stderrStyle: terminalStyle{terminal: true, colored: colored}, terminalWidth: func() int { return columns }}
			original := call
			original.Arguments = append(json.RawMessage(nil), call.Arguments...)
			printer.emit(iota.Event{Type: iota.EventToolStart, ToolCall: &call})
			plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(output.String(), "")
			line := strings.TrimSuffix(plain, "\n")
			if strings.Count(plain, "\n") != 1 || strings.ContainsAny(line, "\r\t") || uniseg.StringWidth(line) > columns-1 || !strings.HasSuffix(line, "…") {
				t.Fatalf("call=%s columns=%d output=%q", call.Name, columns, plain)
			}
			if !reflect.DeepEqual(call, original) || colored != strings.Contains(output.String(), colorTool) {
				t.Fatal("display formatting changed tool arguments or color handling")
			}
		}
	}
}

func TestToolCallReadsCurrentWidthAndEscapesMultilineCommand(t *testing.T) {
	var output bytes.Buffer
	columns := 80
	printer := eventOutput{stderr: &output, stderrStyle: terminalStyle{terminal: true}, terminalWidth: func() int { return columns }}
	call := iota.ToolCall{ID: "shell", Name: "bash", Arguments: json.RawMessage(`{"command":"echo one\necho two\tend"}`)}
	printer.emit(iota.Event{Type: iota.EventToolStart, ToolCall: &call})
	if output.String() != "[bash] echo one\\necho two\\tend\n" {
		t.Fatalf("multiline command=%q", output.String())
	}
	columns = 12
	output.Reset()
	printer.emit(iota.Event{Type: iota.EventToolStart, ToolCall: &call})
	if output.String() != "[bash] ech…\n" {
		t.Fatalf("resized command=%q", output.String())
	}
	columns = 1
	output.Reset()
	printer.emit(iota.Event{Type: iota.EventToolStart, ToolCall: &call})
	if output.String() != "…\n" {
		t.Fatalf("narrow terminal=%q", output.String())
	}
}

func TestTerminalToolCallDoesNotSplitCombiningCharacters(t *testing.T) {
	text := "甲a\u0301乙丙"
	if line := terminalToolCallLine(text, 4); line != "甲a\u0301…" || uniseg.StringWidth(line) != 4 {
		t.Fatalf("Unicode truncation=%q", line)
	}
	if line := terminalToolCallLine("short", 5); line != "short" {
		t.Fatal("fitting text was unnecessarily truncated")
	}
	if line := terminalToolCallLine("\x1b[2J\bunsafe", 30); strings.ContainsAny(line, "\x1b\b") {
		t.Fatal("parameter text could control the terminal")
	}
}

func TestRedirectedToolCallPreservesCompleteOutput(t *testing.T) {
	var output bytes.Buffer
	printer := eventOutput{stderr: &output, terminalWidth: func() int { t.Fatal("redirected output queried terminal width"); return 1 }}
	command := strings.Repeat("long command ", 100) + "\necho next"
	arguments, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		t.Fatal(err)
	}
	printer.emit(iota.Event{Type: iota.EventToolStart, ToolCall: &iota.ToolCall{ID: "shell", Name: "bash", Arguments: arguments}})
	if output.String() != "[bash] "+command+"\n" {
		t.Fatal("redirection lost complete tool parameters")
	}
}
