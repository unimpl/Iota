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

func summaryTurnBatches(turns []modelTurn, budget int) [][]modelTurn {
	var batches [][]modelTurn
	currentTokens := 0
	for _, turn := range turns {
		data, _ := json.Marshal(turn.Messages)
		cost := estimateTextTokens(string(data))
		if len(batches) == 0 || currentTokens+cost > budget {
			batches = append(batches, []modelTurn{turn})
			currentTokens = cost
		} else {
			batches[len(batches)-1] = append(batches[len(batches)-1], turn)
			currentTokens += cost
		}
	}
	return batches
}

func turnBatchUnits(messages []Message) []CompactionUnit {
	var units []CompactionUnit
	for _, batch := range summaryTurnBatches(modelTurns(messages), summaryInputTokens) {
		unit := CompactionUnit{}
		for _, turn := range batch {
			unit.Messages = append(unit.Messages, turn.Messages...)
		}
		unit.MessageCount = len(unit.Messages)
		unit.Sources = sourceRanges(unit.Messages)
		if len(unit.Messages) > 0 && unit.Messages[0].Source != nil {
			unit.RunID, unit.StepID = unit.Messages[0].Source.RunID, unit.Messages[0].Source.StepID
		}
		units = append(units, unit)
	}
	return units
}

func (a *Agent) summarizeTurns(ctx context.Context, messages []Message, memory CompactionMemory, instructions string, turn int, emit EmitFunc) (CompactionMemory, error) {
	turns := modelTurns(messages)
	batches := summaryTurnBatches(turns, summaryInputTokens)
	processed := 0
	for position, retries := 0, 0; position < len(batches); {
		batch := batches[position]
		inputs := make([]turnInput, len(batch))
		for i, item := range batch {
			inputs[i] = turnInput{ID: fmt.Sprintf("t%d", i), Messages: cloneMessages(item.Messages), Sources: append([]SourceRange(nil), item.Sources...)}
			for j := range inputs[i].Messages {
				inputs[i].Messages[j].Sources = nil
				inputs[i].Messages[j].Checkpoint = nil
			}
		}
		data, err := json.Marshal(inputs)
		if err != nil {
			return CompactionMemory{}, err
		}
		prior, err := json.Marshal(memory.Important)
		if err != nil {
			return CompactionMemory{}, err
		}
		prompt := "<turns>\n" + string(data) + "\n</turns>\n<current-important-state>\n" + string(prior) + "\n</current-important-state>"
		if strings.TrimSpace(instructions) != "" {
			prompt += "\nAdditional focus:\n" + instructions
		}
		emitCompactionProgress(emit, turn, CompactionProgress{Stage: "summarize", ProcessedTurns: processed, TotalTurns: len(turns), Batch: position + 1, Batches: len(batches)})
		response, err := a.callModel(ctx, Request{Mode: a.Mode(), Model: a.model, SystemPrompt: compactionPrompt, Messages: []Message{{Role: RoleUser, Content: prompt}}}, turn, true, emit)
		limit, overflow := contextOverflow(err)
		if limit > 0 {
			a.mu.Lock()
			a.contextLimitTokens = limit
			a.mu.Unlock()
		}
		if overflow && retries < 8 {
			if len(batch) > 1 {
				middle := len(batch) / 2
				replacement := [][]modelTurn{batch[:middle], batch[middle:]}
				batches = append(append(append([][]modelTurn{}, batches[:position]...), replacement...), batches[position+1:]...)
			} else {
				batches[position][0].Messages = boundedSummaryMessages(batch[0].Messages, max(128, compactToolChars>>retries))
			}
			retries++
			continue
		}
		if err != nil {
			return CompactionMemory{}, fmt.Errorf("summarize turns: %w", err)
		}
		if response.StopReason != "stop" || len(response.ToolCalls) != 0 || strings.TrimSpace(response.Content) == "" {
			return CompactionMemory{}, errors.New("turn summary is empty, incomplete, or attempted a tool call")
		}
		candidate := cloneCompactionMemory(memory)
		if err := bindTurnBatch(response.Content, inputs, &candidate); err != nil {
			return CompactionMemory{}, err
		}
		memory = candidate
		processed += len(batch)
		emitEvent(emit, Event{Type: EventCompactionDelta, Turn: turn, Reason: "summary_complete", Text: response.Content, Usage: response.Usage})
		emitCompactionProgress(emit, turn, CompactionProgress{Stage: "normalize", ProcessedTurns: processed, TotalTurns: len(turns), Batch: position + 1, Batches: len(batches)})
		emitCompactionProgress(emit, turn, CompactionProgress{Stage: "important", ProcessedTurns: processed, TotalTurns: len(turns), ImportantEntries: len(memory.Important)})
		position++
		retries = 0
	}
	return memory, nil
}
