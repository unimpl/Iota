package app

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

// DefaultTools 是未显式配置时开放给模型的内置工具列表。
const DefaultTools = "read,list,search,find_files,write,edit,bash,save_plan,update_progress,search_history,read_history,request_user_input,read_skill,read_skill_resource,install_skill"

// FileConfig 对应 TOML 配置；指针和切片保留“未设置”与“显式置空”的区别。
type FileConfig struct {
	Model                 string   `toml:"model"`
	BaseURL               string   `toml:"base_url"`
	APIKey                string   `toml:"api_key"`
	CWD                   string   `toml:"cwd"`
	System                string   `toml:"system"`
	Tools                 []string `toml:"tools"`
	MaxTurns              *int     `toml:"max_turns"`
	Timeout               string   `toml:"timeout"`
	SaveSession           *bool    `toml:"save_session"`
	Resume                string   `toml:"resume"`
	Mode                  string   `toml:"mode"`
	KeepRecentTokens      *int     `toml:"compaction_keep_recent_tokens"`
	CompactionTargetRatio *float64 `toml:"compaction_target_ratio"`
}

// Settings is the resolved configuration shared by the standalone CLI and server.
// Transport-specific flags are kept by each executable.
type Settings struct {
	Model, BaseURL, APIKey, CWD, System, Tools string
	MaxTurns                                   int
	Timeout                                    time.Duration
	Resume, Mode                               string
	ModeRequested, NoSession                   bool
	KeepRecentTokens                           int
	CompactionTargetRatio                      float64
}

// LoadConfig 获取用户目录和当前目录，供配置查找使用。
func LoadConfig() (FileConfig, error) {
	home, homeErr := os.UserHomeDir()
	cwd, err := os.Getwd()
	if err != nil {
		return FileConfig{}, fmt.Errorf("get current working directory: %w", err)
	}
	if homeErr != nil {
		home = ""
	}
	return LoadConfigFrom(home, cwd)
}

// LoadConfigFrom 优先读取用户配置，缺失时才读取工作目录配置。
// 只接受已知字段，避免配置拼写错误被静默忽略。
func LoadConfigFrom(home, cwd string) (FileConfig, error) {
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
			return FileConfig{}, fmt.Errorf("inspect config %s: %w", path, err)
		}
		var config FileConfig
		metadata, err := toml.DecodeFile(path, &config)
		if err != nil {
			return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
		}
		if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
			keys := make([]string, len(undecoded))
			for index, key := range undecoded {
				keys[index] = key.String()
			}
			return FileConfig{}, fmt.Errorf("read config %s: unknown keys: %s", path, strings.Join(keys, ", "))
		}
		return config, nil
	}
	return FileConfig{}, nil
}

// SettingsFromConfig 用默认值补齐文件配置；空工具数组表示禁用全部工具。
func SettingsFromConfig(config FileConfig) (Settings, error) {
	opts := Settings{
		Model:                 config.Model,
		BaseURL:               config.BaseURL,
		APIKey:                config.APIKey,
		CWD:                   ".",
		System:                config.System,
		Tools:                 DefaultTools,
		MaxTurns:              iota.DefaultMaxTurns,
		Timeout:               openaicompat.DefaultTimeout,
		Resume:                config.Resume,
		Mode:                  string(iota.ModeDefault),
		ModeRequested:         config.Mode != "",
		KeepRecentTokens:      iota.DefaultKeepRecentTokens,
		CompactionTargetRatio: iota.DefaultCompactionTargetRatio,
	}
	if config.Mode != "" {
		opts.Mode = config.Mode
	}
	if config.SaveSession != nil {
		opts.NoSession = !*config.SaveSession
	}
	if config.CWD != "" {
		opts.CWD = config.CWD
	}
	if config.Tools != nil {
		opts.Tools = strings.Join(config.Tools, ",")
	}
	if config.MaxTurns != nil {
		opts.MaxTurns = *config.MaxTurns
	}
	if config.KeepRecentTokens != nil {
		if *config.KeepRecentTokens <= 0 {
			return Settings{}, errors.New("compaction_keep_recent_tokens must be positive")
		}
		opts.KeepRecentTokens = *config.KeepRecentTokens
	}
	if config.CompactionTargetRatio != nil {
		if math.IsNaN(*config.CompactionTargetRatio) || math.IsInf(*config.CompactionTargetRatio, 0) || *config.CompactionTargetRatio <= 0 || *config.CompactionTargetRatio >= 1 {
			return Settings{}, errors.New("compaction_target_ratio must be between zero and one")
		}
		opts.CompactionTargetRatio = *config.CompactionTargetRatio
	}
	if config.Timeout != "" {
		timeout, err := time.ParseDuration(config.Timeout)
		if err != nil {
			return Settings{}, fmt.Errorf("config timeout: %w", err)
		}
		opts.Timeout = timeout
	}
	return opts, nil
}

// ApplyEnvironment 用环境变量覆盖文件配置，并解析 API key 的变量引用。
// 查找函数可替换，便于测试配置优先级而不依赖进程环境。
func ApplyEnvironment(opts *Settings, lookup func(string) (string, bool)) error {
	// setString 只在变量存在时覆盖，允许显式空值覆盖配置文件。
	setString := func(name string, target *string) {
		if value, ok := lookup(name); ok {
			*target = value
		}
	}
	setString("IOTA_MODEL", &opts.Model)
	setString("OPENAI_BASE_URL", &opts.BaseURL)
	if value, ok := lookup("OPENAI_API_KEY"); ok {
		opts.APIKey = value
	} else if name, ok := EnvironmentReference(opts.APIKey); ok {
		value, exists := lookup(name)
		if !exists {
			return fmt.Errorf("api_key references unset environment variable %s", name)
		}
		opts.APIKey = value
	}
	setString("IOTA_CWD", &opts.CWD)
	setString("IOTA_SYSTEM", &opts.System)
	setString("IOTA_TOOLS", &opts.Tools)
	setString("IOTA_RESUME", &opts.Resume)
	if value, ok := lookup("IOTA_MODE"); ok {
		opts.Mode = value
		opts.ModeRequested = true
	}
	if value, ok := lookup("IOTA_SAVE_SESSION"); ok {
		save, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("IOTA_SAVE_SESSION: %w", err)
		}
		opts.NoSession = !save
	}
	if value, ok := lookup("IOTA_MAX_TURNS"); ok {
		maxTurns, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("IOTA_MAX_TURNS: %w", err)
		}
		opts.MaxTurns = maxTurns
	}
	if value, ok := lookup("IOTA_TIMEOUT"); ok {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("IOTA_TIMEOUT: %w", err)
		}
		opts.Timeout = timeout
	}
	return nil
}

// EnvironmentReference 只识别合法的 $NAME 或 ${NAME}，避免将普通 key 误当变量。
func EnvironmentReference(value string) (string, bool) {
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
