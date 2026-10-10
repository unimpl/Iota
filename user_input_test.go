package iota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func inputQuestion(id string) UserInputQuestion {
	return UserInputQuestion{ID: id, Question: "Where should data live?", RecommendedOptionID: "user", Options: []UserInputOption{
		{ID: "project", Label: "Project", Description: "Shared with the repository"},
		{ID: "user", Label: "User", Description: "Independent of the working directory"},
	}}
}

func inputCall(t *testing.T, id string, questions ...UserInputQuestion) ToolCall {
	t.Helper()
	args, err := json.Marshal(UserInputRequest{Format: UserInputStructured, Questions: questions})
	if err != nil {
		t.Fatal(err)
	}
	return ToolCall{ID: id, Name: "request_user_input", Arguments: args}
}

func TestUserInputBatchesStayInOneRunInBothModes(t *testing.T) {
	for _, mode := range []Mode{ModePlan, ModeDefault} {
		t.Run(string(mode), func(t *testing.T) {
			provider := &fakeProvider{responses: []Response{
				{ToolCalls: []ToolCall{inputCall(t, "first", inputQuestion("one"), inputQuestion("two")), inputCall(t, "second", inputQuestion("three"))}},
				{Content: "done"},
			}}
			callbacks := 0
			var agent *Agent
			var err error
			agent, err = New(Config{Provider: provider, Model: "test", Mode: mode, Tools: []Tool{NewRequestUserInputTool()}, UserInputHandler: func(ctx context.Context, request UserInputRequest) (UserInputResponse, error) {
				callbacks++
				if request.CallIndex != callbacks || request.CallCount != 2 || len(provider.requests) != 1 {
					t.Fatalf("progress=%+v requests=%d", request, len(provider.requests))
				}
				if _, err := agent.Run(ctx, "another run", nil); !errors.Is(err, ErrBusy) {
					t.Fatalf("run was not waiting: %v", err)
				}
				response := UserInputResponse{Answers: make(map[string]UserInputAnswer)}
				for _, question := range request.Questions {
					response.Answers[question.ID] = UserInputAnswer{OptionID: "user", Source: "recommended"}
				}
				return response, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			var starts, ends int
			result, err := agent.Run(t.Context(), "plan", func(event Event) {
				if event.Type == EventRunStart {
					starts++
				}
				if event.Type == EventRunEnd {
					ends++
				}
			})
			if err != nil || result.Text != "done" || starts != 1 || ends != 1 || callbacks != 2 || len(provider.requests) != 2 {
				t.Fatalf("result=%+v err=%v callbacks=%d starts=%d ends=%d", result, err, callbacks, starts, ends)
			}
			second := provider.requests[1]
			if len(provider.requests[0].Tools) != 1 || !strings.Contains(second.Messages[2].Content, `"source":"recommended"`) || !strings.Contains(second.Messages[3].Content, `"three"`) {
				t.Fatalf("request=%+v", second)
			}
		})
	}
}

func TestUserInputCancellationPreservesPartialAnswersAndRestore(t *testing.T) {
	executed := false
	provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{
		inputCall(t, "first", inputQuestion("one"), inputQuestion("two")),
		inputCall(t, "second", inputQuestion("three")),
		{ID: "write", Name: "side_effect", Arguments: json.RawMessage(`{}`)},
	}}}}
	config := Config{Provider: provider, Model: "test", Tools: []Tool{NewRequestUserInputTool(), {Name: "side_effect", Schema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { executed = true; return "", nil }}}, UserInputHandler: func(context.Context, UserInputRequest) (UserInputResponse, error) {
		return UserInputResponse{Cancelled: true, Answers: map[string]UserInputAnswer{"one": {Source: "custom", Value: "my choice"}}}, nil
	}}
	agent, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	var end Event
	result, err := session.Run(t.Context(), agent, "go", func(event Event) {
		if event.Type == EventRunEnd {
			end = event
		}
	})
	if !errors.Is(err, context.Canceled) || result.StopReason != "aborted" || end.Reason != "aborted" || executed || len(provider.requests) != 1 {
		t.Fatalf("result=%+v err=%v end=%+v executed=%t", result, err, end, executed)
	}
	messages := agent.Messages()
	if len(messages) != 5 || messages[2].IsError || !strings.Contains(messages[2].Content, "my choice") || !strings.Contains(messages[2].Content, `"source":"cancelled"`) || !messages[3].IsError || !messages[4].IsError {
		t.Fatalf("messages=%+v", messages)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record sessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Type == string(EventRunStart) {
			starts++
		}
	}
	if starts != 1 {
		t.Fatal("input started a new run")
	}
	restored, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	next, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(next); err != nil {
		t.Fatal(err)
	}
	if len(next.Messages()) != 5 || next.Messages()[2].Content != messages[2].Content {
		t.Fatal("partial answers lost on restore")
	}
}

