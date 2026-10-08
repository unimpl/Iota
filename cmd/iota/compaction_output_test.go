package main

import (
	"bytes"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestCompactionOutputIncludesBeforeAfterAndRemaining(t *testing.T) {
	for _, trigger := range []string{"manual", "overflow"} {
		t.Run(trigger, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			printer := eventOutput{stdout: &stdout, stderr: &stderr}
			remaining := 75.0
			state := iota.CompactionState{Stage: "summary", BeforeChars: 10000, AfterChars: 1000, KeptTurns: 3, ContextLimitTokens: 8000, SummaryPath: "/sessions/example.summary.42.md",
				BeforeUsage: iota.ContextUsage{Tokens: 9000, Estimated: true}, AfterUsage: iota.ContextUsage{Tokens: 2000, Estimated: true, RemainingPercent: &remaining}}
			printer.emit(iota.Event{Type: iota.EventCompactionStart, Reason: trigger, Compaction: &state})
			printer.emit(iota.Event{Type: iota.EventCompactionEnd, Reason: trigger, Compaction: &state})
			for _, part := range []string{"current approximately 9000 tokens", "context before:", "context after: approximately 2000 tokens", "75.0% available", "percentage unknown", "example.summary.42.md"} {
				if !strings.Contains(stderr.String(), part) {
					t.Fatalf("missing %q: %s", part, stderr.String())
				}
			}
			if stdout.Len() != 0 {
				t.Fatalf("status entered assistant output: %q", stdout.String())
			}
		})
	}
}

func TestUnknownContextSizeIsNotPrintedAsZeroTokens(t *testing.T) {
	label := formatContextUsage(iota.ContextUsage{}, 0)
	if !strings.Contains(label, "unavailable") || strings.Contains(label, "0 tokens") {
		t.Fatalf("label=%q", label)
	}
}
