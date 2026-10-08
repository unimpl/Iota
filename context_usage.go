package iota

import (
	"encoding/json"
	"math"
)

// ContextUsage describes input size. Estimates include system instructions and tools;
// an unknown capacity has no remaining percentage, rather than assuming a model limit.
type ContextUsage struct {
	Tokens           int      `json:"tokens"`
	Estimated        bool     `json:"estimated"`
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
}

func estimateContextUsage(request Request, limit int) ContextUsage {
	data, _ := json.Marshal(request)
	// Approximate ASCII at four characters per token and non-ASCII at one.
	// This is display information, never a trigger or proof that a request fits.
	units := 0
	for _, character := range string(data) {
		if character <= 127 {
			units++
		} else {
			units += 4
		}
	}
	usage := ContextUsage{Tokens: (units + 3) / 4, Estimated: true}
	return withContextLimit(usage, limit)
}

func withContextLimit(usage ContextUsage, limit int) ContextUsage {
	usage.RemainingPercent = nil
	if limit > 0 {
		remaining := math.Max(0, math.Min(100, 100*(1-float64(usage.Tokens)/float64(limit))))
		usage.RemainingPercent = &remaining
	}
	return usage
}
