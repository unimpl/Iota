package iota

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Mode selects the built-in collaboration workflow, independently of the model.
type Mode string

const (
	ModeDefault Mode = "default"
	ModePlan    Mode = "plan"
)

// SavedPlan contains the complete plan so the session can recover a missing file.
type SavedPlan struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type PlanStep struct {
	ID     string `json:"step_id,omitempty"`
	Step   string `json:"step"`
	Status string `json:"status"`
}

// PlanProgress is the execution checklist, separate from the Markdown design.
type PlanProgress struct {
	ID          string     `json:"id,omitempty"`
	Version     uint64     `json:"version,omitempty"`
	Explanation string     `json:"explanation,omitempty"`
	Plan        []PlanStep `json:"plan"`
}

// CollaborationState is recorded as a complete snapshot on each state change.
type CollaborationState struct {
	Mode     Mode          `json:"mode"`
	Plan     *SavedPlan    `json:"plan,omitempty"`
	Progress *PlanProgress `json:"progress,omitempty"`
	Approval *PlanApproval `json:"approval,omitempty"`
}

// PlanApproval binds authorization to the exact saved content, not just its path.
type PlanApproval struct {
	PlanID      string `json:"plan_id"`
	ContentHash string `json:"content_hash"`
}

func planContentHash(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

func validateMode(mode Mode) error {
	if mode != ModeDefault && mode != ModePlan {
		return fmt.Errorf("invalid collaboration mode %q; expected default or plan", mode)
	}
	return nil
}

func cloneCollaboration(state CollaborationState) CollaborationState {
	if state.Approval != nil {
		approval := *state.Approval
		state.Approval = &approval
	}
	if state.Plan != nil {
		plan := *state.Plan
		state.Plan = &plan
	}
	if state.Progress != nil {
		progress := *state.Progress
		progress.Plan = append([]PlanStep(nil), progress.Plan...)
		state.Progress = &progress
	}
	return state
}

// Collaboration returns a detached snapshot of the current mode and plan.
func (a *Agent) Collaboration() CollaborationState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneCollaboration(a.collaboration)
}

func (a *Agent) Mode() Mode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.collaboration.Mode
}

// SetMode changes an idle Agent. Use Session.SetMode when persistence is enabled.
func (a *Agent) SetMode(mode Mode, emit EmitFunc) error {
	return a.setMode(mode, emit, nil)
}

func (a *Agent) setMode(mode Mode, emit EmitFunc, record func(Event) error) error {
	if err := validateMode(mode); err != nil {
		return err
	}
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return ErrBusy
	}
	a.running = true
	state := cloneCollaboration(a.collaboration)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()
	if mode == ModePlan {
		if _, err := loadPlanTemplate(context.Background(), a.planTemplatePath); err != nil {
			return err
		}
	}
	state.Mode = mode
	return a.commitCollaboration(context.Background(), EventModeChanged, state, 0, emit, record)
}

func isCollaborationEvent(kind EventType) bool {
	return kind == EventModeChanged || kind == EventPlanSaved || kind == EventProgressUpdated || kind == EventPlanApproved
}

func validateCollaboration(state CollaborationState) error {
	if err := validateMode(state.Mode); err != nil {
		return err
	}
	if state.Plan != nil {
		plan := state.Plan
		if len(plan.ID) != 36 || strings.Trim(plan.ID, "0123456789abcdef-") != "" || !filepath.IsAbs(plan.Path) || filepath.Base(plan.Path) != plan.ID+".md" {
			return errors.New("invalid saved plan ID or path")
		}
		if err := validatePlanContent(plan.Content); err != nil {
			return err
		}
	}
	if state.Approval != nil && (state.Plan == nil || state.Approval.PlanID != state.Plan.ID || state.Approval.ContentHash != planContentHash(state.Plan.Content)) {
		return errors.New("approval does not match the saved plan version")
	}
	if state.Progress != nil {
		return validateProgress(*state.Progress)
	}
	return nil
}

