package iota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type UserInputOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type UserInputQuestion struct {
	ID                  string            `json:"id"`
	Question            string            `json:"question"`
	Options             []UserInputOption `json:"options"`
	RecommendedOptionID string            `json:"recommended_option_id"`
}

// UserInputRequest supplies questions and one-based progress within a model's tool batch.
type UserInputRequest struct {
	Questions []UserInputQuestion `json:"questions"`
	CallIndex int                 `json:"-"`
	CallCount int                 `json:"-"`
}

// UserInputAnswer.Source is selected, recommended, custom, or cancelled.
// Option answers include both the stable option ID and its label as Value.
type UserInputAnswer struct {
	OptionID string `json:"option_id,omitempty"`
	Value    string `json:"value,omitempty"`
	Source   string `json:"source"`
}

type UserInputResponse struct {
	Answers   map[string]UserInputAnswer `json:"answers"`
	Cancelled bool                       `json:"cancelled,omitempty"`
}

// UserInputHandler must honor ctx cancellation and retain partial answers on cancellation.
// Returning Cancelled or context.Canceled aborts the Run after recording paired tool results.
type UserInputHandler func(context.Context, UserInputRequest) (UserInputResponse, error)

// NewRequestUserInputTool declares interactive input; Config.UserInputHandler implements its UI.
func NewRequestUserInputTool() Tool {
	return Tool{Name: "request_user_input", ReadOnly: true, userInputTool: true,
		Description: "Ask independent questions and wait for answers within this run. Prefer one to three questions. Each question requires 2-3 distinct feasible options, concrete tradeoffs, and a recommended_option_id. Users can choose an option or write a custom answer; empty input selects the recommendation. Cancellation ends the run. Dependent questions require a later call after receiving answers.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"items":{"type":"object","properties":{"id":{"type":"string","minLength":1},"question":{"type":"string","minLength":1},"recommended_option_id":{"type":"string","minLength":1},"options":{"type":"array","minItems":2,"maxItems":3,"items":{"type":"object","properties":{"id":{"type":"string","minLength":1},"label":{"type":"string","minLength":1},"description":{"type":"string","minLength":1}},"required":["id","label","description"],"additionalProperties":false}}},"required":["id","question","options","recommended_option_id"],"additionalProperties":false}}},"required":["questions"],"additionalProperties":false}`),
	}
}

func validateUserInputQuestions(questions []UserInputQuestion) error {
	if len(questions) == 0 {
		return errors.New("at least one question is required")
	}
	seen := make(map[string]bool)
	for _, question := range questions {
		if strings.TrimSpace(question.ID) == "" || strings.TrimSpace(question.Question) == "" || seen[question.ID] {
			return errors.New("question IDs must be non-empty and unique, and questions must be non-empty")
		}
		seen[question.ID] = true
		if len(question.Options) < 2 || len(question.Options) > 3 {
			return fmt.Errorf("question %q requires 2-3 options", question.ID)
		}
		options := make(map[string]bool)
		for _, option := range question.Options {
			if strings.TrimSpace(option.ID) == "" || strings.TrimSpace(option.Label) == "" || strings.TrimSpace(option.Description) == "" || options[option.ID] {
				return fmt.Errorf("question %q requires unique non-empty option IDs, labels, and descriptions", question.ID)
			}
			options[option.ID] = true
		}
		if !options[question.RecommendedOptionID] {
			return fmt.Errorf("question %q recommendation must identify an existing option", question.ID)
		}
	}
	return nil
}

func (a *Agent) executeUserInput(ctx context.Context, call ToolCall, callIndex, callCount int) (string, bool, error) {
	var request UserInputRequest
	if err := json.Unmarshal(call.Arguments, &request); err != nil {
		return err.Error(), true, nil
	}
	if err := validateUserInputQuestions(request.Questions); err != nil {
		return "invalid tool arguments: " + err.Error(), true, nil
	}
	request.CallIndex, request.CallCount = callIndex, callCount
	response, err := a.userInputHandler(ctx, request)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		response.Cancelled = true
	}
	if response.Cancelled && err == nil {
		err = context.Canceled
	}
	if response.Answers == nil {
		response.Answers = make(map[string]UserInputAnswer)
	}
	var responseErr error
	for _, question := range request.Questions {
		answer, exists := response.Answers[question.ID]
		if !exists && response.Cancelled {
			response.Answers[question.ID] = UserInputAnswer{Source: "cancelled"}
			continue
		}
		valid := false
		switch answer.Source {
		case "custom":
			valid = answer.OptionID == "" && strings.TrimSpace(answer.Value) != ""
		case "cancelled":
			valid = response.Cancelled && answer.OptionID == "" && answer.Value == ""
		case "selected", "recommended":
			for _, option := range question.Options {
				if option.ID == answer.OptionID && (answer.Source != "recommended" || option.ID == question.RecommendedOptionID) {
					answer.Value = option.Label
					response.Answers[question.ID] = answer
					valid = true
					break
				}
			}
		}
		if !valid {
			responseErr = errors.Join(responseErr, fmt.Errorf("invalid or missing user answer for %q", question.ID))
		}
	}
	if len(response.Answers) > len(request.Questions) {
		responseErr = errors.Join(responseErr, errors.New("user input response contains unknown questions"))
	}
	text, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return marshalErr.Error(), true, marshalErr
	}
	return string(text), responseErr != nil || (err != nil && !response.Cancelled), errors.Join(err, responseErr)
}
