package iota

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPlanningReloadsTemplateAndRecordsAppliedInstructions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iota", "plans", "template.md")
	provider := &fakeProvider{responses: []Response{{Content: "Choose an option or enter your own answer."}, {Content: "Updated plan."}, {Content: "Executing."}}}
	agent, err := New(Config{Provider: provider, Model: "test", Mode: ModePlan, PlanTemplatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Run(t.Context(), agent, "plan", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != defaultPlanTemplate {
		t.Fatalf("template=%q err=%v", data, err)
	}
	first := provider.requests[0].SystemPrompt
	for _, instruction := range []string{"阶段一", "阶段二", "阶段三", "自定义输入", "没有回复不是选择或批准", "save_plan"} {
		if !strings.Contains(first, instruction) {
			t.Fatalf("missing %q in planning instructions", instruction)
		}
	}
	custom := "# Project planning policy\nOffer options and accept custom answers. Preserve the user's existing storage choice."
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), agent, "Use my custom storage choice", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provider.requests[1].SystemPrompt, custom) || strings.Contains(provider.requests[1].SystemPrompt, "阶段一") {
		t.Fatal("template edit did not take effect")
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != custom {
		t.Fatal("custom template was overwritten")
	}
	if err := session.SetMode(agent, ModeDefault, nil); err != nil {
		t.Fatal(err)
	}
	// Default mode does not read or create the planning template.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), agent, "execute", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("default mode initialized a template: %v", err)
	}
	if strings.Contains(provider.requests[2].SystemPrompt, custom) {
		t.Fatal("plan instructions leaked into default mode")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		var record sessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Type != string(EventModelRequest) {
			continue
		}
		var event Event
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, event.Request.SystemPrompt)
	}
	if len(recorded) != 3 || recorded[0] != first || recorded[1] != provider.requests[1].SystemPrompt {
		t.Fatal("session did not record the applied template")
	}
}

func TestInvalidPlanningTemplateStopsBeforeProvider(t *testing.T) {
	for _, content := range [][]byte{nil, []byte(" \n"), {0xff}} {
		path := filepath.Join(t.TempDir(), "template.md")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		provider := &fakeProvider{responses: []Response{{Content: "should not run"}}}
		agent, err := New(Config{Provider: provider, Model: "test", Mode: ModePlan, PlanTemplatePath: path})
		if err != nil {
			t.Fatal(err)
		}
		var end Event
		if _, err := agent.Run(t.Context(), "plan", func(event Event) {
			if event.Type == EventRunEnd {
				end = event
			}
		}); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("invalid template error=%v", err)
		}
		if len(provider.requests) != 0 || !end.IsError {
			t.Fatal("provider called with invalid instructions")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != string(content) {
			t.Fatal("invalid template was silently replaced")
		}
	}
}

func TestConcurrentTemplateInitializationPublishesCompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans", "template.md")
	var wait sync.WaitGroup
	results := make(chan string, 12)
	errors := make(chan error, 12)
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			text, err := loadPlanTemplate(context.Background(), path)
			results <- text
			errors <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for text := range results {
		if text != defaultPlanTemplate {
			t.Fatal("partial template observed")
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "template.md" {
		t.Fatalf("temporary files remain: %v %v", entries, err)
	}
}
