package iota

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestPlanApprovalBindsReviewedVersion(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(fmt.Sprint(persisted), func(t *testing.T) {
			provider := &fakeProvider{responses: []Response{{ToolCalls: []ToolCall{{ID: "save", Name: "save_plan", Arguments: json.RawMessage(`{"content":"# Reviewed plan\nOriginal steps."}`)}}}, {Content: "ready"}}}
			agent, err := New(Config{Provider: provider, Model: "test", Mode: ModePlan, PlansDir: t.TempDir(), DisableSkills: true, Tools: []Tool{NewSavePlanTool()}})
			if err != nil {
				t.Fatal(err)
			}
			var session *Session
			if persisted {
				session, err = NewSession(t.TempDir(), SessionInfo{Model: "test"})
				if err != nil {
					t.Fatal(err)
				}
				defer session.Close()
			}
			if _, err := session.Run(context.Background(), agent, "make a plan", nil); err != nil {
				t.Fatal(err)
			}
			preview, err := agent.PlanPreview()
			if err != nil {
				t.Fatal(err)
			}
			reviewedHash := fmt.Sprintf("%x", sha256.Sum256([]byte(preview.Content)))
			changed := "# Revised plan\nDifferent steps."
			if err := os.WriteFile(preview.Path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := session.ApprovePlanVersion(agent, reviewedHash, nil); !errors.Is(err, ErrPlanChanged) {
				t.Fatalf("approval=%v", err)
			}
			if agent.Mode() != ModePlan || agent.Collaboration().Approval != nil {
				t.Fatal("stale approval changed state")
			}
			latest, err := agent.PlanPreview()
			if err != nil || latest.Content != changed {
				t.Fatalf("preview=%+v err=%v", latest, err)
			}
			latestHash := fmt.Sprintf("%x", sha256.Sum256([]byte(latest.Content)))
			approved, err := session.ApprovePlanVersion(agent, latestHash, nil)
			if err != nil {
				t.Fatal(err)
			}
			if approved.Content != changed || agent.Mode() != ModeDefault || agent.Collaboration().Approval.ContentHash != latestHash {
				t.Fatal("approval did not use reviewed content")
			}
		})
	}
}