func validateCollaborationEvent(event Event) error {
	if event.Collaboration == nil {
		return errors.New("collaboration state is missing")
	}
	state := *event.Collaboration
	if err := validateCollaboration(state); err != nil {
		return err
	}
	switch event.Type {
	case EventModeChanged:
	case EventPlanSaved:
		if state.Mode != ModePlan || state.Plan == nil {
			return errors.New("plan_saved requires a saved plan in plan mode")
		}
	case EventProgressUpdated:
		if state.Mode != ModeDefault || state.Progress == nil {
			return errors.New("progress_updated requires an execution checklist in default mode")
		}
		if state.Progress.ID == "" || state.Progress.Version == 0 {
			return errors.New("progress_updated requires a checklist ID and version")
		}
		for _, step := range state.Progress.Plan {
			if step.ID == "" {
				return errors.New("progress_updated requires stable step IDs")
			}
		}
	case EventPlanApproved:
		if state.Mode != ModeDefault || state.Plan == nil || state.Approval == nil {
			return errors.New("plan_approved requires a saved plan and version-bound approval in default mode")
		}
	default:
		return errors.New("unknown collaboration event")
	}
	return nil
}

// commitCollaboration writes durable state before exposing it to the Agent or observers.
func (a *Agent) commitCollaboration(ctx context.Context, kind EventType, state CollaborationState, turn int, emit EmitFunc, record func(Event) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCollaboration(state); err != nil {
		return err
	}
	state = cloneCollaboration(state)
	event := Event{Type: kind, Turn: turn, Collaboration: &state}
	if record != nil {
		if err := record(event); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.collaboration = cloneCollaboration(state)
	a.mu.Unlock()
	emitEvent(emit, event)
	return nil
}

func (a *Agent) toolAllowed(tool Tool) bool {
	mode := a.Mode()
	if tool.userInputTool {
		return a.userInputHandler != nil
	}
	if tool.historyTool {
		a.mu.Lock()
		available := a.history != nil
		a.mu.Unlock()
		return available && (mode == ModeDefault || tool.ReadOnly)
	}
	if tool.planTool {
		return (tool.Name == "save_plan" && mode == ModePlan) || (tool.Name == "update_progress" && mode == ModeDefault)
	}
	return mode == ModeDefault || tool.ReadOnly
}

func (a *Agent) activeToolDefinitions() []ToolDefinition {
	var definitions []ToolDefinition
	for _, compiled := range a.tools {
		tool := compiled.tool
		if a.toolAllowed(tool) {
			definitions = append(definitions, ToolDefinition{Name: tool.Name, Description: tool.Description, Schema: append(json.RawMessage(nil), tool.Schema...)})
		}
	}
	return definitions
}

func (a *Agent) collaborationPrompt(ctx context.Context) (string, error) {
	state := a.Collaboration()
	guidance := `[Collaboration mode: default]
You are currently in default mode. Earlier planning-only restrictions in conversation history are no longer in force. Follow the current mode and the tools declared in this request.
Respond to the user's latest message. A greeting or unrelated question does not request execution of a saved plan; do not repeatedly remind the user to approve it or automatically resume it.
Switching to default does not approve, cancel, or start a saved plan. A saved plan is a reference, not the current task. When the user explicitly requests implementation in default mode, carry out that request rather than imposing the previous planning-only workflow.
The CLI /execute command is available only in plan mode with a saved plan. In default mode, do not suggest /execute alone; if the user asks to use it, explain /plan followed by /execute. To revise a saved plan with save_plan, the user must first enter /plan.
Use update_progress, when available, to maintain an execution checklist with at most one in_progress step.`
	if state.Mode == ModePlan {
		template, err := loadPlanTemplate(ctx, a.planTemplatePath)
		if err != nil {
			return "", err
		}
		guidance = "[Collaboration mode: plan]\n" + template
	}
	guidance += `
The runtime-context block at the end of this request is the current state, not a new user request. Older checklist versions in history are historical evidence. In default mode, preserve program-assigned checklist and step IDs. Before working on a checklist step, mark it in_progress; verify completion before marking it completed. An in_progress step may have partially completed actions; inspect actual results before repeating an interrupted operation. Revise progress when new evidence requires it, and never mark unfinished work completed merely because a run ends.
When an omitted detail affects the next decision, use search_history and read_history, when available, to inspect original records. Prefer the supplied seq ranges; do not invent missing decisions, file contents, or tool results.`
	inputAvailable := false
	for _, tool := range a.tools {
		if tool.tool.userInputTool && a.toolAllowed(tool.tool) {
			inputAvailable = true
			break
		}
	}
	if inputAvailable {
		guidance += `
User input is available through request_user_input in both plan and default modes. Use it for decisions requiring an answer rather than ending the run with a question. Supply two or three distinct feasible options with concrete tradeoffs and one recommended option for every question. Ask independent questions together; ask dependent questions only after receiving the earlier answers. Empty input explicitly selects the recommendation; it is not a separate approval of other actions. Cancellation ends the current run. Do not invent answers or interpret a cancelled input as agreement.`
	} else {
		guidance += `
User input is unavailable in this run. Do not call request_user_input or ask questions awaiting a reply, even if earlier planning instructions suggest doing so. Proceed with available information; use reasonable defaults only when they preserve the user's intent, and state assumptions. If necessary information or authorization is missing, explain what is missing and why work cannot continue, then finish this run. Lack of an input channel is not acceptance of a recommendation or approval.`
	}
	base := a.systemPrompt
	if a.systemPromptLoader != nil {
		var err error
		base, err = a.systemPromptLoader(ctx)
		if err != nil {
			return "", err
		}
	}
	if base == "" {
		return guidance, nil
	}
	return base + "\n\n" + guidance, nil
}

// NewSavePlanTool declares the Agent-owned planning tool; its target path is never supplied by the model.
func NewSavePlanTool() Tool {
	return Tool{Name: "save_plan", planTool: true,
		Description: "Save or revise the complete Markdown plan in plan mode. Supply content only; the agent selects the plan file. This does not approve or execute the plan.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","minLength":1}},"required":["content"],"additionalProperties":false}`),
	}
}

// NewUpdateProgressTool declares the default-mode execution checklist tool.
func NewUpdateProgressTool() Tool {
	return Tool{Name: "update_progress", planTool: true,
		Description: "Create or replace the complete execution checklist in default mode. For an existing checklist, include its id and retain existing step_id values. Omit IDs only for a new checklist or new steps; the program assigns them. Each step has pending, in_progress, or completed status; at most one step may be in_progress. This does not edit the Markdown plan.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","minLength":1},"explanation":{"type":"string"},"plan":{"type":"array","minItems":1,"items":{"type":"object","properties":{"step_id":{"type":"string","minLength":1},"step":{"type":"string","minLength":1},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["step","status"],"additionalProperties":false}}},"required":["plan"],"additionalProperties":false}`),
	}
}

