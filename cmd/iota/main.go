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

const defaultSystemPrompt = `You are a concise coding agent. Inspect relevant files before changing them. Use tools only when needed, report tool errors accurately, and finish with a clear result.`

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

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

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

func parseOptions(args []string, stderr io.Writer) (options, error) {
	config, err := loadConfig()
	if err != nil {
		return options{}, err
	}
	return parseOptionsWithConfig(args, stderr, config, os.LookupEnv)
}

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

func loadSystemPrompt(cwd, additional string) (string, error) {
	parts := []string{defaultSystemPrompt}
	if strings.TrimSpace(additional) != "" {
		parts = append(parts, additional)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "AGENTS.md"))
	if err == nil {
		parts = append(parts, "Project instructions from AGENTS.md:\n"+string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read AGENTS.md: %w", err)
	}
	return strings.Join(parts, "\n\n"), nil
}

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

func formatRunError(err error) string {
	message := err.Error()
	lower := strings.ToLower(message)
	if strings.Contains(lower, "context") && (strings.Contains(lower, "length") || strings.Contains(lower, "token")) {
		return message + "; use /reset to clear the in-memory conversation"
	}
	return message
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
