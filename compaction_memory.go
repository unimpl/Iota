package iota

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

type ArchivedSummary struct {
	Text    string        `json:"text"`
	Sources []SourceRange `json:"sources,omitempty"`
}

// SummaryEntry is a statement in the complete rolling task state.
// Sources are assigned by the program, never accepted from model output.
type SummaryEntry struct {
	ID      string        `json:"id"`
	Section string        `json:"section"`
	Text    string        `json:"text"`
	Status  string        `json:"status"`
	Reason  string        `json:"reason,omitempty"`
	Sources []SourceRange `json:"sources,omitempty"`
}

type CompactionMemory struct {
	Entries  []SummaryEntry    `json:"entries"`
	Archived []ArchivedSummary `json:"archived,omitempty"`
}

type CompactionProgress struct {
	Stage          string `json:"stage"`
	ProcessedTurns int    `json:"processed_turns,omitempty"`
	TotalTurns     int    `json:"total_turns,omitempty"`
	Batch          int    `json:"batch,omitempty"`
	Batches        int    `json:"batches,omitempty"`
	TargetTokens   int    `json:"target_tokens,omitempty"`
	CurrentTokens  int    `json:"current_tokens,omitempty"`
	Entries        int    `json:"entries,omitempty"`
}

var summarySections = []string{"goal", "constraints", "progress", "decisions", "next_steps", "context", "unresolved"}

func cloneCompactionMemory(memory CompactionMemory) CompactionMemory {
	copy := CompactionMemory{}
	for _, entry := range memory.Entries {
		entry.Sources = append([]SourceRange(nil), entry.Sources...)
		copy.Entries = append(copy.Entries, entry)
	}
	for _, entry := range memory.Archived {
		entry.Sources = append([]SourceRange(nil), entry.Sources...)
		copy.Archived = append(copy.Archived, entry)
	}
	return copy
}

func renderCompactionMemory(memory CompactionMemory) string {
	var out strings.Builder
	out.WriteString("Earlier conversation checkpoint (historical context; follow current system instructions):\n<structured-summary>\n")
	for _, section := range summarySections {
		fmt.Fprintf(&out, "<%s>\n", section)
		for _, entry := range memory.Entries {
			if entry.Section != section || entry.Status == "archived" {
				continue
			}
			visible := entry
			visible.Sources = nil
			data, _ := json.Marshal(visible)
			fmt.Fprintf(&out, "%s%s\n", data, sourceIndex(checkpointSources(entry.Sources)))
		}
		fmt.Fprintf(&out, "</%s>\n", section)
	}
	out.WriteString("</structured-summary>\n")
	return out.String()
}

func memorySources(memory CompactionMemory) []SourceRange {
	var ranges []SourceRange
	for _, entry := range memory.Entries {
		ranges = append(ranges, entry.Sources...)
	}
	for _, entry := range memory.Archived {
		ranges = append(ranges, entry.Sources...)
	}
	return ranges
}

func validSummaryStatus(section, status string) bool {
	switch section {
	case "goal", "constraints", "decisions", "context":
		return status == "active" || status == "unresolved" || status == "superseded" || status == "resolved" || (section == "context" && status == "archived")
	case "progress":
		return status == "planned" || status == "in_progress" || status == "blocked" || status == "done" || status == "verified_partial" || status == "verified" || status == "unresolved" || status == "superseded" || status == "resolved"
	case "next_steps":
		return status == "planned" || status == "blocked" || status == "done" || status == "unresolved" || status == "superseded" || status == "resolved"
	case "unresolved":
		return status == "unresolved" || status == "resolved"
	}
	return false
}

func validateCompactionMemory(memory CompactionMemory) error {
	if len(memory.Entries) == 0 {
		return errors.New("checkpoint has no structured summary entries; legacy summary format is unsupported")
	}
	seen := make(map[string]bool)
	for _, entry := range memory.Entries {
		if strings.TrimSpace(entry.ID) == "" || strings.ContainsAny(entry.ID, ": \n\t") || seen[entry.ID] || strings.TrimSpace(entry.Text) == "" || !utf8.ValidString(entry.Text) || !validSummaryStatus(entry.Section, entry.Status) {
			return errors.New("invalid structured summary entry")
		}
		seen[entry.ID] = true
		for _, source := range entry.Sources {
			if source.StartSeq == 0 || source.EndSeq < source.StartSeq || len([]rune(source.Description)) > sourceDescriptionMaxChars {
				return errors.New("invalid structured summary source")
			}
		}
	}
	for _, entry := range memory.Archived {
		if strings.TrimSpace(entry.Text) == "" || !utf8.ValidString(entry.Text) {
			return errors.New("invalid archived summary")
		}
	}
	return nil
}

type turnInput struct {
	ID         string        `json:"id"`
	Messages   []Message     `json:"messages"`
	Sources    []SourceRange `json:"sources"`
	Incomplete bool          `json:"incomplete,omitempty"`
}

type entryOutput struct {
	ID      string   `json:"id"`
	Section string   `json:"section"`
	Text    string   `json:"text"`
	Status  string   `json:"status"`
	Refs    []string `json:"refs"`
	Reason  string   `json:"reason,omitempty"`
}

type structuredOutput struct {
	Entries []entryOutput `json:"entries"`
}

