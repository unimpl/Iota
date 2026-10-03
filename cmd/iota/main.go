package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

// main 把 CLI 退出码交给操作系统；可测试的控制流程保留在 run 中。
func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run 初始化模型、工具与会话日志，再按输入来源选择单次或交互模式。
// 参数错误返回 2，执行错误返回 1，用户中断的单次模式返回 130。
func run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		printLine(stderr, colorError, "iota: "+err.Error())
		return 2
	}
	if opts.model == "" {
		printLine(stderr, colorError, "iota: model is required; use --model or IOTA_MODEL")
		return 2
	}
	absCWD, err := filepath.Abs(opts.cwd)
	if err != nil {
		printLine(stderr, colorError, "iota: "+err.Error())
		return 2
	}
	info, err := os.Stat(absCWD)
	if err != nil || !info.IsDir() {
		printLine(stderr, colorError, fmt.Sprintf("iota: invalid working directory %q", absCWD))
		return 2
	}
	systemPrompt, err := loadSystemPrompt(absCWD, opts.system)
	if err != nil {
		printLine(stderr, colorError, "iota: "+err.Error())
		return 2
	}
	selectedTools, err := createTools(absCWD, opts.tools, opts.timeout)
	if err != nil {
		printLine(stderr, colorError, "iota: "+err.Error())
		return 2
	}
	provider, err := openaicompat.New(openaicompat.Config{
		BaseURL: opts.baseURL,
		APIKey:  opts.apiKey,
		Timeout: opts.timeout,
	})
	if err != nil {
		printLine(stderr, colorError, "iota: "+err.Error())
		return 2
	}
	agent, err := iota.New(iota.Config{
		Provider:     provider,
		Model:        opts.model,
		SystemPrompt: systemPrompt,
		Tools:        selectedTools,
		MaxTurns:     opts.maxTurns,
	})
	if err != nil {
		printLine(stderr, colorError, "iota: "+err.Error())
		return 2
	}

	terminal := isTerminal(stdin)
	if opts.prompt != "" && !terminal {
		printLine(stderr, colorError, "iota: -p cannot be combined with piped standard input")
		return 2
	}
	var session *iota.Session
	if !opts.noSession {
		if opts.resumeRequested {
			var path string
			path, err = resolveResumePath(opts.resume)
			if err == nil {
				session, err = iota.OpenSession(path)
			}
		} else {
			var home string
			home, err = os.UserHomeDir()
			if err == nil {
				session, err = iota.NewSession(filepath.Join(home, ".iota", "sessions"), iota.SessionInfo{Model: opts.model, CWD: absCWD})
			}
		}
		if err != nil {
			printLine(stderr, colorError, "iota: "+err.Error())
			return 1
		}
		defer func() {
			if err := session.Close(); err != nil {
				printLine(stderr, colorError, "iota: close session: "+err.Error())
			}
		}()
		if err := session.Restore(agent); err != nil {
			printLine(stderr, colorError, "iota: restore session: "+err.Error())
			return 1
		}
		printLine(stderr, colorThinking, "session: "+session.Path())
		defer func() {
			printLine(stderr, colorThinking, fmt.Sprintf("To resume this conversation, run: iota --resume '%s'", strings.ReplaceAll(session.Path(), "'", "'\"'\"'")))
		}()
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	if opts.prompt != "" {
		return execute(agent, opts.prompt, signals, stdout, stderr, true, session)
	}
	if !terminal {
		data, err := io.ReadAll(stdin)
		if err != nil {
			printLine(stderr, colorError, "iota: "+err.Error())
			return 1
		}
		if strings.TrimSpace(string(data)) == "" {
			printLine(stderr, colorError, "iota: standard input is empty")
			return 2
		}
		return execute(agent, string(data), signals, stdout, stderr, true, session)
	}
	return interactive(agent, signals, stdin, stdout, stderr, session)
}
