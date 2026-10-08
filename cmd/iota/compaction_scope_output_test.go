package main

import (
	"bytes"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestCompactionOutputDistinguishesScopeFromFullContext(t *testing.T) {
	var stdout, stderr bytes.Buffer
	printer := eventOutput{stdout: &stdout, stderr: &stderr}
	state := iota.CompactionState{BeforeChars: 6486, AfterChars: 7285,
		BeforeUsage: iota.ContextUsage{Tokens: 3243, Estimated: true}, AfterUsage: iota.ContextUsage{Tokens: 3815, Estimated: true}}
	printer.emit(iota.Event{Type: iota.EventCompactionStart, Reason: "manual", Compaction: &state})
	printer.emit(iota.Event{Type: iota.EventCompactionPrepared, CompactionInput: &iota.CompactionInput{
		Messages:  []iota.Message{{Role: iota.RoleUser, Content: "old greeting"}, {Role: iota.RoleAssistant, Content: "old reply"}},
		UserTurns: 1, KeptTurns: 3, KeptMessages: 6, Chars: 181, Usage: iota.ContextUsage{Tokens: 120, Estimated: true}}})
	printer.emit(iota.Event{Type: iota.EventCompactionEnd, IsError: true, Error: "did not reduce context", Compaction: &state})
	for _, text := range []string{"full request, not the selected old history", "selected old history: 1 user turns, 2 messages", "protected recent history: 3 user turns, 6 messages", "old greeting", "old reply", "candidate full context (not applied): approximately 3815 tokens"} {
		if !strings.Contains(stderr.String(), text) {
			t.Fatalf("missing %q: %s", text, stderr.String())
		}
	}
	if stdout.Len() != 0 {
		t.Fatal("diagnostics entered assistant output")
	}
}

func TestCompactionPreviewEscapesControlSequencesAndBoundsOutput(t *testing.T) {
	var stderr bytes.Buffer
	printer := eventOutput{stdout: &bytes.Buffer{}, stderr: &stderr}
	messages := make([]iota.Message, 12)
	for index := range messages {
		messages[index] = iota.Message{Role: iota.RoleTool, Content: "\x1b[31m" + strings.Repeat("large output", 100)}
	}
	printer.emit(iota.Event{Type: iota.EventCompactionPrepared, CompactionInput: &iota.CompactionInput{Messages: messages}})
	if strings.Contains(stderr.String(), "\x1b") || !strings.Contains(stderr.String(), "7 more messages") || stderr.Len() > 2000 {
		t.Fatalf("unsafe or unbounded preview: %q", stderr.String())
	}
}