func validatePlanContent(content string) error {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	title := strings.TrimLeft(firstLine, "#")
	level := len(firstLine) - len(title)
	if !utf8.ValidString(content) || level < 1 || level > 6 || !strings.HasPrefix(title, " ") || strings.TrimSpace(title) == "" {
		return errors.New("plan must be UTF-8 Markdown starting with a heading")
	}
	return nil
}

func validateProgress(progress PlanProgress) error {
	if len(progress.Plan) == 0 {
		return errors.New("execution plan must contain at least one step")
	}
	active := 0
	ids := make(map[string]bool)
	for _, item := range progress.Plan {
		if item.ID != "" {
			if ids[item.ID] {
				return errors.New("duplicate progress step ID")
			}
			ids[item.ID] = true
		}
		if strings.TrimSpace(item.Step) == "" {
			return errors.New("plan step cannot be blank")
		}
		switch item.Status {
		case "pending", "completed":
		case "in_progress":
			active++
		default:
			return fmt.Errorf("invalid plan step status %q", item.Status)
		}
	}
	if active > 1 {
		return errors.New("at most one plan step may be in_progress")
	}
	return nil
}

func (a *Agent) executePlanTool(ctx context.Context, call ToolCall, turn int, emit EmitFunc, record func(Event) error) (string, error) {
	state := a.Collaboration()
	if call.Name == "update_progress" {
		var progress PlanProgress
		if err := json.Unmarshal(call.Arguments, &progress); err != nil {
			return "", err
		}
		if err := prepareProgress(&progress, state.Progress); err != nil {
			return "", err
		}
		state.Progress = &progress
		if err := a.commitCollaboration(ctx, EventProgressUpdated, state, turn, emit, record); err != nil {
			return "", err
		}
		return fmt.Sprintf("Execution progress %s saved (version %d). Latest state is supplied in runtime-context.", progress.ID, progress.Version), nil
	}
	var input struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return "", err
	}
	if err := validatePlanContent(input.Content); err != nil {
		return "", err
	}
	if state.Plan == nil {
		id, err := newUUID()
		if err != nil {
			return "", err
		}
		state.Plan = &SavedPlan{ID: id, Path: filepath.Join(a.plansDir, id+".md")}
	}
	if err := a.checkPlanPath(*state.Plan); err != nil {
		return "", err
	}
	path := state.Plan.Path
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	state.Plan.Content = input.Content
	state.Progress = nil
	state.Approval = nil
	if err := writePlanFile(ctx, path, input.Content); err != nil {
		return "", err
	}
	if err := a.commitCollaboration(ctx, EventPlanSaved, state, turn, emit, record); err != nil {
		var rollbackErr error
		if errors.Is(readErr, os.ErrNotExist) {
			rollbackErr = os.Remove(path)
		} else {
			rollbackErr = writePlanFile(context.Background(), path, string(previous))
		}
		return "", errors.Join(err, rollbackErr)
	}
	return "Plan saved to " + path + ". Planning remains active; wait for the user to approve execution.", nil
}