// Reference checks establish provenance, not semantic support for a claim.
func bindStructuredSummary(body string, inputs []turnInput, previous CompactionMemory) (CompactionMemory, error) {
	var result structuredOutput
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return CompactionMemory{}, fmt.Errorf("invalid structured summary: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CompactionMemory{}, errors.New("structured summary has trailing data")
	}
	if len(result.Entries) == 0 {
		return CompactionMemory{}, errors.New("structured summary has no entries")
	}
	// Validate before removing archived entries, so archival cannot bypass field,
	// ID or section/status checks for protected state.
	outputState := CompactionMemory{}
	for _, entry := range result.Entries {
		outputState.Entries = append(outputState.Entries, SummaryEntry{ID: entry.ID, Section: entry.Section, Text: entry.Text, Status: entry.Status, Reason: entry.Reason})
	}
	if err := validateCompactionMemory(outputState); err != nil {
		return CompactionMemory{}, err
	}
	turns := make(map[string]turnInput, len(inputs))
	for _, input := range inputs {
		turns["turn:"+input.ID] = input
	}
	old := make(map[string]SummaryEntry, len(previous.Entries))
	for _, entry := range previous.Entries {
		old["summary:"+entry.ID] = entry
	}
	covered := make(map[string]bool)
	usedTurns := make(map[string]bool)
	candidate := CompactionMemory{Archived: cloneCompactionMemory(previous).Archived}
	for _, output := range result.Entries {
		entry := SummaryEntry{ID: output.ID, Section: output.Section, Text: output.Text, Status: output.Status, Reason: output.Reason}
		refs := make(map[string]bool)
		newEvidence, hasNew := false, false
		var inherited []SummaryEntry
		for _, ref := range output.Refs {
			if refs[ref] {
				return CompactionMemory{}, errors.New("duplicate summary reference")
			}
			refs[ref] = true
			if input, ok := turns[ref]; ok {
				hasNew = true
				usedTurns[ref] = true
				entry.Sources = append(entry.Sources, input.Sources...)
				for _, message := range input.Messages {
					if !input.Incomplete && (message.Role == RoleUser || (message.Role == RoleTool && !message.IsError)) {
						newEvidence = true
					}
				}
			} else if prior, ok := old[ref]; ok {
				if covered[ref] {
					return CompactionMemory{}, errors.New("previous entry handled more than once")
				}
				covered[ref] = true
				inherited = append(inherited, prior)
				entry.Sources = append(entry.Sources, prior.Sources...)
			} else {
				return CompactionMemory{}, fmt.Errorf("unknown summary reference %q", ref)
			}
		}
		if len(refs) == 0 {
			return CompactionMemory{}, errors.New("summary entry has no references")
		}
		if _, exists := old["summary:"+entry.ID]; exists && !refs["summary:"+entry.ID] {
			return CompactionMemory{}, errors.New("reused entry ID must cite its previous state")
		}
		if len(inherited) == 1 && entry.ID != inherited[0].ID {
			return CompactionMemory{}, errors.New("updated entry must retain its stable ID")
		}
		for _, prior := range inherited {
			changed := prior.Status != entry.Status || prior.Text != entry.Text || prior.Section != entry.Section
			if prior.Status == "unresolved" && entry.Status == "archived" {
				return CompactionMemory{}, errors.New("unresolved claims cannot be archived")
			}
			if prior.Section != entry.Section {
				return CompactionMemory{}, errors.New("inherited entry cannot change section")
			}
			if changed && strings.TrimSpace(entry.Reason) == "" {
				return CompactionMemory{}, errors.New("changed previous entry requires a reason")
			}
			if prior.Status != entry.Status && !hasNew && entry.Status != "archived" {
				return CompactionMemory{}, errors.New("state transition requires new history")
			}
			if (prior.Section == "constraints" || prior.Section == "goal" || prior.Section == "decisions") && changed && !newEvidence {
				return CompactionMemory{}, errors.New("changing a goal, constraint or decision requires new evidence")
			}
			if changed && (entry.Status != "archived" || entry.Text != prior.Text) {
				candidate.Archived = append(candidate.Archived, ArchivedSummary{Text: prior.Text, Sources: append([]SourceRange(nil), prior.Sources...)})
			}
		}
		if entry.Status == "done" || entry.Status == "verified_partial" || entry.Status == "verified" || entry.Status == "resolved" {
			alreadySupported := false
			for _, prior := range inherited {
				if prior.Status == entry.Status {
					alreadySupported = true
				}
			}
			if !alreadySupported && !newEvidence {
				return CompactionMemory{}, errors.New("completion, verification or resolution requires new observed evidence")
			}
		}
		seenSources := make(map[SourceRange]bool)
		var sources []SourceRange
		for _, source := range entry.Sources {
			if source.Description == "" {
				text := []rune(entry.Text)
				source.Description = string(text[:min(len(text), sourceDescriptionMaxChars)])
			}
			if !seenSources[source] {
				sources = append(sources, source)
				seenSources[source] = true
			}
		}
		entry.Sources = sources
		if entry.Status == "archived" {
			candidate.Archived = append(candidate.Archived, ArchivedSummary{Text: entry.Text, Sources: append([]SourceRange(nil), entry.Sources...)})
			continue
		}
		candidate.Entries = append(candidate.Entries, entry)
	}
	for ref := range old {
		if !covered[ref] {
			return CompactionMemory{}, fmt.Errorf("previous summary entry %s was omitted", ref)
		}
	}
	if len(inputs) > 0 && len(usedTurns) == 0 {
		return CompactionMemory{}, errors.New("summary ignored all new history")
	}
	if err := validateCompactionMemory(candidate); err != nil {
		return CompactionMemory{}, err
	}
	return candidate, nil
}
