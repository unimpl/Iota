package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	iota "github.com/unimpl/Iota"
)

const DefaultTimeout = 120 * time.Second

type Config struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	Timeout    time.Duration
}

type Provider struct {
	endpoint string
	apiKey   string
	client   *http.Client
	timeout  time.Duration
}

func New(config Config) (*Provider, error) {
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", baseURL)
	}
	if config.Timeout < 0 {
		return nil, errors.New("timeout cannot be negative")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	client := config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Provider{
		endpoint: strings.TrimRight(baseURL, "/") + "/chat/completions",
		apiKey:   config.APIKey,
		client:   client,
		timeout:  config.Timeout,
	}, nil
}

type requestBody struct {
	Model    string        `json:"model"`
	Messages []message     `json:"messages"`
	Tools    []toolWrapper `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
	Options  streamOptions `json:"stream_options"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type message struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type toolWrapper struct {
	Type     string       `json:"type"`
	Function functionTool `json:"function"`
}

type functionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *iota.Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (p *Provider) Stream(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	body, err := encodeRequest(request)
	if err != nil {
		return iota.Response{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return iota.Response{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	httpResponse, err := p.client.Do(httpRequest)
	if err != nil {
		return iota.Response{}, err
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(httpResponse.Body, 64<<10))
		if readErr != nil {
			return iota.Response{}, fmt.Errorf("chat completions returned %s: %w", httpResponse.Status, readErr)
		}
		return iota.Response{}, fmt.Errorf("chat completions returned %s: %s", httpResponse.Status, strings.TrimSpace(string(data)))
	}

	return consumeStream(httpResponse.Body, emit)
}

func encodeRequest(request iota.Request) ([]byte, error) {
	messages := make([]message, 0, len(request.Messages)+1)
	if request.SystemPrompt != "" {
		messages = append(messages, message{Role: "system", Content: request.SystemPrompt})
	}
	for _, source := range request.Messages {
		target := message{Role: string(source.Role), Content: source.Content, ToolCallID: source.ToolCallID}
		if source.Role == iota.RoleAssistant {
			target.ToolCalls = make([]wireToolCall, len(source.ToolCalls))
			for i, call := range source.ToolCalls {
				target.ToolCalls[i] = wireToolCall{
					ID:       call.ID,
					Type:     "function",
					Function: wireFunction{Name: call.Name, Arguments: string(call.Arguments)},
				}
			}
		}
		messages = append(messages, target)
	}
	tools := make([]toolWrapper, len(request.Tools))
	for i, tool := range request.Tools {
		tools[i] = toolWrapper{Type: "function", Function: functionTool{Name: tool.Name, Description: tool.Description, Parameters: tool.Schema}}
	}
	return json.Marshal(requestBody{
		Model:    request.Model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
		Options:  streamOptions{IncludeUsage: true},
	})
}

func consumeStream(reader io.Reader, emit func(iota.Delta)) (iota.Response, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var response iota.Response
	builders := make(map[int]*wireToolCall)
	maxIndex := -1
	done := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return iota.Response{}, fmt.Errorf("decode stream event: %w", err)
		}
		if chunk.Error != nil {
			return iota.Response{}, fmt.Errorf("provider error %s: %s", chunk.Error.Type, chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage := *chunk.Usage
			response.Usage = &usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				response.Content += *choice.Delta.Content
				if emit != nil && *choice.Delta.Content != "" {
					emit(iota.Delta{Text: *choice.Delta.Content})
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := builders[delta.Index]
				if call == nil {
					call = &wireToolCall{Index: delta.Index}
					builders[delta.Index] = call
				}
				if delta.ID != "" {
					call.ID = delta.ID
				}
				call.Function.Name += delta.Function.Name
				call.Function.Arguments += delta.Function.Arguments
				if delta.Index > maxIndex {
					maxIndex = delta.Index
				}
			}
			if choice.FinishReason != nil {
				response.StopReason = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return iota.Response{}, fmt.Errorf("read stream: %w", err)
	}
	if !done {
		return iota.Response{}, io.ErrUnexpectedEOF
	}
	if response.StopReason == "tool_calls" && maxIndex < 0 {
		return iota.Response{}, errors.New("provider ended with tool_calls but returned no tool calls")
	}
	for index := 0; index <= maxIndex; index++ {
		call := builders[index]
		if call == nil || call.ID == "" || call.Function.Name == "" {
			return iota.Response{}, fmt.Errorf("incomplete tool call at index %d", index)
		}
		arguments := json.RawMessage(call.Function.Arguments)
		if !json.Valid(arguments) {
			return iota.Response{}, fmt.Errorf("invalid tool arguments at index %d", index)
		}
		response.ToolCalls = append(response.ToolCalls, iota.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	return response, nil
}