func TestUserInputUnavailableWithoutHandlerAndInvalidQuestions(t *testing.T) {
	for _, mode := range []Mode{ModePlan, ModeDefault} {
		provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{inputCall(t, "call", inputQuestion("one"))}}, {Content: "missing input"}}}
		agent, err := New(Config{Provider: provider, Model: "test", Mode: mode, Tools: []Tool{NewRequestUserInputTool()}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := agent.Run(t.Context(), "go", nil); err != nil {
			t.Fatal(err)
		}
		if len(provider.requests[0].Tools) != 0 || !strings.Contains(provider.requests[0].SystemPrompt, "Do not call request_user_input or ask questions") || !agent.Messages()[2].IsError {
			t.Fatal("non-interactive input was offered")
		}
	}
	for _, invalid := range []string{"duplicate_question", "duplicate_option", "missing_recommendation", "blank_label", "one_option", "no_options"} {
		t.Run(invalid, func(t *testing.T) {
			question := inputQuestion("one")
			questions := []UserInputQuestion{question}
			switch invalid {
			case "duplicate_question":
				questions = append(questions, question)
			case "duplicate_option":
				questions[0].Options[1].ID = "project"
			case "missing_recommendation":
				questions[0].RecommendedOptionID = "missing"
			case "blank_label":
				questions[0].Options[0].Label = "  "
			case "one_option":
				questions[0].Options = questions[0].Options[:1]
			case "no_options":
				questions[0].Options = nil
			}
			provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{inputCall(t, "call", questions...)}}, {Content: "fix"}}}
			agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{NewRequestUserInputTool()}, UserInputHandler: func(context.Context, UserInputRequest) (UserInputResponse, error) {
				t.Fatal("invalid arguments reached UI")
				return UserInputResponse{}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := agent.Run(t.Context(), "go", nil); err != nil {
				t.Fatal(err)
			}
			if !agent.Messages()[2].IsError {
				t.Fatal("invalid questions accepted")
			}
		})
	}
}

func TestUserInputContextCancellationAndHandlerFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{inputCall(t, "call", inputQuestion("one")), inputCall(t, "next", inputQuestion("two"))}}}}
		started := make(chan struct{})
		agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{NewRequestUserInputTool()}, UserInputHandler: func(ctx context.Context, _ UserInputRequest) (UserInputResponse, error) {
			close(started)
			if fail {
				return UserInputResponse{}, errors.New("UI disconnected")
			}
			<-ctx.Done()
			return UserInputResponse{}, ctx.Err()
		}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := agent.Run(ctx, "go", nil); done <- err }()
		<-started
		cancel()
		err = <-done
		if err == nil || len(provider.requests) != 1 || len(agent.Messages()) != 4 {
			t.Fatalf("err=%v messages=%+v", err, agent.Messages())
		}
	}
}

func TestUserInputRejectsInvalidHandlerAnswersBeforeContinuing(t *testing.T) {
	for _, response := range []UserInputResponse{
		{},
		{Answers: map[string]UserInputAnswer{"one": {Source: "recommended", OptionID: "project"}}},
		{Answers: map[string]UserInputAnswer{"one": {Source: "custom", Value: "  "}}},
		{Answers: map[string]UserInputAnswer{"one": {Source: "selected", OptionID: "missing"}}},
		{Answers: map[string]UserInputAnswer{"one": {Source: "unanswered"}}},
		{Answers: map[string]UserInputAnswer{"one": {Source: "selected", OptionID: "user"}, "unknown": {Source: "custom", Value: "extra"}}},
	} {
		provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{inputCall(t, "call", inputQuestion("one")), inputCall(t, "next", inputQuestion("two"))}}}}
		callbacks := 0
		agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{NewRequestUserInputTool()}, UserInputHandler: func(context.Context, UserInputRequest) (UserInputResponse, error) { callbacks++; return response, nil }})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := agent.Run(t.Context(), "go", nil); err == nil {
			t.Fatalf("invalid response accepted: %+v", response)
		}
		if callbacks != 1 || len(provider.requests) != 1 || !agent.Messages()[2].IsError || len(agent.Messages()) != 4 {
			t.Fatal("continued after invalid input response")
		}
	}
}

