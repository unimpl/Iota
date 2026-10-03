package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
)

// TestEventOutputTransitions 验证连续片段只产生一个标题，切换到思考或工具前正确换行。
func TestEventOutputTransitions(t *testing.T) {
	for _, colored := range []bool{false, true} {
		name := "plain terminal"
		if colored {
			name = "colored terminal"
		}
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			style := terminalStyle{terminal: true, colored: colored}
			printer := eventOutput{stdout: &output, stderr: &output, stdoutStyle: style, stderrStyle: style}
			call := &iota.ToolCall{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}
			for _, event := range []iota.Event{
				{Type: iota.EventReasoningDelta, Reasoning: "先思考"},
				{Type: iota.EventTextDelta, Text: "检查"},
				{Type: iota.EventTextDelta, Text: "目录。"},
				{Type: iota.EventToolCallDelta},
				{Type: iota.EventModelResponse},
				{Type: iota.EventToolStart, ToolCall: call},
				{Type: iota.EventToolEnd, ToolCall: call, ToolResult: "failed", IsError: true},
				{Type: iota.EventReasoningDelta, Reasoning: "继续"},
				{Type: iota.EventTextDelta, Text: "完成\n"},
				{Type: iota.EventModelResponse},
				{Type: iota.EventRunEnd},
			} {
				printer.emit(event)
			}
			text := output.String()
			plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(text, "")
			want := "[thinking]\n先思考\n[/thinking]\n[assistant]\n检查目录。\n[bash] pwd\n[bash error] failed\n[thinking]\n继续\n[/thinking]\n[assistant]\n完成\n"
			if plain != want {
				t.Fatalf("output=%q, want %q", plain, want)
			}
			if colored {
				for _, fragment := range []string{
					"\x1b[90m先思考\x1b[0m",
					"\x1b[1;36m[assistant]\x1b[0m",
					"\x1b[36m检查\x1b[0m",
					"\x1b[33m[bash] pwd\x1b[0m",
					"\x1b[1;31m[bash error] failed\x1b[0m",
				} {
					if !strings.Contains(text, fragment) {
						t.Fatalf("output=%q, missing %q", text, fragment)
					}
				}
			} else if strings.Contains(text, "\x1b") {
				t.Fatalf("uncolored terminal output contains ANSI: %q", text)
			}
		})
	}
}

// TestEventOutputMixedStreams 确保重定向一条流不会影响另一条终端流的样式。
func TestEventOutputMixedStreams(t *testing.T) {
	for _, stdoutTerminal := range []bool{false, true} {
		name := "redirected stdout"
		if stdoutTerminal {
			name = "redirected stderr"
		}
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			printer := eventOutput{
				stdout: &stdout, stderr: &stderr,
				stdoutStyle: terminalStyle{terminal: stdoutTerminal, colored: stdoutTerminal},
				stderrStyle: terminalStyle{terminal: !stdoutTerminal, colored: !stdoutTerminal},
			}
			printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: "think"})
			printer.emit(iota.Event{Type: iota.EventTextDelta, Text: "answer"})
			printer.emit(iota.Event{Type: iota.EventModelResponse})
			printer.emit(iota.Event{Type: iota.EventTextDelta, Text: "continued"})
			printer.emit(iota.Event{Type: iota.EventRunEnd})
			if stdoutTerminal {
				if stderr.String() != "[thinking]\nthink\n[/thinking]\n" || !strings.Contains(stdout.String(), "\x1b[36m") {
					t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			} else if stdout.String() != "answercontinued" || !strings.Contains(stderr.String(), "\x1b[90m") {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}
