package main

import (
	"io"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/internal/app"
)

// planningCommand changes collaboration state; only an explicit execution command approves a plan.
func planningCommand(line string, agent *iota.Agent, session *iota.Session, emit iota.EmitFunc, stderr io.Writer) (bool, string, error) {
	handled, prompt, output, err := app.PlanningCommand(line, agent, session, emit)
	if output != "" {
		printLine(stderr, colorThinking, output)
	}
	return handled, prompt, err
}
