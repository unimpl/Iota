package iota

import (
	"encoding/json"
	"errors"
	"fmt"
)

const runEntryPrompt = `[Run entry]
Interpret the user's latest message together with current progress, saved plan, execution authorization, and recent conversation.
For continuation requests such as "继续", "go on", or "continue", resume the relevant unfinished checklist, preserve its IDs, and verify interrupted operations before repeating them. If an authorized plan has no checklist, inspect earlier execution records before creating one. A saved plan alone is not execution authorization. If work is already complete or the continuation target is ambiguous, resolve that ambiguity before starting more work.
For a new request, handle simple work directly. For complex work with multiple meaningful steps, create a concise checklist with update_progress when available. Omit the checklist id only when starting a new checklist. Do not carry unrelated steps into new work.
For changes to existing work, revise the relevant checklist while preserving valid progress and user decisions.`

// runtimeMessage is a transient suffix. Replacing it leaves the preceding history
// unchanged, so the provider can reuse that long common prefix.
func (a *Agent) runtimeMessage() Message {
	state := a.Collaboration()
	data, _ := json.Marshal(state)
	text := "[Current runtime state; not a user request]\n<runtime-context>\n" + string(data) + "\n</runtime-context>"
	a.mu.Lock()
	turn := a.runTurn
	a.mu.Unlock()
	if turn == 1 && state.Mode == ModeDefault {
		text += "\n\n" + runEntryPrompt
	}
	return Message{Role: RoleUser, Content: text, RuntimeContext: true}
}

func prepareProgress(progress *PlanProgress, previous *PlanProgress) error {
	if err := validateProgress(*progress); err != nil {
		return err
	}
	existing := make(map[string]bool)
	if progress.ID != "" {
		if previous == nil || progress.ID != previous.ID {
			return errors.New("unknown progress ID; omit id to create a new checklist")
		}
		for _, step := range previous.Plan {
			existing[step.ID] = true
		}
		progress.Version = previous.Version + 1
	} else {
		id, err := newUUID()
		if err != nil {
			return err
		}
		progress.ID, progress.Version = id, 1
	}
	for i := range progress.Plan {
		step := &progress.Plan[i]
		if step.ID != "" {
			if !existing[step.ID] {
				return fmt.Errorf("unknown step ID %q; omit step_id for a new step", step.ID)
			}
			continue
		}
		id, err := newUUID()
		if err != nil {
			return err
		}
		step.ID = id
	}
	return validateProgress(*progress)
}

func activeStepID(state CollaborationState) string {
	if state.Mode != ModeDefault {
		return ""
	}
	if state.Progress != nil {
		for _, step := range state.Progress.Plan {
			if step.Status == "in_progress" {
				return step.ID
			}
		}
	}
	return ""
}
