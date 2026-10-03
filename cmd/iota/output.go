package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chzyer/readline"
	iota "github.com/unimpl/Iota"
)

const (
	colorReset      = "\x1b[0m"
	colorUser       = "\x1b[32m"
	colorUserLabel  = "\x1b[1;32m"
	colorThinking   = "\x1b[90m"
	colorAssistant  = "\x1b[36m"
	colorReplyLabel = "\x1b[1;36m"
	colorTool       = "\x1b[33m"
	colorError      = "\x1b[1;31m"
)

// terminalStyle 分别记录输出是否为终端、是否允许颜色；禁用颜色仍保留角色标识。
type terminalStyle struct {
	terminal bool
	colored  bool
}

func styleFor(output io.Writer) terminalStyle {
	file, ok := output.(*os.File)
	terminal := ok && readline.IsTerminal(int(file.Fd()))
	return terminalStyle{terminal: terminal, colored: terminal && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"}
}

// text 为每次输出单独复位，避免 stdout、stderr 和输入重绘互相继承颜色。
func (s terminalStyle) text(color, text string) string {
	if !s.colored || text == "" {
		return text
	}
	return color + text + colorReset
}

// Paint 只改变 readline 的显示副本，不把颜色序列写入用户输入或历史。
func (s terminalStyle) Paint(line []rune, _ int) []rune {
	return []rune(s.text(colorUser, string(line)))
}

func printLine(output io.Writer, color, text string) {
	fmt.Fprintln(output, styleFor(output).text(color, text))
}

// eventOutput 在流式事件切换时关闭当前文本块，避免工具状态接在未换行的正文后。
type eventOutput struct {
	stdout         io.Writer
	stderr         io.Writer
	stdoutStyle    terminalStyle
	stderrStyle    terminalStyle
	reasoningOpen  bool
	assistantOpen  bool
	assistantEnded bool
}

func (o *eventOutput) emit(event iota.Event) {
	if o.reasoningOpen && (event.Type == iota.EventTextDelta || event.Type == iota.EventToolCallDelta || event.Type == iota.EventModelResponse || event.Type == iota.EventRunEnd) {
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "\n[/thinking]"))
		o.reasoningOpen = false
	}
	if o.assistantOpen && (event.Type == iota.EventReasoningDelta || event.Type == iota.EventToolCallDelta || event.Type == iota.EventToolStart || event.Type == iota.EventModelResponse || event.Type == iota.EventRunEnd) {
		if !o.assistantEnded {
			fmt.Fprintln(o.stdout)
		}
		o.assistantOpen = false
	}
	switch event.Type {
	case iota.EventReasoningDelta:
		if !o.reasoningOpen {
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "[thinking]"))
			o.reasoningOpen = true
		}
		fmt.Fprint(o.stderr, o.stderrStyle.text(colorThinking, event.Reasoning))
	case iota.EventTextDelta:
		if o.stdoutStyle.terminal {
			if !o.assistantOpen {
				fmt.Fprintln(o.stdout, o.stdoutStyle.text(colorReplyLabel, "[assistant]"))
				o.assistantOpen = true
			}
			o.assistantEnded = strings.HasSuffix(event.Text, "\n")
		}
		fmt.Fprint(o.stdout, o.stdoutStyle.text(colorAssistant, event.Text))
	case iota.EventToolStart:
		arguments := string(event.ToolCall.Arguments)
		if event.ToolCall.Name == "bash" {
			var args struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(event.ToolCall.Arguments, &args); err == nil && args.Command != "" {
				arguments = args.Command
			}
		}
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorTool, fmt.Sprintf("[%s] %s", event.ToolCall.Name, arguments)))
	case iota.EventToolEnd:
		if event.IsError {
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorError, fmt.Sprintf("[%s error] %s", event.ToolCall.Name, event.ToolResult)))
		}
	}
}
