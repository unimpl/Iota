package main

import (
	"io"
	"os"
	"sync"

	"github.com/chzyer/readline"
)

// inputConfig 把输入编辑绑定到实际输入文件，提示和重绘都写入 stderr。
func inputConfig(stdin *os.File, output io.Writer) *readline.Config {
	fd := int(stdin.Fd())
	terminal := readline.IsTerminal(fd)
	var mu sync.Mutex
	var state *readline.State
	return &readline.Config{
		Prompt:         "> ",
		Stdout:         output,
		Stderr:         output,
		EOFPrompt:      "\n",
		FuncIsTerminal: func() bool { return terminal },
		FuncGetWidth: func() int {
			width, _, err := readline.GetSize(fd)
			if err != nil || width <= 0 {
				return 80
			}
			return width
		},
		FuncMakeRaw: func() error {
			mu.Lock()
			defer mu.Unlock()
			if !terminal || state != nil {
				return nil
			}
			var err error
			state, err = readline.MakeRaw(fd)
			return err
		},
		FuncExitRaw: func() error {
			mu.Lock()
			defer mu.Unlock()
			if state == nil {
				return nil
			}
			if err := readline.Restore(fd, state); err != nil {
				return err
			}
			state = nil
			return nil
		},
	}
}

// isTerminal 根据文件模式区分交互输入与管道输入。
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
