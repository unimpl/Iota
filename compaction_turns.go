package iota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func checkpointMessage(memory CompactionMemory) Message {
	copy := cloneCompactionMemory(memory)
	return Message{Role: RoleUser, ContextSummary: true, Content: renderCompactionMemory(copy), Sources: memorySources(copy), Checkpoint: &copy}
}

func (a *Agent) originalSummaryMessages(messages []Message) []Message {
	original := cloneMessages(messages)
	a.mu.Lock()
	session := a.history
	a.mu.Unlock()
	if session == nil {
		return original
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	bySeq := make(map[uint64]Message, len(session.history))
	for _, message := range session.history {
		if message.Source != nil {
			bySeq[message.Source.Seq] = message
		}
	}
	for i := range original {
		message := &original[i]
		if message.Role != RoleTool || message.Source == nil {
			continue
		}
		if full, ok := bySeq[message.Source.Seq]; ok {
			message.Content = full.Content
		}
	}
	return original
}

func emitCompactionProgress(emit EmitFunc, turn int, progress CompactionProgress) {
	emitEvent(emit, Event{Type: EventCompactionProgress, Turn: turn, CompactionProgress: &progress})
}

// Units label original turn sources; they do not imply per-turn summaries.
func turnSourceUnits(messages []Message) []CompactionUnit {
	var units []CompactionUnit
	for index, turn := range modelTurns(messages) {
		unit := CompactionUnit{ID: fmt.Sprintf("t%d", index), Messages: turn.Messages, MessageCount: len(turn.Messages), Sources: turn.Sources}
		if len(turn.Messages) > 0 && turn.Messages[0].Source != nil {
			unit.RunID, unit.StepID = turn.Messages[0].Source.RunID, turn.Messages[0].Source.StepID
		}
		units = append(units, unit)
	}
	return units
}

func (a *Agent) structuredSummaryRequest(inputs []turnInput, memory CompactionMemory, instructions string) (Request, error) {
	data, err := json.Marshal(inputs)
	if err != nil {
		return Request{}, err
	}
	entries := append([]SummaryEntry(nil), memory.Entries...)
	for i := range entries {
		entries[i].Sources = nil
	}
	prior, err := json.Marshal(entries)
	if err != nil {
		return Request{}, err
	}
	prompt := "<turns>\n" + string(data) + "\n</turns>\n<previous-summary>\n" + string(prior) + "\n</previous-summary>"
	if strings.TrimSpace(instructions) != "" {
		prompt += "\nAdditional focus:\n" + instructions
	}
	return Request{Mode: a.Mode(), Model: a.model, SystemPrompt: compactionPrompt, Messages: []Message{{Role: RoleUser, Content: prompt}}}, nil
}

func (a *Agent) summarizeTurns(ctx context.Context, messages []Message, memory CompactionMemory, instructions string, turn int, emit EmitFunc) (CompactionMemory, error) {
	turns := modelTurns(cloneMessages(messages))
	// Prefer a single joint request, including all raw turns and the old checkpoint.
	batches := [][]modelTurn{turns}
	processed := 0
	for position, retries := 0, 0; position < len(batches); {
		if err := ctx.Err(); err != nil {
			return CompactionMemory{}, err
		}
		batch := batches[position]
		inputs := make([]turnInput, len(batch))
		for i, item := range batch {
			inputs[i] = turnInput{ID: fmt.Sprintf("t%d", processed+i), Messages: cloneMessages(item.Messages), Sources: append([]SourceRange(nil), item.Sources...), Incomplete: item.Incomplete}
			for j := range inputs[i].Messages {
				inputs[i].Messages[j].Sources = nil
				inputs[i].Messages[j].Checkpoint = nil
			}
		}
		request, err := a.structuredSummaryRequest(inputs, memory, instructions)
		if err != nil {
			return CompactionMemory{}, err
		}
		a.mu.Lock()
		capacity := a.contextLimitTokens
		a.mu.Unlock()
		// Leave room for output. The provider remains the authority for actual limits.
		exceeds := capacity > 0 && estimateContextUsage(request, capacity).Tokens+max(1024, capacity/10) > capacity
		var response Response
		if !exceeds {
			emitCompactionProgress(emit, turn, CompactionProgress{Stage: "summarize", ProcessedTurns: processed, TotalTurns: len(turns), Batch: position + 1, Batches: len(batches)})
			response, err = a.callModel(ctx, request, turn, true, emit)
		}
		limit, overflow := contextOverflow(err)
		if limit > 0 {
			a.mu.Lock()
			a.contextLimitTokens = limit
			a.mu.Unlock()
		}
		if (overflow || exceeds) && retries < 8 && len(batch) > 0 {
			stage := "split"
			if len(batch) > 1 {
				middle := len(batch) / 2
				replacement := [][]modelTurn{batch[:middle], batch[middle:]}
				batches = append(append(append([][]modelTurn{}, batches[:position]...), replacement...), batches[position+1:]...)
			} else {
				stage = "excerpt"
				batches[position][0].Messages = boundedSummaryMessages(batch[0].Messages, max(128, compactToolChars>>retries))
				batches[position][0].Incomplete = true
			}
			emitCompactionProgress(emit, turn, CompactionProgress{Stage: stage, ProcessedTurns: processed, TotalTurns: len(turns), Batch: position + 1, Batches: len(batches)})
			retries++
			continue
		}
		if exceeds {
			return CompactionMemory{}, fmt.Errorf("necessary summary state and turn input exceed reported capacity %d; history was preserved", capacity)
		}
		if err != nil {
			return CompactionMemory{}, fmt.Errorf("summarize turns: %w", err)
		}
		if response.StopReason != "stop" || len(response.ToolCalls) != 0 || strings.TrimSpace(response.Content) == "" {
			return CompactionMemory{}, errors.New("structured summary is empty, incomplete, or attempted a tool call")
		}
		emitCompactionProgress(emit, turn, CompactionProgress{Stage: "validate", ProcessedTurns: processed, TotalTurns: len(turns), Batch: position + 1, Batches: len(batches)})
		candidate, err := bindStructuredSummary(response.Content, inputs, memory)
		if err != nil {
			return CompactionMemory{}, err
		}
		memory = candidate
		processed += len(batch)
		emitEvent(emit, Event{Type: EventCompactionDelta, Turn: turn, Reason: "summary_complete", Text: response.Content, Usage: response.Usage})
		emitCompactionProgress(emit, turn, CompactionProgress{Stage: "merged", ProcessedTurns: processed, TotalTurns: len(turns), Batch: position + 1, Batches: len(batches), Entries: len(memory.Entries)})
		position++
		retries = 0
	}
	return memory, nil
}
