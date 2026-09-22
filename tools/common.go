package tools

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 50 * 1024
)

func resolvePath(cwd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(cwd, path)
}

func truncateHead(text string, maxLines, maxBytes int) (string, bool) {
	if len(text) <= maxBytes && lineCount(text) <= maxLines {
		return text, false
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	result := strings.Join(lines, "\n")
	if len(result) > maxBytes {
		data := []byte(result[:maxBytes])
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
		if index := bytes.LastIndexByte(data, '\n'); index >= 0 {
			data = data[:index]
		}
		result = string(data)
	}
	return result, true
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	count := strings.Count(text, "\n") + 1
	if strings.HasSuffix(text, "\n") {
		count--
	}
	return count
}

func truncationNote(text string) string {
	if text == "" {
		return fmt.Sprintf("[output truncated at %d lines or %d bytes]", DefaultMaxLines, DefaultMaxBytes)
	}
	return fmt.Sprintf("%s\n\n[output truncated at %d lines or %d bytes]", text, DefaultMaxLines, DefaultMaxBytes)
}
