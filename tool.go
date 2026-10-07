package iota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// compiledTool 缓存参数 schema 的编译结果，避免每次工具调用重复编译。
type compiledTool struct {
	tool   Tool
	schema *jsonschema.Schema
}

// toolNamePattern 限制发送给模型的工具名，避免服务端不接受特殊字符。
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// rejectExternalReferences 禁止 schema 通过外部引用加载资源，避免校验时依赖网络或文件。
// 解析后还要确认没有额外的 JSON 内容。
func rejectExternalReferences(schema json.RawMessage) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(schema))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := ensureNoExternalReference(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("schema contains trailing data")
	}
	return nil
}

// ensureNoExternalReference 递归检查对象和数组；仅允许指向当前 schema 的 # 引用。
func ensureNoExternalReference(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("external $ref %q is not allowed", ref)
				}
			}
			if err := ensureNoExternalReference(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := ensureNoExternalReference(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateToolCalls 检查调用 ID、名称和 JSON 参数；重复 ID 会破坏结果配对。
func validateToolCalls(calls []ToolCall) error {
	seen := make(map[string]struct{}, len(calls))
	for index, call := range calls {
		if call.ID == "" {
			return fmt.Errorf("tool call %d has no ID", index)
		}
		if _, exists := seen[call.ID]; exists {
			return fmt.Errorf("duplicate tool call ID %q", call.ID)
		}
		if call.Name == "" {
			return fmt.Errorf("tool call %q has no name", call.ID)
		}
		if !json.Valid(call.Arguments) {
			return fmt.Errorf("tool call %q has invalid JSON arguments", call.ID)
		}
		seen[call.ID] = struct{}{}
	}
	return nil
}

// executeTool 校验参数后执行指定工具，并将失败转成模型可见的错误文本。
// 返回的布尔值表示工具错误，不表示整个 Agent 运行失败。
func (a *Agent) executeTool(ctx context.Context, call ToolCall, turn int, emit EmitFunc, record func(Event) error) (string, bool) {
	var selected *compiledTool
	for i := range a.tools {
		if a.tools[i].tool.Name == call.Name {
			selected = &a.tools[i]
			break
		}
	}
	if selected == nil {
		return fmt.Sprintf("tool %q not found", call.Name), true
	}
	var args any
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return "invalid tool arguments: " + err.Error(), true
	}
	if err := selected.schema.Validate(args); err != nil {
		return "invalid tool arguments: " + err.Error(), true
	}
	if err := ctx.Err(); err != nil {
		return err.Error(), true
	}
	if !a.toolAllowed(selected.tool) {
		return fmt.Sprintf("tool %q is unavailable in %s mode", call.Name, a.Mode()), true
	}
	if selected.tool.planTool {
		text, err := a.executePlanTool(ctx, call, turn, emit, record)
		if err != nil {
			return err.Error(), true
		}
		return text, false
	}
	text, err := selected.tool.Execute(ctx, call.Arguments)
	if err != nil {
		return err.Error(), true
	}
	return text, false
}
