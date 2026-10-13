package server

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestWebApprovalRefusesEditedPlan(t *testing.T) {
	var calls atomic.Int32
	provider := testProvider{stream: func(ctx context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
		calls.Add(1)
		var last iota.Message
		for _, message := range request.Messages {
			if !message.RuntimeContext {
				last = message
			}
		}
		if request.Mode == iota.ModePlan && last.Role == iota.RoleUser {
			return iota.Response{ToolCalls: []iota.ToolCall{{ID: "save", Name: "save_plan", Arguments: json.RawMessage(`{"content":"# Original plan\nSteps."}`)}}}, nil
		}
		return iota.Response{Content: "ready"}, nil
	}}
	s, httpServer := testServer(t, provider)
	name := createSession(t, httpServer, s.config.Settings.CWD)
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "plan", Action: "run", Prompt: "/plan create plan"}, 202)
	state := waitState(t, httpServer, name, "completed")
	if state.PlanHash == "" || state.Collaboration.Plan == nil {
		t.Fatal("plan preview missing")
	}
	previousCalls := calls.Load()
	if err := os.WriteFile(state.Collaboration.Plan.Path, []byte("# Changed plan\nNew steps."), 0o600); err != nil {
		t.Fatal(err)
	}
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "stale", Action: "execute", PlanHash: state.PlanHash}, 202)
	latest := waitState(t, httpServer, name, "failed")
	if calls.Load() != previousCalls || latest.Collaboration.Mode != iota.ModePlan || latest.PlanHash == state.PlanHash {
		t.Fatal("stale approval executed or preview did not refresh")
	}
	request(t, httpServer, "POST", "sessions/"+name+"/commands", commandRequest{ID: "reviewed", Action: "execute", PlanHash: latest.PlanHash}, 202)
	finished := waitState(t, httpServer, name, "completed")
	if finished.Collaboration.Mode != iota.ModeDefault || calls.Load() != previousCalls+1 {
		t.Fatal("reviewed plan did not execute")
	}
}
