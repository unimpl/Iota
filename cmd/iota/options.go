package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	iota "github.com/unimpl/Iota"
)

// options 是合并文件、环境变量和命令行参数后的运行配置。
type options struct {
	prompt          string
	model           string
	baseURL         string
	apiKey          string
	cwd             string
	system          string
	tools           string
	maxTurns        int
	timeout         time.Duration
	noSession       bool
	resume          string
	resumeRequested bool
	mode            string
	modeRequested   bool
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
	flags.BoolVar(&opts.noSession, "no-session", opts.noSession, "disable saving conversations")
	flags.StringVar(&opts.resume, "resume", opts.resume, "restore a session by path or UUID; use an empty value for the latest session")
	flags.StringVar(&opts.mode, "mode", opts.mode, "collaboration mode: default or plan")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	opts.resumeRequested = opts.resume != ""
	flags.Visit(func(option *flag.Flag) {
		if option.Name == "resume" {
			opts.resumeRequested = true
		}
		if option.Name == "mode" {
			opts.modeRequested = true
		}
	})
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if opts.maxTurns <= 0 {
		return options{}, errors.New("--max-turns must be positive")
	}
	if opts.timeout <= 0 {
		return options{}, errors.New("--timeout must be positive")
	}
	if opts.noSession && opts.resumeRequested {
		return options{}, errors.New("--no-session cannot be combined with --resume")
	}
	if opts.mode != string(iota.ModeDefault) && opts.mode != string(iota.ModePlan) {
		return options{}, errors.New("--mode must be default or plan")
	}
	return opts, nil
}
