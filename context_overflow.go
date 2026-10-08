package iota

import (
	"regexp"
	"strconv"
	"strings"
)

var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded|model_context_window_exceeded`),
	regexp.MustCompile(`(?i)prompt (?:is )?too long|prompt exceeds max length`),
	regexp.MustCompile(`(?i)exceeds (?:the )?(?:(?:model'?s |this model'?s )?maximum )?context (?:window|length)`),
	regexp.MustCompile(`(?i)maximum context length is [\d,]+ tokens`),
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum|exceeds the available context size`),
	regexp.MustCompile(`(?i)context window exceeds limit|exceeded model token limit|reduce the length of the messages`),
	regexp.MustCompile(`(?i)range of input length should be|input is too long for requested model`),
}

var contextLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length(?: is| of)?\s*\(?([\d,]+)`),
	regexp.MustCompile(`(?i)[\d,]+ tokens\s*>\s*([\d,]+)\s*maximum`),
	regexp.MustCompile(`(?i)maximum number of tokens allowed\s*\(?([\d,]+)`),
	regexp.MustCompile(`(?i)available context size\s*\(?([\d,]+)`),
	regexp.MustCompile(`(?i)range of input length should be\s*\[1,\s*([\d,]+)`),
}

// contextOverflow recognizes explicit capacity errors, never token rate limits.
// A missing numeric limit stays unknown; successful usage cannot establish it.
func contextOverflow(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	text := err.Error()
	lower := strings.ToLower(text)
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "too many requests") || strings.Contains(lower, "returned 429 ") || strings.HasPrefix(lower, "429 ") || strings.Contains(lower, "throttl") {
		return 0, false
	}
	matched := false
	for _, pattern := range overflowPatterns {
		if pattern.MatchString(text) {
			matched = true
			break
		}
	}
	if !matched {
		return 0, false
	}
	for _, pattern := range contextLimitPatterns {
		if match := pattern.FindStringSubmatch(text); len(match) > 1 {
			limit, err := strconv.Atoi(strings.ReplaceAll(match[1], ",", ""))
			if err == nil && limit > 0 {
				return limit, true
			}
		}
	}
	return 0, true
}
