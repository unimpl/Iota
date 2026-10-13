package app

import (
	"errors"
	"fmt"
	"strings"

	iota "github.com/unimpl/Iota"
)

// PlanningCommand prepares the same explicit mode/approval actions in both UIs.
func PlanningCommand(line string, agent *iota.Agent, session *iota.Session, emit iota.EmitFunc) (handled bool, prompt string, output string, err error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false, "", "", nil
	}
	command := fields[0]
	argument := strings.TrimSpace(strings.TrimPrefix(line, command))
	switch command {
	case "/plan", "/default":
		mode := iota.ModePlan
		if command == "/default" || argument == "off" {
			mode = iota.ModeDefault
			if argument == "off" {
				argument = ""
			}
		}
		if err := session.SetMode(agent, mode, emit); err != nil {
			return true, "", "", err
		}
		return true, argument, "", nil
	case "/mode":
		if argument == "" {
			return true, "", "mode: " + string(agent.Mode()), nil
		}
		return true, "", "", session.SetMode(agent, iota.Mode(argument), emit)
	case "/execute":
		if argument != "" {
			return true, "", "", errors.New("/execute does not accept arguments; revise the plan first")
		}
		if agent.Mode() != iota.ModePlan {
			return true, "", "", errors.New("/execute requires plan mode; enter /plan first")
		}
		plan, err := session.ApprovePlan(agent, emit)
		if err != nil {
			return true, "", "", err
		}
		return true, ApprovedPlanPrompt(plan), "", nil
	default:
		return false, "", "", nil
	}
}

// ApprovedPlanPrompt submits exactly the content that the SDK recorded as approved.
func ApprovedPlanPrompt(plan iota.SavedPlan) string {
	return fmt.Sprintf("Implement the approved plan from %s. Complete the work and validation; maintain progress with update_progress when available.\n\n<approved-plan>\n%s\n</approved-plan>", plan.Path, plan.Content)
}
