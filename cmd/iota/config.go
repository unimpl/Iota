package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

// defaultTools 是未显式配置时开放给模型的内置工具列表。
const defaultTools = "read,list,write,edit,bash,save_plan,update_plan"

// fileConfig 对应 TOML 配置；指针和切片保留“未设置”与“显式置空”的区别。
type fileConfig struct {
	Model           string   `toml:"model"`
	BaseURL         string   `toml:"base_url"`
	APIKey          string   `toml:"api_key"`
	CWD             string   `toml:"cwd"`
	System          string   `toml:"system"`
	Tools           []string `toml:"tools"`
	MaxTurns        *int     `toml:"max_turns"`
	Timeout         string   `toml:"timeout"`
	SaveSession     *bool    `toml:"save_session"`
	Resume          string   `toml:"resume"`
	Mode            string   `toml:"mode"`
	KeepRecentTurns *int     `toml:"compaction_keep_recent_turns"`
}

// loadConfig 获取用户目录和当前目录，供配置查找使用。
func loadConfig() (fileConfig, error) {
	home, homeErr := os.UserHomeDir()
	cwd, err := os.Getwd()
	if err != nil {
		return fileConfig{}, fmt.Errorf("get current working directory: %w", err)
	}
	if homeErr != nil {
		home = ""
	}
	return loadConfigFrom(home, cwd)
}

// loadConfigFrom 优先读取用户配置，缺失时才读取工作目录配置。
// 只接受已知字段，避免配置拼写错误被静默忽略。
func loadConfigFrom(home, cwd string) (fileConfig, error) {
	paths := make([]string, 0, 2)
	if home != "" {
		paths = append(paths, filepath.Join(home, ".iota", "config.toml"))
	}
	paths = append(paths, filepath.Join(cwd, "config.toml"))

	for _, path := range paths {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fileConfig{}, fmt.Errorf("inspect config %s: %w", path, err)
		}
		var config fileConfig
		metadata, err := toml.DecodeFile(path, &config)
		if err != nil {
			return fileConfig{}, fmt.Errorf("read config %s: %w", path, err)
		}
		if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
			keys := make([]string, len(undecoded))
			for index, key := range undecoded {
				keys[index] = key.String()
			}
			return fileConfig{}, fmt.Errorf("read config %s: unknown keys: %s", path, strings.Join(keys, ", "))
		}
		return config, nil
	}
	return fileConfig{}, nil
}

// optionsFromConfig 用默认值补齐文件配置；空工具数组表示禁用全部工具。
func optionsFromConfig(config fileConfig) (options, error) {
	opts := options{
		model:           config.Model,
		baseURL:         config.BaseURL,
		apiKey:          config.APIKey,
		cwd:             ".",
		system:          config.System,
		tools:           defaultTools,
		maxTurns:        iota.DefaultMaxTurns,
		timeout:         openaicompat.DefaultTimeout,
		resume:          config.Resume,
		mode:            string(iota.ModeDefault),
		modeRequested:   config.Mode != "",
		keepRecentTurns: iota.DefaultKeepRecentTurns,
	}
	if config.Mode != "" {
		opts.mode = config.Mode
	}
	if config.SaveSession != nil {
		opts.noSession = !*config.SaveSession
	}
	if config.CWD != "" {
		opts.cwd = config.CWD
	}
	if config.Tools != nil {
		opts.tools = strings.Join(config.Tools, ",")
	}
	if config.MaxTurns != nil {
		opts.maxTurns = *config.MaxTurns
	}
	if config.KeepRecentTurns != nil {
		if *config.KeepRecentTurns <= 0 {
			return options{}, errors.New("compaction_keep_recent_turns must be positive")
		}
		opts.keepRecentTurns = *config.KeepRecentTurns
	}
	if config.Timeout != "" {
		timeout, err := time.ParseDuration(config.Timeout)
		if err != nil {
			return options{}, fmt.Errorf("config timeout: %w", err)
		}
		opts.timeout = timeout
	}
	return opts, nil
}

// applyEnvironment 用环境变量覆盖文件配置，并解析 API key 的变量引用。
// 查找函数可替换，便于测试配置优先级而不依赖进程环境。
func applyEnvironment(opts *options, lookup func(string) (string, bool)) error {
	// setString 只在变量存在时覆盖，允许显式空值覆盖配置文件。
	setString := func(name string, target *string) {
		if value, ok := lookup(name); ok {
			*target = value
		}
	}
	setString("IOTA_MODEL", &opts.model)
	setString("OPENAI_BASE_URL", &opts.baseURL)
	if value, ok := lookup("OPENAI_API_KEY"); ok {
		opts.apiKey = value
	} else if name, ok := environmentReference(opts.apiKey); ok {
		value, exists := lookup(name)
		if !exists {
			return fmt.Errorf("api_key references unset environment variable %s", name)
		}
		opts.apiKey = value
	}
	setString("IOTA_CWD", &opts.cwd)
	setString("IOTA_SYSTEM", &opts.system)
	setString("IOTA_TOOLS", &opts.tools)
	setString("IOTA_RESUME", &opts.resume)
	if value, ok := lookup("IOTA_MODE"); ok {
		opts.mode = value
		opts.modeRequested = true
	}
	if value, ok := lookup("IOTA_SAVE_SESSION"); ok {
		save, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("IOTA_SAVE_SESSION: %w", err)
		}
		opts.noSession = !save
	}
	if value, ok := lookup("IOTA_MAX_TURNS"); ok {
		maxTurns, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("IOTA_MAX_TURNS: %w", err)
		}
		opts.maxTurns = maxTurns
	}
	if value, ok := lookup("IOTA_TIMEOUT"); ok {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("IOTA_TIMEOUT: %w", err)
		}
		opts.timeout = timeout
	}
	return nil
}

// environmentReference 只识别合法的 $NAME 或 ${NAME}，避免将普通 key 误当变量。
func environmentReference(value string) (string, bool) {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		value = value[2 : len(value)-1]
	} else if strings.HasPrefix(value, "$") {
		value = value[1:]
	} else {
		return "", false
	}
	if value == "" {
		return "", false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return "", false
	}
	return value, true
}
