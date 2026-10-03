package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	iota "github.com/unimpl/Iota"
)

// execute 运行一次提示并转发文本和工具事件；中断时等待 Agent 完成清理。
func execute(agent *iota.Agent, prompt string, signals <-chan os.Signal, stdout, stderr io.Writer, single bool, session *iota.Session) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// outcome 把运行结果和错误一起从工作协程传回，避免阻塞信号处理。
	type outcome struct {
		result iota.RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		reasoningOpen := false
		result, err := session.Run(ctx, agent, prompt, func(event iota.Event) {
			if reasoningOpen && (event.Type == iota.EventTextDelta || event.Type == iota.EventToolCallDelta || event.Type == iota.EventModelResponse || event.Type == iota.EventRunEnd) {
				fmt.Fprintln(stderr, "\n[/thinking]")
				reasoningOpen = false
			}
			switch event.Type {
			case iota.EventReasoningDelta:
				if !reasoningOpen {
					fmt.Fprintln(stderr, "[thinking]")
					reasoningOpen = true
				}
				fmt.Fprint(stderr, event.Reasoning)
			case iota.EventTextDelta:
				fmt.Fprint(stdout, event.Text)
			case iota.EventToolStart:
				if event.ToolCall.Name == "bash" {
					var args struct {
						Command string `json:"command"`
					}
					if err := json.Unmarshal(event.ToolCall.Arguments, &args); err == nil && args.Command != "" {
						fmt.Fprintf(stderr, "[bash] %s\n", args.Command)
						break
					}
				}
				fmt.Fprintf(stderr, "[%s] %s\n", event.ToolCall.Name, event.ToolCall.Arguments)
			case iota.EventToolEnd:
				if event.IsError {
					fmt.Fprintf(stderr, "[%s error] %s\n", event.ToolCall.Name, event.ToolResult)
				}
			}
		})
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-signals:
		cancel()
		completed := <-done
		_ = completed
		fmt.Fprintln(stderr, "iota: canceled")
		if single {
			return 130
		}
		return 0
	case completed := <-done:
		fmt.Fprintln(stdout)
		if completed.err != nil {
			fmt.Fprintln(stderr, "iota:", formatRunError(completed.err))
			return 1
		}
		return 0
	}
}

// formatRunError 在上下文容量错误后提示可用的重置操作。
func formatRunError(err error) string {
	message := err.Error()
	lower := strings.ToLower(message)
	if strings.Contains(lower, "context") && (strings.Contains(lower, "length") || strings.Contains(lower, "token")) {
		return message + "; use /reset to clear the in-memory conversation"
	}
	return message
}
