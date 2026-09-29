package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
	builtins "github.com/unimpl/Iota/tools"
)

// defaultSystemPrompt 给未配置项目规则时的 Agent 提供基本行为约束。
const defaultSystemPrompt = `You are a concise coding agent. Inspect relevant files before changing them. Use tools only when needed, report tool errors accurately, and finish with a clear result.`

// options 是合并文件、环境变量和命令行参数后的运行配置。
type options struct {
	prompt   string
	model    string
	baseURL  string
	apiKey   string
	cwd      string
	system   string
	tools    string
	maxTurns int
	timeout  time.Duration
}

// main 把 CLI 退出码交给操作系统；可测试的控制流程保留在 run 中。
func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run 初始化模型、工具与会话日志，再按输入来源选择单次或交互模式。
// 参数错误返回 2，执行错误返回 1，用户中断的单次模式返回 130。
func run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "iota:", err)
		return 2
	}
	if opts.model == "" {
		fmt.Fprintln(stderr, "iota: model is required; use --model or IOTA_MODEL")
		return 2
	}
	absCWD, err := filepath.Abs(opts.cwd)
	if err != nil {
		fmt.Fprintln(stderr, "iota:", err)
		return 2
	}
	info, err := os.Stat(absCWD)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "iota: invalid working directory %q\n", absCWD)
		return 2
	}
	systemPrompt, err := loadSystemPrompt(absCWD, opts.system)
	if err != nil {
		fmt.Fprintln(stderr, "iota:", err)
		return 2
	}
	selectedTools, err := createTools(absCWD, opts.tools, opts.timeout)
	if err != nil {
		fmt.Fprintln(stderr, "iota:", err)
		return 2
	}
	provider, err := openaicompat.New(openaicompat.Config{
		BaseURL: opts.baseURL,
		APIKey:  opts.apiKey,
		Timeout: opts.timeout,
	})
	if err != nil {
		fmt.Fprintln(stderr, "iota:", err)
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
		fmt.Fprintln(stderr, "iota:", err)
		return 2
	}

	terminal := isTerminal(stdin)
	if opts.prompt != "" && !terminal {
		fmt.Fprintln(stderr, "iota: -p cannot be combined with piped standard input")
		return 2
	}
	log, err := newSessionLog(opts.model, absCWD)
	if err != nil {
		fmt.Fprintln(stderr, "iota:", err)
		return 1
	}
	// 即使运行失败也尝试写入结束事件并关闭日志，避免查看器看到悬空会话。
	defer func() {
		if err := log.write("", "session_end", nil); err != nil {
			fmt.Fprintln(stderr, "iota: session recording incomplete:", err)
		}
		if err := log.close(); err != nil {
			fmt.Fprintln(stderr, "iota: close session file:", err)
		}
	}()
	fmt.Fprintln(stderr, "session:", log.path)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	if opts.prompt != "" {
		return execute(agent, opts.prompt, signals, stdout, stderr, true, log)
	}
	if !terminal {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "iota:", err)
			return 1
		}
		if strings.TrimSpace(string(data)) == "" {
			fmt.Fprintln(stderr, "iota: standard input is empty")
			return 2
		}
		return execute(agent, string(data), signals, stdout, stderr, true, log)
	}
	return interactive(agent, signals, stdin, stdout, stderr, log)
}

// parseOptions 加载本地配置，再交给可注入依赖的解析函数处理覆盖关系。
func parseOptions(args []string, stderr io.Writer) (options, error) {
	config, err := loadConfig()
	if err != nil {
		return options{}, err
	}
	return parseOptionsWithConfig(args, stderr, config, os.LookupEnv)
}

// parseOptionsWithConfig 按文件、环境变量、命令行的顺序应用配置。
// 最后校验轮数和超时，避免无效值进入执行循环。
func parseOptionsWithConfig(args []string, stderr io.Writer, config fileConfig, lookupEnv func(string) (string, bool)) (options, error) {
	opts, err := optionsFromConfig(config)
	if err != nil {
		return options{}, err
	}
	if err := applyEnvironment(&opts, lookupEnv); err != nil {
		return options{}, err
	}
	flags := flag.NewFlagSet("iota", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.prompt, "p", "", "run one prompt and exit")
	flags.StringVar(&opts.model, "model", opts.model, "model name")
	flags.StringVar(&opts.baseURL, "base-url", opts.baseURL, "OpenAI-compatible base URL")
	flags.StringVar(&opts.cwd, "cwd", opts.cwd, "working directory")
	flags.StringVar(&opts.system, "system", opts.system, "additional system prompt")
	flags.StringVar(&opts.tools, "tools", opts.tools, "comma-separated tools, or none")
	flags.IntVar(&opts.maxTurns, "max-turns", opts.maxTurns, "maximum model turns per run")
	flags.DurationVar(&opts.timeout, "timeout", opts.timeout, "timeout for each model request and bash command")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if opts.maxTurns <= 0 {
		return options{}, errors.New("--max-turns must be positive")
	}
	if opts.timeout <= 0 {
		return options{}, errors.New("--timeout must be positive")
	}
	return opts, nil
}

