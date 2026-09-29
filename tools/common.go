package tools

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// 输出同时受行数与字节数限制，分别约束大量短行和少量长行。
const (
	// DefaultMaxLines 限制短行很多时的输出长度；与字节上限中先达到的限制一起生效。
	DefaultMaxLines = 2000
	// DefaultMaxBytes 限制单行很长时的输出长度；截断时会丢弃边界处不完整的 UTF-8 字符。
	DefaultMaxBytes = 50 * 1024
)

// resolvePath 让工具的相对路径统一以 cwd 为基准。
// 绝对路径会直接使用，且此函数不检查路径是否越过 cwd。
func resolvePath(cwd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(cwd, path)
}

// truncateHead 保留文本开头，避免过长的读取结果占满上下文。
// 超过字节限制时回退到完整的 UTF-8 字符及最近的换行边界；布尔值表示发生截断。
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

// lineCount 统计实际内容行数；末尾换行符不表示额外的空白行。
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

// truncationNote 标明输出可能不完整，防止调用方把截断结果当作文件全文。
func truncationNote(text string) string {
	if text == "" {
		return fmt.Sprintf("[output truncated at %d lines or %d bytes]", DefaultMaxLines, DefaultMaxBytes)
	}
	return fmt.Sprintf("%s\n\n[output truncated at %d lines or %d bytes]", text, DefaultMaxLines, DefaultMaxBytes)
}
