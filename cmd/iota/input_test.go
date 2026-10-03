package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/chzyer/readline"
)

// TestInputChineseBackspace 验证中英文混输后的每次删除，同时检查整行擦除与重绘。
func TestInputChineseBackspace(t *testing.T) {
	for _, colored := range []bool{false, true} {
		for _, erase := range []byte{0x7f, 0x08} {
			for count, want := range []string{"你好abc", "你好ab", "你好a", "你好", "你", ""} {
				t.Run(fmt.Sprintf("color=%t/erase=%x/count=%d", colored, erase, count), func(t *testing.T) {
					stdin, err := os.CreateTemp(t.TempDir(), "input")
					if err != nil {
						t.Fatal(err)
					}
					defer stdin.Close()
					keys := "你好abc" + strings.Repeat(string(erase), count) + "\r"
					if _, err := io.WriteString(stdin, keys); err != nil {
						t.Fatal(err)
					}
					if _, err := stdin.Seek(0, io.SeekStart); err != nil {
						t.Fatal(err)
					}
					var output bytes.Buffer
					config := inputConfig(stdin, &output)
					if colored {
						config.Prompt = "\x1b[1;32m[you] > \x1b[0m"
						config.Painter = terminalStyle{terminal: true, colored: true}
					}
					config.Stdin = stdin
					config.ForceUseInteractive = true
					config.FuncOnWidthChanged = func(func()) {}
					editor, err := readline.NewEx(config)
					if err != nil {
						t.Fatal(err)
					}
					defer editor.Close()
					line, err := editor.Readline()
					if err != nil || line != want {
						t.Fatalf("line=%q err=%v, want %q", line, err, want)
					}
					// 回车前必须从行首清除旧内容；只移动一列无法擦除两个显示列的中文。
					redraw := "[you] > " + want + "\n"
					if colored {
						redraw = "\x1b[1;32m[you] > \x1b[0m\x1b[32m" + want + "\n\x1b[0m"
					}
					redraw = "\x1b[J\x1b[2K\r" + redraw
					if !strings.HasSuffix(output.String(), redraw) {
						t.Fatalf("output=%q, want final redraw %q", output.String(), redraw)
					}
				})
			}
		}
	}
}
