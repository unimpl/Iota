package iota

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Unit boundaries are preferences; budgets and complete tool batches take precedence.
const summaryInputTokens = 12000
const summaryMaxChars = 8000

type CompactionUnit struct {
	RunID        string        `json:"run_id,omitempty"`
	StepID       string        `json:"step_id,omitempty"`
	Sources      []SourceRange `json:"sources,omitempty"`
	MessageCount int           `json:"message_count"`
	Messages     []Message     `json:"-"`
}

// messageBlocks never separates an assistant call batch from any of its results.
func messageBlocks(messages []Message) [][]Message {
	var blocks [][]Message
	for i := 0; i < len(messages); {
		end := i + 1
		if messages[i].Role == RoleAssistant {
			end += len(messages[i].ToolCalls)
		}
		end = min(end, len(messages))
		blocks = append(blocks, cloneMessages(messages[i:end]))
		i = end
	}
	return blocks
}

func sourceRanges(messages []Message) []SourceRange {
	var ranges []SourceRange
	for _, message := range messages {
		incoming := append([]SourceRange(nil), message.Sources...)
		if message.Source != nil {
			source := message.Source
			incoming = append(incoming, SourceRange{StartSeq: source.Seq, EndSeq: source.Seq, RunID: source.RunID, StepID: source.StepID})
		}
		for _, source := range incoming {
			if len(ranges) > 0 {
				last := &ranges[len(ranges)-1]
				// Gaps contain non-message events; history reads filter those out.
				if last.RunID == source.RunID && last.StepID == source.StepID && source.StartSeq >= last.StartSeq {
					last.EndSeq = max(last.EndSeq, source.EndSeq)
					continue
				}
			}
			ranges = append(ranges, source)
		}
	}
	return ranges
}

func compactionUnits(messages []Message, budget int) []CompactionUnit {
	var units []CompactionUnit
	for _, block := range messageBlocks(messages) {
		runID, stepID := "", ""
		if block[0].Source != nil {
			runID, stepID = block[0].Source.RunID, block[0].Source.StepID
		}
		merge := len(units) > 0 && block[0].Role != RoleUser
		if merge {
			previous := &units[len(units)-1]
			data, _ := json.Marshal(append(cloneMessages(previous.Messages), block...))
			merge = previous.RunID == runID && previous.StepID == stepID && estimateTextTokens(string(data)) <= budget
		}
		if merge {
			previous := &units[len(units)-1]
			previous.Messages = append(previous.Messages, block...)
			previous.MessageCount = len(previous.Messages)
			previous.Sources = sourceRanges(previous.Messages)
		} else {
			units = append(units, CompactionUnit{RunID: runID, StepID: stepID, Messages: block, MessageCount: len(block), Sources: sourceRanges(block)})
		}
	}
	return units
}

// compactionBoundary preserves recent runs when possible. On overflow it can
// checkpoint an older prefix of the protected run, retaining its newest batch.
func compactionBoundary(messages []Message, keep int, overflow bool) (int, int) {
	cut, kept := recentTurnBoundary(messages, keep)
	data, _ := json.Marshal(messages)
	if !overflow && (cut > 0 || estimateTextTokens(string(data)) <= summaryInputTokens) {
		return cut, kept
	}
	blocks := messageBlocks(messages)
	if len(blocks) < 2 {
		return cut, kept
	}
	latest, _ := json.Marshal(blocks[len(blocks)-1])
	if overflow && estimateTextTokens(string(latest)) > summaryInputTokens {
		// A single complete batch can be too large to retain. Summarize it as
		// a whole; original records remain accessible through source indices.
		return len(messages), 0
	}
	// Prefer half of the history by estimated size. Round to a complete batch.
	budget := estimateTextTokens(string(data)) / 2
	consumed, candidate := 0, 0
	for _, block := range blocks[:len(blocks)-1] {
		data, _ := json.Marshal(block)
		consumed += estimateTextTokens(string(data))
		candidate += len(block)
		if consumed >= budget {
			break
		}
	}
	// Prefer a run/step transition near the budget boundary when one exists.
	boundary := 0
	for i, block := range blocks[:len(blocks)-1] {
		boundary += len(block)
		current, next := block[0].Source, blocks[i+1][0].Source
		if boundary >= candidate && current != nil && next != nil && (current.RunID != next.RunID || current.StepID != next.StepID) {
			candidate = boundary
			break
		}
	}
	if candidate > cut {
		cut = candidate
		kept = 0
		for _, message := range messages[cut:] {
			if message.Role == RoleUser && !message.ContextSummary {
				kept++
			}
		}
	}
	return cut, kept
}

func boundedSummaryMessages(messages []Message, chars int) []Message {
	bounded := cloneMessages(messages)
	for i := range bounded {
		message := &bounded[i]
		text := []rune(message.Content)
		if len(text) > chars {
			message.Content = string(text[:chars/2]) + "\n[Middle omitted; inspect original source seq with read_history]\n" + string(text[len(text)-chars/2:])
		}
		for j := range message.ToolCalls {
			call := &message.ToolCalls[j]
			if len([]rune(string(call.Arguments))) > chars {
				// Keep valid JSON and a source pointer rather than slicing arguments.
				data, _ := json.Marshal(struct {
					Excerpt string `json:"excerpt"`
					Notice  string `json:"notice"`
				}{string([]rune(string(call.Arguments))[:chars]), "Arguments excerpt only; retrieve original source seq"})
				call.Arguments = data
			}
		}
	}
	return bounded
}

func sourceIndex(ranges []SourceRange) string {
	if len(ranges) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString("\n<source-index>\n")
	for _, source := range ranges {
		fmt.Fprintf(&text, "seq %d-%d", source.StartSeq, source.EndSeq)
		if source.RunID != "" {
			fmt.Fprintf(&text, " run=%s", source.RunID)
		}
		if source.StepID != "" {
			fmt.Fprintf(&text, " step=%s", source.StepID)
		}
		text.WriteByte('\n')
	}
	text.WriteString("</source-index>")
	return text.String()
}

// Older sources stay readable as one broad interval; recent units keep precise
// run/step pointers. Full unit indices remain in the JSONL checkpoint.
func checkpointSources(ranges []SourceRange) []SourceRange {
	const maxRanges = 32
	if len(ranges) <= maxRanges {
		return append([]SourceRange(nil), ranges...)
	}
	older := ranges[:len(ranges)-maxRanges+1]
	combined := SourceRange{StartSeq: older[0].StartSeq, EndSeq: older[0].EndSeq}
	for _, source := range older[1:] {
		combined.StartSeq = min(combined.StartSeq, source.StartSeq)
		combined.EndSeq = max(combined.EndSeq, source.EndSeq)
	}
	return append([]SourceRange{combined}, ranges[len(older):]...)
}
