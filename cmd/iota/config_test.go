package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestLoadConfigRejectsUnknownKey(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".iota", "config.toml"), `modle = "typo"`)

	_, err := loadConfigFrom(home, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unknown keys: modle") {
		t.Fatalf("error = %v", err)
	}
}

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

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
