package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultSystemPrompt 给未配置项目规则时的 Agent 提供基本行为约束。
const defaultSystemPrompt = `You are a concise coding agent. Inspect relevant files before changing them. Use tools only when needed, report tool errors accurately, and finish with a clear result.`

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