func TestUserInputFreeformRepliesAndUnansweredStayInOneRun(t *testing.T) {
	for _, mode := range []Mode{ModePlan, ModeDefault} {
		t.Run(string(mode), func(t *testing.T) {
			args := json.RawMessage(`{"format":"freeform","questions":[{"id":"wish","question":"What superpower would you like?"},{"id":"why","question":"What is your favorite color?"}]}`)
			provider := &fakeProvider{responses: []Response{
				{ToolCalls: []ToolCall{{ID: "game", Name: "request_user_input", Arguments: args}}},
				{Content: "Your second answer was empty."},
			}}
			config := Config{Provider: provider, Model: "test", Mode: mode, Tools: []Tool{NewRequestUserInputTool()}, UserInputHandler: func(ctx context.Context, request UserInputRequest) (UserInputResponse, error) {
				if request.Format != UserInputFreeform || len(request.Questions[0].Options) != 0 || request.Questions[0].RecommendedOptionID != "" {
					t.Fatalf("request=%+v", request)
				}
				return UserInputResponse{Answers: map[string]UserInputAnswer{
					"wish": {Source: "custom", Value: "time travel"},
					"why":  {Source: "unanswered"},
				}}, nil
			}}
			agent, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			session, err := NewSession(t.TempDir(), SessionInfo{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := session.Run(t.Context(), agent, "Let's play a question game", nil)
			if err != nil || result.Turns != 2 || len(provider.requests) != 2 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !strings.Contains(provider.requests[0].SystemPrompt, "including games and casual conversation") || !strings.Contains(provider.requests[0].SystemPrompt, "only this format requires") {
				t.Fatal("missing scoped input instructions")
			}
			var response UserInputResponse
			if err := json.Unmarshal([]byte(agent.Messages()[2].Content), &response); err != nil {
				t.Fatal(err)
			}
			if response.Cancelled || response.Answers["why"].Source != "unanswered" || response.Answers["why"].Value != "" || agent.Messages()[2].IsError {
				t.Fatalf("response=%+v", response)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			restored, err := OpenSession(session.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			next, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := restored.Restore(next); err != nil {
				t.Fatal(err)
			}
			if next.Messages()[2].Content != agent.Messages()[2].Content {
				t.Fatal("unanswered reply lost on restore")
			}
		})
	}
}

func TestUserInputFormatValidationBeforeUI(t *testing.T) {
	for _, args := range []string{
		`{"questions":[{"id":"one","question":"Why?"}]}`,
		`{"format":"unknown","questions":[{"id":"one","question":"Why?"}]}`,
		`{"format":"structured","questions":[{"id":"one","question":"Why?"}]}`,
		`{"format":"freeform","questions":[{"id":"one","question":"Why?","recommended_option_id":"a"}]}`,
		`{"format":"freeform","questions":[{"id":"one","question":"Why?","options":[{"id":"a","label":"A","description":"First"},{"id":"b","label":"B","description":"Second"}]}]}`,
		`{"format":"freeform","questions":[{"id":"one","question":"Why?"},{"id":"one","question":"Again?"}]}`,
	} {
		provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{{ID: "question", Name: "request_user_input", Arguments: json.RawMessage(args)}}}, {Content: "corrected"}}}
		agent, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{NewRequestUserInputTool()}, UserInputHandler: func(context.Context, UserInputRequest) (UserInputResponse, error) {
			t.Fatal("invalid format reached UI")
			return UserInputResponse{}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := agent.Run(t.Context(), "go", nil); err != nil {
			t.Fatal(err)
		}
		if !agent.Messages()[2].IsError {
			t.Fatalf("invalid format accepted: %s", args)
		}
	}
}
