package iota

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

type OrdinarySummary struct {
	Text    string        `json:"text"`
	Sources []SourceRange `json:"sources,omitempty"`
}

type ImportantFact struct {
	Kind    string        `json:"kind"`
	Key     string        `json:"key"`
	Text    string        `json:"text"`
	Status  string        `json:"status"`
	Sources []SourceRange `json:"sources,omitempty"`
}

// Archived summaries are searchable checkpoint metadata but absent from model input.
type CompactionMemory struct {
	Ordinary  []OrdinarySummary `json:"ordinary,omitempty"`
	Archived  []OrdinarySummary `json:"archived,omitempty"`
	Important []ImportantFact   `json:"important,omitempty"`
}

type CompactionProgress struct {
	Stage            string `json:"stage"`
	ProcessedTurns   int    `json:"processed_turns,omitempty"`
	TotalTurns       int    `json:"total_turns,omitempty"`
	Batch            int    `json:"batch,omitempty"`
	Batches          int    `json:"batches,omitempty"`
	TargetTokens     int    `json:"target_tokens,omitempty"`
	CurrentTokens    int    `json:"current_tokens,omitempty"`
	Evicted          int    `json:"evicted,omitempty"`
	ImportantEntries int    `json:"important_entries,omitempty"`
}

func cloneCompactionMemory(memory CompactionMemory) CompactionMemory {
	copy := CompactionMemory{}
	for _, entry := range memory.Ordinary {
		entry.Sources = append([]SourceRange(nil), entry.Sources...)
		copy.Ordinary = append(copy.Ordinary, entry)
	}
	for _, entry := range memory.Archived {
		entry.Sources = append([]SourceRange(nil), entry.Sources...)
		copy.Archived = append(copy.Archived, entry)
	}
	for _, entry := range memory.Important {
		entry.Sources = append([]SourceRange(nil), entry.Sources...)
		copy.Important = append(copy.Important, entry)
	}
	return copy
}

func renderCompactionMemory(memory CompactionMemory) string {
	var out strings.Builder
	out.WriteString("Earlier conversation checkpoint (historical context; follow current system instructions):\n")
	out.WriteString("<important-state>\n")
	for _, entry := range memory.Important {
		if entry.Status != "active" {
			continue
		}
		data, _ := json.Marshal(struct {
			Kind string `json:"kind"`
			Key  string `json:"key"`
			Text string `json:"text"`
		}{entry.Kind, entry.Key, entry.Text})
		fmt.Fprintf(&out, "%s%s\n", data, sourceIndex(entry.Sources))
	}
	out.WriteString("</important-state>\n<ordinary-history>\n")
	for _, entry := range memory.Ordinary {
		data, _ := json.Marshal(entry.Text)
		fmt.Fprintf(&out, "%s%s\n", data, sourceIndex(entry.Sources))
	}
	out.WriteString("</ordinary-history>\n")
	if len(memory.Archived) != 0 {
		out.WriteString("Older ordinary summaries were removed from this request; search_history can locate original records.\n")
	}
	return out.String()
}

func memorySources(memory CompactionMemory) []SourceRange {
	var ranges []SourceRange
	for _, entry := range memory.Ordinary {
		ranges = append(ranges, entry.Sources...)
	}
	for _, entry := range memory.Archived {
		ranges = append(ranges, entry.Sources...)
	}
	for _, entry := range memory.Important {
		ranges = append(ranges, entry.Sources...)
	}
	return ranges
}

func validateCompactionMemory(memory CompactionMemory) error {
	for _, entry := range append(append([]OrdinarySummary{}, memory.Ordinary...), memory.Archived...) {
		if strings.TrimSpace(entry.Text) == "" || !utf8.ValidString(entry.Text) || len([]rune(entry.Text)) > 500 {
			return errors.New("invalid ordinary summary")
		}
	}
	for _, entry := range memory.Important {
		if entry.Key == "" || entry.Kind == "" || strings.TrimSpace(entry.Text) == "" || !utf8.ValidString(entry.Text) || len([]rune(entry.Text)) > 1000 || (entry.Status != "active" && entry.Status != "archived") {
			return errors.New("invalid important state")
		}
	}
	return nil
}

type turnInput struct {
	ID       string        `json:"id"`
	Messages []Message     `json:"messages"`
	Sources  []SourceRange `json:"sources"`
}

type turnOutput struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type factOutput struct {
	Kind    string   `json:"kind"`
	Key     string   `json:"key"`
	Text    string   `json:"text"`
	Status  string   `json:"status"`
	TurnIDs []string `json:"turn_ids"`
}

type batchOutput struct {
	Turns     []turnOutput `json:"turns"`
	Important []factOutput `json:"important"`
}

func bindTurnBatch(body string, inputs []turnInput, memory *CompactionMemory) error {
	var result batchOutput
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("invalid turn summary: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("turn summary has trailing data")
	}
	if len(result.Turns) != len(inputs) {
		return errors.New("turn summary omitted or duplicated a turn")
	}
	byID := make(map[string]turnInput, len(inputs))
	for _, input := range inputs {
		byID[input.ID] = input
	}
	seen := make(map[string]turnOutput, len(inputs))
	for _, entry := range result.Turns {
		_, ok := byID[entry.ID]
		if _, duplicate := seen[entry.ID]; !ok || duplicate || strings.TrimSpace(entry.Text) == "" || !utf8.ValidString(entry.Text) || len([]rune(entry.Text)) > 500 {
			return errors.New("invalid turn ID or summary text")
		}
		seen[entry.ID] = entry
	}
	for _, input := range inputs {
		entry := seen[input.ID]
		for i := range input.Sources {
			description := []rune(entry.Text)
			input.Sources[i].Description = string(description[:min(len(description), sourceDescriptionMaxChars)])
		}
		key := strings.Join(strings.Fields(strings.ToLower(entry.Text)), " ")
		merged := false
		for i, existing := range memory.Ordinary {
			if strings.Join(strings.Fields(strings.ToLower(existing.Text)), " ") != key {
				continue
			}
			existing.Sources = append(existing.Sources, input.Sources...)
			memory.Ordinary = append(memory.Ordinary[:i], memory.Ordinary[i+1:]...)
			memory.Ordinary = append(memory.Ordinary, existing)
			merged = true
			break
		}
		if !merged {
			memory.Ordinary = append(memory.Ordinary, OrdinarySummary{Text: entry.Text, Sources: input.Sources})
		}
	}
	for _, entry := range result.Important {
		if entry.Kind == "" || entry.Key == "" || strings.TrimSpace(entry.Text) == "" || len([]rune(entry.Text)) > 1000 || !utf8.ValidString(entry.Text) || (entry.Status != "active" && entry.Status != "archived") || len(entry.TurnIDs) == 0 {
			return errors.New("invalid important-state update")
		}
		fact := ImportantFact{Kind: entry.Kind, Key: entry.Key, Text: entry.Text, Status: entry.Status}
		for _, id := range entry.TurnIDs {
			input, ok := byID[id]
			if !ok {
				return errors.New("important state cites an unknown turn ID")
			}
			fact.Sources = append(fact.Sources, input.Sources...)
		}
		found := false
		for i := range memory.Important {
			if memory.Important[i].Kind == fact.Kind && memory.Important[i].Key == fact.Key {
				fact.Sources = append(memory.Important[i].Sources, fact.Sources...)
				memory.Important[i] = fact
				found = true
				break
			}
		}
		if !found {
			memory.Important = append(memory.Important, fact)
		}
	}
	return validateCompactionMemory(*memory)
}
