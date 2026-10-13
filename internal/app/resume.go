package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ResolveResumePath 将路径、UUID 或空值解析为可恢复的文件路径。
func ResolveResumePath(value string) (string, error) {
	if filepath.IsAbs(value) || strings.ContainsAny(value, `/\`) || strings.HasSuffix(value, ".jsonl") {
		return filepath.Abs(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return FindResumeSession(filepath.Join(home, ".iota", "sessions"), value)
}

// FindResumeSession 按修改时间选择最近的匹配文件；时间相同时按文件名倒序。
func FindResumeSession(dir, uuid string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read sessions directory: %w", err)
	}
	var selected string
	var latest time.Time
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") || !strings.Contains(entry.Name(), uuid) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("inspect session %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if selected == "" || info.ModTime().After(latest) || (info.ModTime().Equal(latest) && entry.Name() > selected) {
			selected, latest = entry.Name(), info.ModTime()
		}
	}
	if selected == "" {
		if uuid == "" {
			return "", fmt.Errorf("no saved sessions found in %s", dir)
		}
		return "", fmt.Errorf("no saved session matching %q in %s", uuid, dir)
	}
	return filepath.Join(dir, selected), nil
}
