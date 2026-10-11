package iota

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const sourceDescriptionMaxChars = 200

type summarySource struct {
	ID     string      `json:"id"`
	Source SourceRange `json:"source"`
}

type summaryDescription struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type summaryResult struct {
	Summary string               `json:"summary"`
	Sources []summaryDescription `json:"sources"`
}

type summaryCheckpoint struct {
	Summary string
	Sources []SourceRange
}

// Existing descriptions are authoritative lookup metadata, not summary prose.
// Only previously undescribed ranges are presented for new descriptions.
func undescribedSources(sources []SourceRange) []summarySource {
	var targets []summarySource
	for _, source := range sources {
		if source.Description == "" {
			targets = append(targets, summarySource{ID: fmt.Sprintf("s%d", len(targets)), Source: source})
		}
	}
	return targets
}

func bindSummarySources(text string, sources []SourceRange, targets []summarySource) (summaryCheckpoint, error) {
	checkpoint := summaryCheckpoint{Summary: text, Sources: append([]SourceRange(nil), sources...)}
	if len(targets) != 0 {
		var result summaryResult
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result); err != nil {
			return summaryCheckpoint{}, fmt.Errorf("invalid indexed compaction summary: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return summaryCheckpoint{}, errors.New("indexed compaction summary has trailing data")
		}
		if len(result.Sources) != len(targets) {
			return summaryCheckpoint{}, errors.New("compaction summary must describe every requested source ID exactly once")
		}
		descriptions := make(map[string]string, len(targets))
		for _, entry := range result.Sources {
			if entry.ID == "" || descriptions[entry.ID] != "" || strings.TrimSpace(entry.Description) == "" || !utf8.ValidString(entry.Description) || len([]rune(entry.Description)) > sourceDescriptionMaxChars {
				return summaryCheckpoint{}, errors.New("invalid or duplicate compaction source description")
			}
			descriptions[entry.ID] = entry.Description
		}
		position := 0
		for i := range checkpoint.Sources {
			if checkpoint.Sources[i].Description != "" {
				continue
			}
			description, ok := descriptions[targets[position].ID]
			if !ok {
				return summaryCheckpoint{}, errors.New("compaction summary returned an unknown or missing source ID")
			}
			checkpoint.Sources[i].Description = description
			position++
		}
		checkpoint.Summary = result.Summary
	}
	if strings.TrimSpace(checkpoint.Summary) == "" || !utf8.ValidString(checkpoint.Summary) || len([]rune(checkpoint.Summary)) > summaryMaxChars {
		return summaryCheckpoint{}, errors.New("compaction summary is empty or exceeds its output budget; history was preserved")
	}
	return checkpoint, nil
}
