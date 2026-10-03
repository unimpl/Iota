package main

import (
	"fmt"
	"strings"
	"time"

	iota "github.com/unimpl/Iota"
	builtins "github.com/unimpl/Iota/tools"
)

// createTools 按配置顺序选择内置工具，并去掉重复名称。
// 空列表或 none 明确禁用工具，未知名称会报错。
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
