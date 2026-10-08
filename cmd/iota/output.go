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
	if o.reasoningOpen && (event.Type == iota.EventTextDelta || event.Type == iota.EventToolCallDelta || event.Type == iota.EventModelResponse || event.Type == iota.EventRunEnd || event.Type == iota.EventCompactionStart) {
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "\n[/thinking]"))
		o.reasoningOpen = false
	}
	if o.assistantOpen && (event.Type == iota.EventReasoningDelta || event.Type == iota.EventToolCallDelta || event.Type == iota.EventToolStart || event.Type == iota.EventModelResponse || event.Type == iota.EventRunEnd || event.Type == iota.EventCompactionStart) {
		if !o.assistantEnded {
			fmt.Fprintln(o.stdout)
		}
		o.assistantOpen = false
	}
	switch event.Type {
	case iota.EventCompactionStart:
		message := "compacting context (" + event.Reason + ")"
		if state := event.Compaction; state != nil {
			message += ": current " + formatContextUsage(state.BeforeUsage, state.ContextLimitTokens) + " (full request, not the selected old history)"
		}
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, message))
	case iota.EventCompactionPrepared:
		input := event.CompactionInput
		if input == nil {
			break
		}
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, fmt.Sprintf("selected old history: %d user turns, %d messages, %d characters, approximately %d tokens", input.UserTurns, len(input.Messages), input.Chars, input.Usage.Tokens)))
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, fmt.Sprintf("protected recent history: %d user turns, %d messages; system instructions and tool definitions stay separate", input.KeptTurns, input.KeptMessages)))
		if input.HasPreviousSummary {
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "selected history also includes the previous checkpoint summary"))
		}
		for index, message := range input.Messages {
			if index == 5 {
				fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, fmt.Sprintf("  … %d more messages; inspect compaction_prepared in the session viewer", len(input.Messages)-index)))
				break
			}
			text := message.Content
			if text == "" && len(message.ToolCalls) > 0 {
				data, _ := json.Marshal(message.ToolCalls)
				text = string(data)
			}
			characters := []rune(text)
			if len(characters) > 160 {
				text = string(characters[:160]) + "…"
			}
			label := string(message.Role)
			if message.ContextSummary {
				label = "previous summary"
			}
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, fmt.Sprintf("  [%s] %q", label, text)))
		}
	case iota.EventCompactionEnd:
		if event.IsError {
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorError, "compaction failed: "+event.Error))
			if state := event.Compaction; state != nil && state.AfterChars > 0 {
				fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "full context before: "+formatContextUsage(state.BeforeUsage, state.ContextLimitTokens)))
				fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "candidate full context (not applied): "+formatContextUsage(state.AfterUsage, state.ContextLimitTokens)))
			}
		} else if state := event.Compaction; state != nil {
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, fmt.Sprintf("context compacted (%s): %d → %d characters; kept %d recent turns", state.Stage, state.BeforeChars, state.AfterChars, state.KeptTurns)))
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "context before: "+formatContextUsage(state.BeforeUsage, state.ContextLimitTokens)))
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "context after: "+formatContextUsage(state.AfterUsage, state.ContextLimitTokens)))
			if state.SummaryPath != "" {
				fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "summary: "+state.SummaryPath))
			}
		}
	case iota.EventModeChanged:
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "mode: "+string(event.Collaboration.Mode)))
	case iota.EventPlanSaved:
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "plan saved: "+event.Collaboration.Plan.Path))
	case iota.EventPlanApproved:
		fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "plan approved: "+event.Collaboration.Plan.Path))
	case iota.EventPlanUpdated:
		for _, step := range event.Collaboration.Progress.Plan {
			fmt.Fprintln(o.stderr, o.stderrStyle.text(colorThinking, "["+step.Status+"] "+step.Step))
		}
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

func formatContextUsage(usage iota.ContextUsage, limit int) string {
	if usage.Tokens <= 0 {
		return "size unavailable; available percentage unknown"
	}
	label := fmt.Sprintf("%d tokens", usage.Tokens)
	if usage.Estimated {
		label = "approximately " + label
	}
	if limit > 0 {
		label += fmt.Sprintf(" / %d capacity", limit)
	}
	if usage.RemainingPercent != nil {
		label += fmt.Sprintf("; approximately %.1f%% available", *usage.RemainingPercent)
	} else {
		label += "; available percentage unknown (capacity unavailable)"
	}
	return label
}