func (a *Agent) checkPlanPath(plan SavedPlan) error {
	if a.plansDir == "" || plan.Path != filepath.Join(a.plansDir, plan.ID+".md") {
		return errors.New("saved plan belongs to a different plans directory; restore with the original PlansDir")
	}
	return nil
}

// ApprovePlan reads the editable file and switches to default; it does not start a run.
func (a *Agent) ApprovePlan(emit EmitFunc) (SavedPlan, error) {
	return a.approvePlan(emit, nil)
}

func (a *Agent) approvePlan(emit EmitFunc, record func(Event) error) (SavedPlan, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return SavedPlan{}, ErrBusy
	}
	a.running = true
	state := cloneCollaboration(a.collaboration)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()
	if state.Mode != ModePlan {
		return SavedPlan{}, errors.New("plan approval requires plan mode; switch to plan mode first")
	}
	if state.Plan == nil {
		return SavedPlan{}, errors.New("no saved plan to approve in plan mode")
	}
	if err := a.checkPlanPath(*state.Plan); err != nil {
		return SavedPlan{}, err
	}
	data, err := os.ReadFile(state.Plan.Path)
	if err != nil {
		return SavedPlan{}, err
	}
	if err := validatePlanContent(string(data)); err != nil {
		return SavedPlan{}, err
	}
	if state.Plan.Content != string(data) {
		state.Progress = nil
	}
	state.Plan.Content = string(data)
	state.Approval = &PlanApproval{PlanID: state.Plan.ID, ContentHash: planContentHash(state.Plan.Content)}
	state.Mode = ModeDefault
	if err := a.commitCollaboration(context.Background(), EventPlanApproved, state, 0, emit, record); err != nil {
		return SavedPlan{}, err
	}
	return *state.Plan, nil
}
