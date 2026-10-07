package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chzyer/readline"
	iota "github.com/unimpl/Iota"
)

// interactive 用行编辑器读取输入，按字符和显示宽度处理中文删除与光标移动。
// 每次只读取一行，运行模型时恢复普通终端模式，以便继续响应中断信号。
func interactive(agent *iota.Agent, signals <-chan os.Signal, stdin *os.File, stdout, stderr io.Writer, session *iota.Session) int {
	input := readline.NewCancelableStdin(stdin)
	config := inputConfig(stdin, stderr)
	config.Stdin = input
	editor, err := readline.NewEx(config)
	if err != nil {
		input.Close()
		printLine(stderr, colorError, "iota: initialize input editor: "+err.Error())
		return 1
	}
	closed := false
	closeEditor := func() {
		if closed {
			return
		}
		closed = true
		input.Close()
		if err := editor.Close(); err != nil {
			printLine(stderr, colorError, "iota: restore terminal settings: "+err.Error())
		}
	}
	defer closeEditor()
	for {
		prompt := "[you] > "
		if agent.Mode() == iota.ModePlan {
			prompt = "[you:plan] > "
		}
		editor.SetPrompt(styleFor(stderr).text(colorUserLabel, prompt))
		result := make(chan readline.Result, 1)
		go func() {
			line, err := editor.Readline()
			result <- readline.Result{Line: line, Error: err}
		}()
		select {
		case <-signals:
			closeEditor()
			<-result
			fmt.Fprintln(stderr)
			return 0
		case read := <-result:
			if errors.Is(read.Error, io.EOF) || errors.Is(read.Error, readline.ErrInterrupt) {
				return 0
			}
			if read.Error != nil {
				printLine(stderr, colorError, "iota: "+read.Error.Error())
				return 1
			}
			line := strings.TrimSpace(read.Line)
			switch line {
			case "":
				continue
			case "/exit":
				return 0
			case "/reset":
				if err := session.Reset(agent); err != nil {
					printLine(stderr, colorError, "iota: "+err.Error())
				} else {
					printLine(stderr, colorThinking, "conversation reset")
				}
				continue
			}
			output := eventOutput{stdout: stdout, stderr: stderr, stdoutStyle: styleFor(stdout), stderrStyle: styleFor(stderr)}
			handled, nextPrompt, err := planningCommand(line, agent, session, output.emit, stderr)
			if err != nil {
				printLine(stderr, colorError, "iota: "+err.Error())
				continue
			}
			if handled {
				if nextPrompt != "" {
					_ = execute(agent, nextPrompt, signals, stdout, stderr, false, session)
				}
				continue
			}
			_ = execute(agent, line, signals, stdout, stderr, false, session)
		}
	}
}