// loadSystemPrompt 读取用户目录与工作目录中的提示文件，再追加命令行提示和 AGENTS.md。
func loadSystemPrompt(cwd, additional string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return loadSystemPromptFrom(cwd, home, additional)
}

// loadSystemPromptFrom 按基础提示、额外提示、追加文件、项目规则的顺序组装提示。
// 两类 .iota 文件分别按项目优先查找；空 SYSTEM.md 使用默认提示，但不回退用户文件。
func loadSystemPromptFrom(cwd, home, additional string) (string, error) {
	base := defaultSystemPrompt
	custom, found, err := readSystemPromptFile(cwd, home, "SYSTEM.md")
	if err != nil {
		return "", err
	}
	if found && strings.TrimSpace(custom) != "" {
		base = custom
	}
	parts := []string{base}
	if strings.TrimSpace(additional) != "" {
		parts = append(parts, additional)
	}
	appendPrompt, found, err := readSystemPromptFile(cwd, home, "APPEND_SYSTEM.md")
	if err != nil {
		return "", err
	}
	if found && strings.TrimSpace(appendPrompt) != "" {
		parts = append(parts, appendPrompt)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "AGENTS.md"))
	if err == nil {
		parts = append(parts, "Project instructions from AGENTS.md:\n"+string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read AGENTS.md: %w", err)
	}
	return strings.Join(parts, "\n\n"), nil
}

// readSystemPromptFile 优先读取工作目录的 .iota 文件，缺失时才查找用户目录。
// 已找到但无法读取的文件会报错，避免悄悄使用较低优先级的提示。
func readSystemPromptFile(cwd, home, name string) (string, bool, error) {
	paths := []string{filepath.Join(cwd, ".iota", name)}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".iota", name))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data), true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("read %s: %w", path, err)
		}
	}
	return "", false, nil
}

// createTools 按配置顺序选择内置工具，并去掉重复名称。
// 空列表或 none 明确禁用工具，未知名称会报错。
func createTools(cwd, list string, timeout time.Duration) ([]iota.Tool, error) {
	available := map[string]iota.Tool{
		"read":  builtins.NewRead(cwd),
		"write": builtins.NewWrite(cwd),
		"edit":  builtins.NewEdit(cwd),
		"bash":  builtins.NewBash(cwd, timeout),
	}
	if strings.TrimSpace(list) == "" || strings.EqualFold(strings.TrimSpace(list), "none") {
		return nil, nil
	}
	var result []iota.Tool
	seen := make(map[string]bool)
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		tool, ok := available[name]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		if !seen[name] {
			result = append(result, tool)
			seen[name] = true
		}
	}
	return result, nil
}

// interactive 逐行读取终端输入，处理退出与重置命令后运行普通提示。
// 扫描放在独立协程中，主协程才能同时响应中断信号。
func interactive(agent *iota.Agent, signals <-chan os.Signal, stdin *os.File, stdout, stderr io.Writer, log *sessionLog) int {
	lines := make(chan string)
	errorsChannel := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdin)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		errorsChannel <- scanner.Err()
	}()
	for {
		fmt.Fprint(stderr, "> ")
		select {
		case <-signals:
			fmt.Fprintln(stderr)
			return 0
		case err := <-errorsChannel:
			if err != nil {
				fmt.Fprintln(stderr, "iota:", err)
				return 1
			}
			fmt.Fprintln(stderr)
			return 0
		case line := <-lines:
			line = strings.TrimSpace(line)
			switch line {
			case "":
				continue
			case "/exit":
				return 0
			case "/reset":
				if err := agent.Reset(); err != nil {
					fmt.Fprintln(stderr, "iota:", err)
				} else {
					if err := log.write("", "session_reset", nil); err != nil {
						fmt.Fprintln(stderr, "iota: session recording incomplete:", err)
					}
					fmt.Fprintln(stderr, "conversation reset")
				}
				continue
			}
			_ = execute(agent, line, signals, stdout, stderr, false, log)
		}
	}
}

// execute 运行一次提示并转发文本和工具事件；中断时等待 Agent 完成清理。
func execute(agent *iota.Agent, prompt string, signals <-chan os.Signal, stdout, stderr io.Writer, single bool, log *sessionLog) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runID := ""
	if log != nil {
		var err error
		runID, err = newUUID()
		if err != nil {
			fmt.Fprintln(stderr, "iota:", err)
			return 1
		}
	}
	// outcome 把运行结果和错误一起从工作协程传回，避免阻塞信号处理。
	type outcome struct {
		result iota.RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		recordingErrorReported := false
		result, err := agent.Run(ctx, prompt, func(event iota.Event) {
			if writeErr := log.event(runID, event); writeErr != nil && !recordingErrorReported {
				fmt.Fprintln(stderr, "iota: session recording incomplete:", writeErr)
				recordingErrorReported = true
			}
			switch event.Type {
			case iota.EventTextDelta:
				fmt.Fprint(stdout, event.Text)
			case iota.EventToolStart:
				fmt.Fprintf(stderr, "[%s]\n", event.ToolCall.Name)
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

// isTerminal 根据文件模式区分交互输入与管道输入。
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
