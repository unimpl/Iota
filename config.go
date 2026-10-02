package iota

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// DefaultMaxTurns 限制单次运行的模型往返次数，避免工具循环无限持续。
const DefaultMaxTurns = 20

// Config 配置 Agent 的模型、工具和单次运行上限；MaxTurns 为零时使用默认值。
type Config struct {
	Provider     Provider
	Model        string
	SystemPrompt string
	Tools        []Tool
	MaxTurns     int
}

// New 校验配置并预编译所有工具 schema，让无效工具在运行前失败。
func New(config Config) (*Agent, error) {
	if config.Provider == nil {
		return nil, errors.New("provider is required")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("model is required")
	}
	if config.MaxTurns < 0 {
		return nil, errors.New("max turns cannot be negative")
	}
	if config.MaxTurns == 0 {
		config.MaxTurns = DefaultMaxTurns
	}

	seen := make(map[string]struct{}, len(config.Tools))
	compiled := make([]compiledTool, 0, len(config.Tools))
	for _, tool := range config.Tools {
		if !toolNamePattern.MatchString(tool.Name) {
			return nil, fmt.Errorf("invalid tool name %q", tool.Name)
		}
		if _, ok := seen[tool.Name]; ok {
			return nil, fmt.Errorf("duplicate tool name %q", tool.Name)
		}
		if tool.Execute == nil {
			return nil, fmt.Errorf("tool %q has no execute function", tool.Name)
		}
		if len(tool.Schema) == 0 {
			return nil, fmt.Errorf("tool %q has no schema", tool.Name)
		}
		if err := rejectExternalReferences(tool.Schema); err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", tool.Name, err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(tool.Schema))
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", tool.Name, err)
		}
		compiler := jsonschema.NewCompiler()
		uri := "mem://tools/" + tool.Name + ".json"
		if err := compiler.AddResource(uri, document); err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", tool.Name, err)
		}
		schema, err := compiler.Compile(uri)
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", tool.Name, err)
		}
		seen[tool.Name] = struct{}{}
		compiled = append(compiled, compiledTool{tool: tool, schema: schema})
	}

	return &Agent{
		provider:     config.Provider,
		model:        config.Model,
		systemPrompt: config.SystemPrompt,
		tools:        compiled,
		maxTurns:     config.MaxTurns,
	}, nil
}
