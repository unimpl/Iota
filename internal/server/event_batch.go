package server

import (
	"encoding/json"
	"sort"
	"strings"
)

type eventBatch struct {
	Records []Record `json:"records"`
	Cursor  uint64   `json:"cursor"`
}

type deltaKey struct {
	kind string
	run  string
	turn int
}

type mergedDelta struct {
	record Record
	text   strings.Builder
}

type eventBuffer struct {
	cursor  uint64
	records []Record
	deltas  map[deltaKey]*mergedDelta
}

func immediateEvent(kind string) bool {
	switch kind {
	case "session_start", "session_end", "run_start", "turn_start", "message_added", "session_reset", "context_compacted", "tool_start", "tool_end", "run_end", "mode_changed", "plan_saved", "plan_approved", "progress_updated", "client_request", "client_result":
		return true
	default:
		return false
	}
}

// Only independent text/reasoning deltas may commute. Every semantic event
// flushes the window before another turn, tool or operation can contribute.
func (b *eventBuffer) add(record Record) (bool, error) {
	b.cursor = record.Sequence
	if record.Type == "text_delta" || record.Type == "reasoning_delta" {
		var payload struct {
			Turn      int    `json:"turn"`
			Text      string `json:"text"`
			Reasoning string `json:"reasoning"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return false, err
		}
		key := deltaKey{kind: record.Type, run: record.RunID, turn: payload.Turn}
		if b.deltas == nil {
			b.deltas = make(map[deltaKey]*mergedDelta)
		}
		delta := b.deltas[key]
		if delta == nil {
			delta = &mergedDelta{}
			b.deltas[key] = delta
		}
		delta.record = record
		delta.record.Payload = nil // Never retain raw provider data.
		if record.Type == "text_delta" {
			delta.text.WriteString(payload.Text)
		} else {
			delta.text.WriteString(payload.Reasoning)
		}
		return false, nil
	}
	if !immediateEvent(record.Type) {
		return false, nil // Advance the cursor without transmitting trace records.
	}
	record = chatRecord(record)
	if record.Type == "message_added" && record.Payload == nil {
		return false, nil // Injected context is not a visible message.
	}
	b.records = append(b.records, record)
	return true, nil
}

func (b *eventBuffer) take() eventBatch {
	records := b.records
	for key, delta := range b.deltas {
		payload := struct {
			Turn      int    `json:"turn,omitempty"`
			Text      string `json:"text,omitempty"`
			Reasoning string `json:"reasoning,omitempty"`
		}{Turn: key.turn}
		if key.kind == "text_delta" {
			payload.Text = delta.text.String()
		} else {
			payload.Reasoning = delta.text.String()
		}
		delta.record.Payload, _ = json.Marshal(payload)
		records = append(records, delta.record)
	}
	// Each merged record uses its last source sequence, so the existing reducer
	// can deduplicate normally. The batch cursor also covers omitted records.
	sort.Slice(records, func(i, j int) bool { return records[i].Sequence < records[j].Sequence })
	if records == nil {
		records = []Record{}
	}
	b.records, b.deltas = nil, nil
	return eventBatch{Records: records, Cursor: b.cursor}
}
