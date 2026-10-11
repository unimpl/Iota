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
				if last.RunID == source.RunID && last.StepID == source.StepID && last.Description == source.Description && source.StartSeq >= last.StartSeq {
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

type modelTurn struct {
	Start, End int
	Messages   []Message
	Sources    []SourceRange
}

// A model turn contains one assistant response and its complete tool batch.
// User messages immediately before that response belong to the same turn.
func modelTurns(messages []Message) []modelTurn {
	var turns []modelTurn
	for i := 0; i < len(messages); {
		if messages[i].ContextSummary {
			i++
			continue
		}
		start := i
		for i < len(messages) && messages[i].Role == RoleUser && !messages[i].ContextSummary {
			i++
		}
		if i < len(messages) && messages[i].Role == RoleAssistant {
			i++
			i = min(len(messages), i+len(messages[i-1].ToolCalls))
		}
		if i == start {
			i++
		}
		part := messages[start:i]
		turns = append(turns, modelTurn{Start: start, End: i, Messages: part, Sources: sourceRanges(part)})
	}
	return turns
}

// The protection target is rounded up to whole turns. Overflow never changes it.
func compactionBoundary(messages []Message, keepTokens int) (cut, kept, tokens int) {
	turns := modelTurns(messages)
	if len(turns) == 0 {
		return 0, 0, 0
	}
	cut = turns[len(turns)-1].Start
	for i := len(turns) - 1; i >= 0; i-- {
		part := turns[i]
		data, _ := json.Marshal(modelVisibleRequest(Request{Messages: part.Messages}).Messages)
		tokens += estimateTextTokens(string(data))
		kept++
		cut = part.Start
		if tokens >= keepTokens {
			break
		}
	}
	return cut, kept, tokens
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
		if source.Description != "" {
			description, _ := json.Marshal(source.Description)
			fmt.Fprintf(&text, " description=%s", description)
		}
		text.WriteByte('\n')
	}
	text.WriteString("</source-index>")
	return text.String()
}

// Bound only the displayed index. Checkpoint metadata retains all descriptions
// and original addresses for inheritance across later compactions.
func checkpointSources(ranges []SourceRange) []SourceRange {
	const maxRanges = 32
	if len(ranges) <= maxRanges {
		return append([]SourceRange(nil), ranges...)
	}
	older := ranges[:len(ranges)-maxRanges+1]
	combined := SourceRange{StartSeq: older[0].StartSeq, EndSeq: older[0].EndSeq,
		Description: "Older history grouped for context size; use search_history with a topic, tool name, or file path to locate specific original records."}
	for _, source := range older[1:] {
		combined.StartSeq = min(combined.StartSeq, source.StartSeq)
		combined.EndSeq = max(combined.EndSeq, source.EndSeq)
	}
	return append([]SourceRange{combined}, ranges[len(older):]...)
}
