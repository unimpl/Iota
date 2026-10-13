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
func createTools(cwd, list string, timeout time.Duration, onStatus ...func(string)) ([]iota.Tool, error) {
	rg := builtins.RipgrepConfig{}
	if len(onStatus) > 0 {
		rg.OnStatus = onStatus[0]
	}
	available := map[string]iota.Tool{
		"read":                builtins.NewRead(cwd),
		"list":                builtins.NewList(cwd),
		"search":              builtins.NewSearch(cwd, rg),
		"find_files":          builtins.NewFindFiles(cwd, rg),
		"write":               builtins.NewWrite(cwd),
		"edit":                builtins.NewEdit(cwd),
		"bash":                builtins.NewBash(cwd, timeout),
		"save_plan":           iota.NewSavePlanTool(),
		"update_progress":     iota.NewUpdateProgressTool(),
		"search_history":      iota.NewSearchHistoryTool(),
		"read_history":        iota.NewReadHistoryTool(),
		"request_user_input":  iota.NewRequestUserInputTool(),
		"read_skill":          iota.NewReadSkillTool(),
		"read_skill_resource": iota.NewReadSkillResourceTool(),
		"install_skill":       iota.NewInstallSkillTool(),
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
