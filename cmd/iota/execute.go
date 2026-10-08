package main

import (
	"context"
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
		output := eventOutput{stdout: stdout, stderr: stderr, stdoutStyle: styleFor(stdout), stderrStyle: styleFor(stderr)}
		result, err := session.Run(ctx, agent, prompt, output.emit)
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-signals:
		cancel()
		completed := <-done
		_ = completed
		printLine(stderr, colorTool, "iota: canceled")
		if single {
			return 130
		}
		return 0
	case completed := <-done:
		fmt.Fprintln(stdout)
		if completed.err != nil {
			printLine(stderr, colorError, "iota: "+formatRunError(completed.err))
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
		return message + "; use /compact to summarize older context, or /reset to clear the in-memory conversation"
	}
	return message
}
