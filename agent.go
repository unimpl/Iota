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
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const DefaultMaxTurns = 20

var (
	ErrBusy         = errors.New("agent is already running")
	ErrMaxTurns     = errors.New("maximum turns reached")
	ErrOutputLength = errors.New("model response was truncated by the output limit")
)

type Config struct {
	Provider     Provider
	Model        string
	SystemPrompt string
	Tools        []Tool
	MaxTurns     int
}

type compiledTool struct {
	tool   Tool
	schema *jsonschema.Schema
}

type Agent struct {
	provider     Provider
	model        string
	systemPrompt string
	tools        []compiledTool
	maxTurns     int

	mu       sync.Mutex
	running  bool
	messages []Message
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

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

func (a *Agent) Messages() []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneMessages(a.messages)
}

func (a *Agent) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return ErrBusy
	}
	a.messages = nil
	return nil
}

func (a *Agent) Run(ctx context.Context, prompt string, emit EmitFunc) (result RunResult, runErr error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return RunResult{}, ErrBusy
	}
	a.running = true
	userMessage := Message{Role: RoleUser, Content: prompt}
	a.messages = append(a.messages, userMessage)
	start := len(a.messages) - 1
	a.mu.Unlock()

	reason := "error"
	emitEvent(emit, Event{Type: EventRunStart})
	emitEvent(emit, Event{Type: EventMessageAdded, Message: cloneMessagePointer(userMessage)})
	defer func() {
		a.mu.Lock()
		result.Messages = cloneMessages(a.messages[start:])
		a.running = false
		a.mu.Unlock()
		if runErr == nil && result.StopReason != "" {
			reason = result.StopReason
		} else if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			reason = "aborted"
		}
		end := Event{Type: EventRunEnd, Turn: result.Turns, Reason: reason, IsError: runErr != nil}
		if runErr != nil {
			end.Error = runErr.Error()
		}
		emitEvent(emit, end)
	}()

	definitions := make([]ToolDefinition, len(a.tools))
	for i, tool := range a.tools {
		definitions[i] = ToolDefinition{Name: tool.tool.Name, Description: tool.tool.Description, Schema: append(json.RawMessage(nil), tool.tool.Schema...)}
	}

	for turn := 1; turn <= a.maxTurns; turn++ {
		result.Turns = turn
		if err := ctx.Err(); err != nil {
			return result, err
		}
		emitEvent(emit, Event{Type: EventTurnStart, Turn: turn})
		request := Request{
			Model:        a.model,
			SystemPrompt: a.systemPrompt,
			Messages:     a.Messages(),
			Tools:        definitions,
		}
		requestEvent := request
		requestEvent.Messages = cloneMessages(request.Messages)
		requestEvent.Tools = append([]ToolDefinition(nil), request.Tools...)
		for i := range requestEvent.Tools {
			requestEvent.Tools[i].Schema = append(json.RawMessage(nil), request.Tools[i].Schema...)
		}
		emitEvent(emit, Event{Type: EventModelRequest, Turn: turn, Request: &requestEvent})
		response, err := a.provider.Stream(ctx, request, func(delta Delta) {
			if delta.Text != "" {
				emitEvent(emit, Event{Type: EventTextDelta, Turn: turn, Text: delta.Text})
			}
		})
		if err != nil {
			return result, err
		}
		if response.StopReason == "length" {
			return result, ErrOutputLength
		}
		if err := validateToolCalls(response.ToolCalls); err != nil {
			return result, fmt.Errorf("invalid provider response: %w", err)
		}
		responseEvent := Event{Type: EventModelResponse, Turn: turn, Reason: response.StopReason}
		if response.Usage != nil {
			usage := *response.Usage
			responseEvent.Usage = &usage
		}
		emitEvent(emit, responseEvent)

		assistant := Message{Role: RoleAssistant, Content: response.Content, ToolCalls: cloneToolCalls(response.ToolCalls)}
		a.appendMessage(assistant, turn, emit)
		if len(response.ToolCalls) == 0 {
			result.Text = response.Content
			result.StopReason = response.StopReason
			if result.StopReason == "" {
				result.StopReason = "stop"
			}
			return result, nil
		}

		for index, call := range response.ToolCalls {
			if err := ctx.Err(); err != nil {
				a.appendCancelledResults(response.ToolCalls[index:], turn, emit)
				return result, err
			}
			emitEvent(emit, Event{Type: EventToolStart, Turn: turn, ToolCall: cloneToolCallPointer(call)})
			text, isError := a.executeTool(ctx, call)
			toolMessage := Message{Role: RoleTool, Content: text, ToolCallID: call.ID, ToolName: call.Name, IsError: isError}
			a.appendMessage(toolMessage, turn, emit)
			emitEvent(emit, Event{Type: EventToolEnd, Turn: turn, ToolCall: cloneToolCallPointer(call), ToolResult: text, IsError: isError})
			if err := ctx.Err(); err != nil {
				a.appendCancelledResults(response.ToolCalls[index+1:], turn, emit)
				return result, err
			}
		}
	}
	result.StopReason = "max_turns"
	return result, ErrMaxTurns
}

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

func (a *Agent) executeTool(ctx context.Context, call ToolCall) (string, bool) {
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
	text, err := selected.tool.Execute(ctx, call.Arguments)
	if err != nil {
		return err.Error(), true
	}
	return text, false
}

func (a *Agent) appendMessage(message Message, turn int, emit EmitFunc) {
	a.mu.Lock()
	a.messages = append(a.messages, message)
	a.mu.Unlock()
	emitEvent(emit, Event{Type: EventMessageAdded, Turn: turn, Message: cloneMessagePointer(message)})
}

func (a *Agent) appendCancelledResults(calls []ToolCall, turn int, emit EmitFunc) {
	for _, call := range calls {
		a.appendMessage(Message{Role: RoleTool, Content: "operation canceled before tool execution", ToolCallID: call.ID, ToolName: call.Name, IsError: true}, turn, emit)
	}
}

func emitEvent(emit EmitFunc, event Event) {
	if emit != nil {
		emit(event)
	}
}

func cloneMessages(messages []Message) []Message {
	result := make([]Message, len(messages))
	for i, message := range messages {
		result[i] = message
		result[i].ToolCalls = cloneToolCalls(message.ToolCalls)
	}
	return result
}

func cloneToolCalls(calls []ToolCall) []ToolCall {
	result := make([]ToolCall, len(calls))
	for i, call := range calls {
		result[i] = call
		result[i].Arguments = append(json.RawMessage(nil), call.Arguments...)
	}
	return result
}

func cloneToolCallPointer(call ToolCall) *ToolCall {
	copy := call
	copy.Arguments = append(json.RawMessage(nil), call.Arguments...)
	return &copy
}

func cloneMessagePointer(message Message) *Message {
	copy := message
	copy.ToolCalls = cloneToolCalls(message.ToolCalls)
	return &copy
}
