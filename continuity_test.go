package iota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepeatedCompactionInheritsOriginalSourceMetadata(t *testing.T) {
	provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
		if request.SystemPrompt == compactionPrompt {
			// Deliberately return no seq, run, or step identifiers.
			return Response{Content: "Previous work checkpoint.", StopReason: "stop"}, nil
		}
		return Response{Content: strings.Repeat("New investigation detail. ", 200), StopReason: "stop"}, nil
	}}
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	state := CollaborationState{Mode: ModeDefault, Progress: &PlanProgress{ID: "checklist", Version: 1,
		Plan: []PlanStep{{ID: "original-step", Step: "Investigate cache", Status: "in_progress"}},
	}}
	if err := session.recordState("", Event{Type: EventProgressUpdated, Collaboration: &state}); err != nil {
		t.Fatal(err)
	}
	run := 0
	for _, message := range compactionHistory() {
		if message.Role == RoleUser {
			run++
		}
		if run > 1 && message.Role == RoleAssistant {
			message.Content = strings.Repeat("Recent investigation detail. ", 200)
		}
		if err := session.write(fmt.Sprintf("original-run-%d", run), string(EventMessageAdded), Event{Type: EventMessageAdded, Message: cloneMessagePointer(message)}); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := New(Config{Provider: provider, Model: "test", KeepRecentTokens: 1, Tools: []Tool{NewReadHistoryTool()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Restore(agent); err != nil {
		t.Fatal(err)
	}
	originalTool := agent.Messages()[2]
	first, err := session.Compact(t.Context(), agent, "Keep original history references.", nil)
	if err != nil || first.Stage != "summary" || len(first.Sources) < 2 {
		t.Fatalf("first checkpoint sources=%+v err=%v", first.Sources, err)
	}
	for round := 0; round < 2; round++ {
		if _, err := session.Run(t.Context(), agent, "Continue the investigation.", nil); err != nil {
			t.Fatal(err)
		}
		next, err := session.Compact(t.Context(), agent, "Keep original history references.", nil)
		if err != nil || next.Stage != "summary" {
			t.Fatalf("round=%d stage=%s err=%v", round, next.Stage, err)
		}
		for _, original := range first.Sources {
			found := false
			for _, inherited := range next.Context.Messages[0].Sources {
				if inherited.RunID == original.RunID && inherited.StepID == original.StepID && inherited.StartSeq <= original.StartSeq && inherited.EndSeq >= original.EndSeq {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("round=%d lost original metadata: %+v", round, original)
			}
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	fresh, err := New(Config{Provider: provider, Model: "test", Tools: []Tool{NewReadHistoryTool()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Restore(fresh); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Messages()[0].Sources, agent.Messages()[0].Sources) {
		t.Fatal("reopening lost the inherited source metadata")
	}
	args := historyArguments{StartSeq: originalTool.Source.Seq, EndSeq: originalTool.Source.Seq,
		RunID: originalTool.Source.RunID, StepID: originalTool.Source.StepID}
	var recovered strings.Builder
	for pages := 0; pages < 20; pages++ {
		arguments, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		text, err := fresh.executeHistoryTool(t.Context(), ToolCall{Name: "read_history", Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		var page historyPage
		if err := json.Unmarshal([]byte(text), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 1 || page.Records[0].Source != *originalTool.Source {
			t.Fatal("original run/step metadata not preserved in history lookup")
		}
		recovered.WriteString(page.Records[0].Content)
		if page.NextSeq == 0 {
			break
		}
		if page.NextOffset <= args.Offset {
			t.Fatal("history pagination did not advance")
		}
		args.StartSeq, args.Offset = page.NextSeq, page.NextOffset
	}
	if recovered.String() != originalTool.Content {
		t.Fatal("original tool output cannot be read completely after repeated compaction")
	}
}

// Exercise real plan, approval, progress, input, compaction, and session APIs together.
// The summary deliberately omits the user's choice so state preservation does not
// depend on the summarizer faithfully repeating authoritative collaboration state.
func TestTaskContinuityAcrossCompactionAndRestore(t *testing.T) {
	for _, scenario := range []string{"manual_prune", "manual_summary", "automatic_summary"} {
		t.Run(scenario, func(t *testing.T) {
			const goal = "Implement the cache fix and run verification."
			const choice = "Use the user's isolated cache directory."
			const planContent = "# Cache fix\nImplement the cache fix and run verification."
			plans := filepath.Join(t.TempDir(), "plans")
			phase, overflows, answers, summaries := "plan", 0, 0, 0
			var before CollaborationState
			provider := &compactionProvider{respond: func(_ context.Context, request Request, _ int) (Response, error) {
				if request.SystemPrompt == compactionPrompt {
					summaries++
					return Response{Content: "Earlier investigation completed; verification remains pending.", StopReason: "stop"}, nil
				}
				switch phase {
				case "plan":
					phase = "plan_saved"
					return Response{StopReason: "tool_calls", ToolCalls: []ToolCall{{ID: "save", Name: "save_plan", Arguments: json.RawMessage(`{"content":"# Cache fix\nImplement the cache fix and run verification."}`)}}}, nil
				case "plan_saved":
					return Response{Content: "Plan ready.", StopReason: "stop"}, nil
				case "execute":
					phase = "answered"
					return Response{StopReason: "tool_calls", ToolCalls: []ToolCall{
						{ID: "progress", Name: "update_progress", Arguments: json.RawMessage(`{"plan":[{"step":"Implement cache fix","status":"in_progress"},{"step":"Run verification","status":"pending"}]}`)},
						inputCall(t, "question", inputQuestion("cache")),
						{ID: "bulk", Name: "inspect", Arguments: json.RawMessage(`{}`)},
					}}, nil
				case "answered":
					before = requestCollaboration(t, request)
					if before.Plan == nil || before.Plan.Content != planContent || before.Approval == nil || before.Progress == nil {
						t.Fatalf("plan, approval, or progress missing before compaction: %+v", before)
					}
					data, err := json.Marshal(request.Messages)
					if err != nil || !strings.Contains(string(data), choice) {
						t.Fatalf("answer missing from the next model request: %v", err)
					}
					phase = "finish"
					return Response{StopReason: "tool_calls", ToolCalls: []ToolCall{{ID: "latest", Name: "inspect", Arguments: json.RawMessage(`{}`)}}}, nil
				case "finish":
					if !reflect.DeepEqual(requestCollaboration(t, request), before) {
						t.Fatal("collaboration state changed during overflow recovery")
					}
					if scenario == "automatic_summary" && overflows < 2 {
						overflows++
						return Response{}, errors.New("maximum context length is 16000 tokens; requested 20000 tokens")
					}
					return Response{Content: "Modification done; verification still pending.", StopReason: "stop"}, nil
				case "later":
					return Response{Content: "Waiting for verification.", StopReason: "stop"}, nil
				default:
					t.Fatalf("unexpected phase %q", phase)
					return Response{}, errors.New("unexpected phase")
				}
			}}
			inspections := 0
			config := Config{Provider: provider, Model: "test", Mode: ModePlan, PlansDir: plans, KeepRecentTokens: 1,
				Tools: []Tool{NewSavePlanTool(), NewUpdateProgressTool(), NewRequestUserInputTool(), NewSearchHistoryTool(), NewReadHistoryTool(), {
					Name: "inspect", ReadOnly: true, Schema: json.RawMessage(`{"type":"object"}`),
					Execute: func(context.Context, json.RawMessage) (string, error) {
						inspections++
						if inspections == 1 {
							return strings.Repeat("Inspection detail. ", 1500), nil
						}
						return "Latest inspection complete.", nil
					},
				}}, UserInputHandler: func(_ context.Context, request UserInputRequest) (UserInputResponse, error) {
					answers++
					if request.CallIndex != 1 || request.CallCount != 1 {
						t.Fatalf("unexpected question batch: %+v", request)
					}
					return UserInputResponse{Answers: map[string]UserInputAnswer{"cache": {Source: "custom", Value: choice}}}, nil
				}}
			agent, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			session, err := NewSession(t.TempDir(), SessionInfo{Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if _, err := session.Run(t.Context(), agent, "Plan: "+goal, nil); err != nil {
				t.Fatal(err)
			}
			if agent.Collaboration().Approval != nil {
				t.Fatal("saving the plan unexpectedly approved it")
			}
			if _, err := session.ApprovePlan(agent, nil); err != nil {
				t.Fatal(err)
			}
			phase = "execute"
			starts, ends := 0, 0
			var stages []string
			result, err := session.Run(t.Context(), agent, goal, func(event Event) {
				switch event.Type {
				case EventRunStart:
					starts++
				case EventRunEnd:
					ends++
				case EventContextCompacted:
					stages = append(stages, event.Compaction.Stage)
				}
			})
			if err != nil || result.Turns != 3 || starts != 1 || ends != 1 || answers != 1 {
				t.Fatalf("err=%v turns=%d starts=%d ends=%d answers=%d", err, result.Turns, starts, ends, answers)
			}
			if scenario == "automatic_summary" {
				if !reflect.DeepEqual(stages, []string{"tool_results", "summary"}) || summaries == 0 {
					t.Fatalf("automatic recovery stages=%v summaries=%d", stages, summaries)
				}
			} else {
				// Move the answered task outside the protected recent user turn.
				phase = "later"
				if _, err := session.Run(t.Context(), agent, "Keep the existing task state.", nil); err != nil {
					t.Fatal(err)
				}
				focus, wantStage := "", "tool_results"
				if scenario == "manual_summary" {
					focus, wantStage = "Preserve task decisions.", "summary"
				}
				state, err := session.Compact(t.Context(), agent, focus, nil)
				if err != nil || state.Stage != wantStage {
					t.Fatalf("manual compaction stage=%s err=%v", state.Stage, err)
				}
			}
			if !reflect.DeepEqual(agent.Collaboration(), before) {
				t.Fatal("compaction changed plan, approval, progress IDs, versions, or step status")
			}
			request, err := agent.currentRequest(t.Context())
			if err != nil || !reflect.DeepEqual(requestCollaboration(t, request), before) {
				t.Fatalf("authoritative state missing from post-compaction request: %v", err)
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			choiceVisible := strings.Contains(string(encoded), choice)
			if choiceVisible != (scenario == "manual_prune") {
				t.Fatalf("unexpected choice visibility: %t", choiceVisible)
			}
			if !strings.Contains(string(encoded), goal) {
				t.Fatal("task goal in the saved plan disappeared")
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			// The complete plan must also be recoverable when its file is missing.
			if err := os.Remove(before.Plan.Path); err != nil {
				t.Fatal(err)
			}
			restored, err := OpenSession(session.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			freshProvider := &fakeProvider{responses: []Response{{Content: "Verification remains pending.", StopReason: "stop"}}}
			config.Provider = freshProvider
			fresh, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := restored.Restore(fresh); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fresh.Collaboration(), before) || !reflect.DeepEqual(fresh.Messages(), agent.Messages()) {
				t.Fatal("reopen lost collaboration state or restored pre-compaction messages")
			}
			data, err := os.ReadFile(before.Plan.Path)
			if err != nil || string(data) != planContent {
				t.Fatalf("saved plan file not recovered: %v", err)
			}
			history, err := fresh.executeHistoryTool(t.Context(), ToolCall{Name: "search_history", Arguments: json.RawMessage(`{"query":"Use the user's isolated cache directory."}`)})
			if err != nil || !strings.Contains(history, choice) {
				t.Fatalf("original answer not recoverable: %v", err)
			}
			var page historyPage
			if err := json.Unmarshal([]byte(history), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Records) != 1 || page.Records[0].ToolName != "request_user_input" || page.Records[0].Source.RunID == "" {
				t.Fatalf("original answer lost source identity: %+v", page)
			}
			if _, err := restored.Run(t.Context(), fresh, "continue", nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(requestCollaboration(t, freshProvider.requests[0]), before) {
				t.Fatal("restored request omitted plan, approval, or unfinished progress")
			}
			t.Logf("plan, approval, progress and stable IDs survive; choice visible=%t, original answer recoverable=true", choiceVisible)
		})
	}
}

// Characterize current termination semantics. A model stop is not a verified
// task outcome, and recoverable tool failures do not automatically fail a run.
func TestRunTerminationDoesNotCompleteUnfinishedProgress(t *testing.T) {
	for _, scenario := range []string{"model_stop", "cancelled", "tool_error", "max_turns", "model_error"} {
		t.Run(scenario, func(t *testing.T) {
			toolFailed := errors.New("verification failed")
			provider := &compactionProvider{respond: func(_ context.Context, _ Request, call int) (Response, error) {
				if scenario == "model_error" {
					return Response{}, errors.New("provider unavailable")
				}
				if scenario == "cancelled" {
					return Response{}, context.Canceled
				}
				if call == 1 && (scenario == "tool_error" || scenario == "max_turns") {
					return Response{StopReason: "tool_calls", ToolCalls: []ToolCall{{ID: "verify", Name: "verify", Arguments: json.RawMessage(`{}`)}}}, nil
				}
				// No successful verification occurred; runtime currently accepts this claim.
				return Response{Content: "All changes verified successfully.", StopReason: "stop"}, nil
			}}
			limit := 3
			if scenario == "max_turns" {
				limit = 1
			}
			agent, err := New(Config{Provider: provider, Model: "test", MaxTurns: limit, Tools: []Tool{{
				Name: "verify", Schema: json.RawMessage(`{"type":"object"}`),
				Execute: func(context.Context, json.RawMessage) (string, error) { return "", toolFailed },
			}}})
			if err != nil {
				t.Fatal(err)
			}
			agent.collaboration.Progress = &PlanProgress{ID: "checklist", Version: 1, Plan: []PlanStep{
				{ID: "edit", Step: "Modify code", Status: "completed"},
				{ID: "verify", Step: "Run verification", Status: "pending"},
			}}
			before := agent.Collaboration()
			var end Event
			var toolErrors int
			result, runErr := agent.Run(t.Context(), "Finish and verify the change.", func(event Event) {
				if event.Type == EventRunEnd {
					end = event
				}
				if event.Type == EventToolEnd && event.IsError {
					toolErrors++
				}
			})
			if !reflect.DeepEqual(agent.Collaboration(), before) {
				t.Fatal("termination marked unfinished verification completed")
			}
			switch scenario {
			case "model_stop", "tool_error":
				if runErr != nil || result.StopReason != "stop" || end.Reason != "stop" || end.IsError {
					t.Fatalf("result=%+v err=%v end=%+v", result, runErr, end)
				}
				if scenario == "tool_error" && toolErrors != 1 {
					t.Fatal("verification tool failure was not recorded")
				}
				t.Log("Current limitation: final success claim accepted while verification remains pending.")
			case "cancelled":
				if !errors.Is(runErr, context.Canceled) || end.Reason != "aborted" || !end.IsError {
					t.Fatalf("err=%v end=%+v", runErr, end)
				}
			case "max_turns":
				if !errors.Is(runErr, ErrMaxTurns) || result.StopReason != "max_turns" || end.Reason != "error" || !end.IsError {
					t.Fatalf("result=%+v err=%v end=%+v", result, runErr, end)
				}
				t.Log("Current inconsistency: result=max_turns, run_end.reason=error.")
			case "model_error":
				if runErr == nil || end.Reason != "error" || !end.IsError {
					t.Fatalf("err=%v end=%+v", runErr, end)
				}
			}
		})
	}
}
