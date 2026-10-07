package main

import (
	"bytes"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
	"github.com/unimpl/Iota/provider/openaicompat"
)

func TestExecuteInDefaultExplainsRequiredModeWithoutChangingState(t *testing.T) {
	provider, err := openaicompat.New(openaicompat.Config{})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	handled, prompt, err := planningCommand("/execute", agent, nil, nil, &bytes.Buffer{})
	if !handled || prompt != "" || err == nil || !strings.Contains(err.Error(), "enter /plan first") {
		t.Fatalf("handled=%t prompt=%q err=%v", handled, prompt, err)
	}
	if agent.Mode() != iota.ModeDefault || len(agent.Messages()) != 0 {
		t.Fatal("rejected execution changed conversation state")
	}
}
