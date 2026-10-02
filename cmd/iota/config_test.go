package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadConfigPrefersHome 确认用户配置与项目配置同时存在时优先使用前者。
func TestLoadConfigPrefersHome(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(home, ".iota", "config.toml"), `model = "home-model"`)
	writeConfig(t, filepath.Join(cwd, "config.toml"), `model = "cwd-model"`)

	config, err := loadConfigFrom(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if config.Model != "home-model" {
		t.Fatalf("model = %q", config.Model)
	}
}

// TestLoadConfigFallsBackToWorkingDirectory 验证用户配置缺失时读取当前目录配置。
func TestLoadConfigFallsBackToWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, "config.toml"), `model = "cwd-model"`)

	config, err := loadConfigFrom(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if config.Model != "cwd-model" {
		t.Fatalf("model = %q", config.Model)
	}
}

// TestLoadConfigDecodesValues 验证空工具列表和可选数值不会与缺省值混淆。
func TestLoadConfigDecodesValues(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".iota", "config.toml"), `
model = "configured-model"
tools = []
max_turns = 6
timeout = "15s"
`)

	config, err := loadConfigFrom(home, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if config.Model != "configured-model" || config.Tools == nil || len(config.Tools) != 0 || config.MaxTurns == nil || *config.MaxTurns != 6 || config.Timeout != "15s" {
		t.Fatalf("config = %+v", config)
	}
}

// TestLoadConfigRejectsUnknownKey 确保配置键拼写错误不会被静默忽略。
func TestLoadConfigRejectsUnknownKey(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".iota", "config.toml"), `modle = "typo"`)

	_, err := loadConfigFrom(home, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unknown keys: modle") {
		t.Fatalf("error = %v", err)
	}
}

// TestOptionPrecedence 验证命令行、环境变量、配置文件逐级覆盖的顺序。
func TestOptionPrecedence(t *testing.T) {
	maxTurns := 7
	config := fileConfig{
		Model:    "file-model",
		BaseURL:  "https://file.example",
		APIKey:   "file-key",
		CWD:      "/file/cwd",
		System:   "file system",
		Tools:    []string{"read"},
		MaxTurns: &maxTurns,
		Timeout:  "30s",
	}
	environment := map[string]string{
		"IOTA_MODEL":      "env-model",
		"OPENAI_BASE_URL": "https://env.example",
		"OPENAI_API_KEY":  "env-key",
		"IOTA_MAX_TURNS":  "9",
		"IOTA_TIMEOUT":    "45s",
		"IOTA_SYSTEM":     "env system",
		"IOTA_CWD":        "/env/cwd",
	}
	lookup := func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	}
	var stderr bytes.Buffer
	opts, err := parseOptionsWithConfig(
		[]string{"--model", "flag-model", "--max-turns", "11", "--cwd", "/flag/cwd"},
		&stderr,
		config,
		lookup,
	)
	if err != nil {
		t.Fatal(err)
	}
	if opts.model != "flag-model" || opts.maxTurns != 11 || opts.cwd != "/flag/cwd" {
		t.Fatalf("flag precedence failed: %+v", opts)
	}
	if opts.baseURL != "https://env.example" || opts.apiKey != "env-key" || opts.timeout != 45*time.Second || opts.system != "env system" {
		t.Fatalf("environment precedence failed: %+v", opts)
	}
	if opts.tools != "read" {
		t.Fatalf("file config tools = %q", opts.tools)
	}
}

// TestAPIKeyEnvironmentReference 验证文件配置中的变量引用在运行时解析。
func TestAPIKeyEnvironmentReference(t *testing.T) {
	opts, err := optionsFromConfig(fileConfig{APIKey: "$API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := applyEnvironment(&opts, func(name string) (string, bool) {
		if name == "API_KEY" {
			return "resolved-key", true
		}
		return "", false
	}); err != nil {
		t.Fatal(err)
	}
	if opts.apiKey != "resolved-key" {
		t.Fatalf("apiKey = %q", opts.apiKey)
	}
}

// TestOpenAIAPIKeyOverridesConfigReference 确认显式环境变量优先于配置中的引用。
func TestOpenAIAPIKeyOverridesConfigReference(t *testing.T) {
	opts, err := optionsFromConfig(fileConfig{APIKey: "$MISSING_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := applyEnvironment(&opts, func(name string) (string, bool) {
		if name == "OPENAI_API_KEY" {
			return "override-key", true
		}
		return "", false
	}); err != nil {
		t.Fatal(err)
	}
	if opts.apiKey != "override-key" {
		t.Fatalf("apiKey = %q", opts.apiKey)
	}
}

// TestEmptyToolListDisablesTools 确认显式空数组会禁用全部工具。
func TestEmptyToolListDisablesTools(t *testing.T) {
	opts, err := optionsFromConfig(fileConfig{Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := createTools(t.TempDir(), opts.tools, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tools != nil {
		t.Fatalf("tools = %+v", tools)
	}
}

// TestSessionOptions 验证会话选择的配置覆盖与互斥检查。
func TestSessionOptions(t *testing.T) {
	save := false
	var stderr bytes.Buffer
	opts, err := parseOptionsWithConfig([]string{"--no-session=false", "--resume", "/sessions/selected.jsonl"}, &stderr,
		fileConfig{SaveSession: &save}, func(string) (string, bool) { return "", false })
	if err != nil || opts.noSession || opts.resume != "/sessions/selected.jsonl" {
		t.Fatalf("options=%+v err=%v", opts, err)
	}
	if _, err := parseOptionsWithConfig([]string{"--no-session", "--resume", "session.jsonl"}, &stderr,
		fileConfig{}, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("expected incompatible session flags")
	}
	opts, err = parseOptionsWithConfig(nil, &stderr, fileConfig{SaveSession: &save}, func(name string) (string, bool) {
		if name == "IOTA_SAVE_SESSION" {
			return "true", true
		}
		if name == "IOTA_RESUME" {
			return "restored.jsonl", true
		}
		return "", false
	})
	if err != nil || opts.noSession || opts.resume != "restored.jsonl" {
		t.Fatalf("options=%+v err=%v", opts, err)
	}
}

// writeConfig 创建测试配置及其父目录，避免测试依赖已有的本地文件。
func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
