package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	iota "github.com/unimpl/Iota"
)

type searchInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	Regex      bool   `json:"regex"`
	IgnoreCase bool   `json:"ignore_case"`
	Hidden     bool   `json:"hidden"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
}

type searchMatch struct {
	Path          string `json:"path"`
	Line          int    `json:"line"`
	Text          string `json:"text"`
	TextTruncated bool   `json:"text_truncated,omitempty"`
}

type searchPage struct {
	Matches    []searchMatch `json:"matches,omitempty"`
	Files      []string      `json:"files,omitempty"`
	Truncated  bool          `json:"truncated"`
	NextOffset int           `json:"next_offset,omitempty"`
	Message    string        `json:"message,omitempty"`
}

// NewSearch searches content through ripgrep without granting shell execution.
func NewSearch(cwd string, config RipgrepConfig) iota.Tool {
	return newRipgrepTool(cwd, config, false)
}

// NewFindFiles filters the file paths discovered by ripgrep with a glob.
func NewFindFiles(cwd string, config RipgrepConfig) iota.Tool {
	return newRipgrepTool(cwd, config, true)
}

func newRipgrepTool(cwd string, config RipgrepConfig, files bool) iota.Tool {
	name := "search"
	description := "Search file contents with ripgrep. pattern is literal unless regex=true. Returns JSON matches with path, 1-based line and text. Respects ignore rules; hidden files require hidden=true. glob filters file paths. offset is a 1-based result index; limit defaults to 100. If truncated, continue with next_offset and identical parameters. Long matching lines are clipped; use read for full text."
	properties := `"pattern":{"type":"string","minLength":1},"regex":{"type":"boolean"},"ignore_case":{"type":"boolean"},"glob":{"type":"string","minLength":1},`
	if files {
		name = "find_files"
		description = "Find file paths discovered by ripgrep using a glob pattern, e.g. *.go or **/config*.go. Supports *, ?, character classes, and ** path segments. Slashless patterns match basenames; patterns with / match the returned path. Returns JSON file paths. Respects ignore rules; hidden files require hidden=true. offset is a 1-based result index; limit defaults to 100. If truncated, continue with next_offset and identical parameters."
		properties = `"pattern":{"type":"string","minLength":1},`
	}
	schema := `{"type":"object","properties":{` + properties + `"path":{"type":"string","minLength":1},"hidden":{"type":"boolean"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1,"maximum":2000}},"required":["pattern"],"additionalProperties":false}`
	return iota.Tool{
		Name: name, ReadOnly: true, Description: description, Schema: json.RawMessage(schema),
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var input searchInput
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&input); err != nil {
				return "", err
			}
			if input.Pattern == "" {
				return "", errors.New("pattern must not be empty")
			}
			if files && (input.Regex || input.IgnoreCase || input.Glob != "") {
				return "", errors.New("find_files accepts a glob pattern, not content search options")
			}
			if input.Path == "" {
				input.Path = "."
			}
			if input.Offset == 0 {
				input.Offset = 1
			}
			if input.Limit == 0 {
				input.Limit = 100
			}
			if input.Offset < 1 || input.Limit < 1 || input.Limit > DefaultMaxLines {
				return "", errors.New("offset must be positive and limit must be between 1 and 2000")
			}
			glob := input.Glob
			if files {
				glob = input.Pattern
			}
			if _, err := compileSearchGlob(glob); err != nil {
				return "", err
			}
			binary, err := config.resolve(ctx)
			if err != nil {
				return "", err
			}
			return runRipgrep(ctx, binary, cwd, input, files)
		},
	}
}

func runRipgrep(ctx context.Context, binary, cwd string, input searchInput, files bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	args := []string{"--no-config", "--color", "never", "--sort", "path", "--no-require-git", "--glob", "!**/.git/**"}
	if input.Hidden {
		args = append(args, "--hidden")
	}
	if files {
		args = append(args, "--files", "--null")
	} else {
		args = append(args, "--json", "--line-number")
		if !input.Regex {
			args = append(args, "--fixed-strings")
		}
		if input.IgnoreCase {
			args = append(args, "--ignore-case")
		}
		args = append(args, "-e", input.Pattern)
	}
	args = append(args, "--", input.Path)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = time.Second
	stderr := &searchErrorBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start ripgrep: %w", err)
	}
	page, readErr := readSearchPage(stdout, input, files)
	limited := page.Truncated && readErr == nil && ctx.Err() == nil
	if readErr != nil || limited {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return "", fmt.Errorf("read ripgrep results: %w", readErr)
	}
	// Truncation deliberately stops rg after one extra result. Other errors,
	// including errors reported before truncation, remain failures.
	if stderr.text.Len() > 0 {
		return "", fmt.Errorf("ripgrep: %s", strings.TrimSpace(stderr.text.String()))
	}
	if !limited {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if waitErr != nil {
			var exit *exec.ExitError
			if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
				return "", fmt.Errorf("ripgrep failed: %w", waitErr)
			}
		}
	}
	if len(page.Files) == 0 && len(page.Matches) == 0 {
		page.Message = "No results at this offset."
	}
	encoded, err := json.Marshal(page)
	return string(encoded), err
}

// Bound diagnostics while still draining stderr so rg cannot block on its pipe.
type searchErrorBuffer struct{ text strings.Builder }

func (buffer *searchErrorBuffer) Write(data []byte) (int, error) {
	if remaining := DefaultMaxBytes - buffer.text.Len(); remaining > 0 {
		buffer.text.Write(data[:min(len(data), remaining)])
	}
	return len(data), nil
}

func readSearchPage(input io.Reader, query searchInput, files bool) (searchPage, error) {
	page := searchPage{}
	glob := query.Glob
	if files {
		glob = query.Pattern
	}
	matchPath, err := compileSearchGlob(glob)
	if err != nil {
		return page, err
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	if files {
		scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if index := bytes.IndexByte(data, 0); index >= 0 {
				return index + 1, data[:index], nil
			}
			if atEOF && len(data) > 0 {
				return 0, nil, errors.New("incomplete ripgrep file record")
			}
			return 0, nil, nil
		})
	}
	seen, count, outputBytes := 0, 0, 128
	for scanner.Scan() {
		var match searchMatch
		if files {
			match.Path = scanner.Text()
			if !utf8.ValidString(match.Path) {
				return page, errors.New("file path is not valid UTF-8")
			}
		} else {
			var event struct {
				Type string `json:"type"`
				Data struct {
					Path struct {
						Text *string `json:"text"`
					} `json:"path"`
					Lines struct {
						Text *string `json:"text"`
					} `json:"lines"`
					Line int `json:"line_number"`
				} `json:"data"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return page, err
			}
			if event.Type != "match" {
				continue
			}
			if event.Data.Path.Text == nil || event.Data.Lines.Text == nil {
				return page, errors.New("matching path or text is not valid UTF-8")
			}
			match = searchMatch{Path: *event.Data.Path.Text, Line: event.Data.Line, Text: strings.TrimSuffix(strings.TrimSuffix(*event.Data.Lines.Text, "\n"), "\r")}
			if len(match.Text) > 4096 {
				match.Text = match.Text[:4096]
				for !utf8.ValidString(match.Text) {
					match.Text = match.Text[:len(match.Text)-1]
				}
				match.TextTruncated = true
			}
		}
		if !matchPath(match.Path) {
			continue
		}
		seen++
		if seen < query.Offset {
			continue
		}
		var encoded []byte
		if files {
			encoded, _ = json.Marshal(match.Path)
		} else {
			encoded, _ = json.Marshal(match)
		}
		if count >= query.Limit || outputBytes+len(encoded)+1 > DefaultMaxBytes {
			page.Truncated = true
			page.NextOffset = query.Offset + count
			if count == 0 {
				return page, errors.New("one result exceeds the output limit")
			}
			break
		}
		if files {
			page.Files = append(page.Files, match.Path)
		} else {
			page.Matches = append(page.Matches, match)
		}
		count++
		outputBytes += len(encoded) + 1
	}
	return page, scanner.Err()
}

// Filter the paths rg discovers, rather than passing positive --glob flags:
// positive rg globs override ignore rules and even include hidden files.
// A slashless pattern matches basenames; ** matches zero or more path segments.
func compileSearchGlob(pattern string) (func(string) bool, error) {
	if pattern == "" {
		return func(string) bool { return true }, nil
	}
	pattern = strings.TrimPrefix(pattern, "./")
	if _, err := path.Match(pattern, ""); err != nil {
		return nil, fmt.Errorf("invalid file glob: %w", err)
	}
	if !strings.Contains(pattern, "/") {
		return func(name string) bool {
			matched, _ := path.Match(pattern, path.Base(filepath.ToSlash(name)))
			return matched
		}, nil
	}
	parts := strings.Split(pattern, "/")
	return func(name string) bool {
		segments := strings.Split(strings.TrimPrefix(filepath.ToSlash(name), "./"), "/")
		matched := make([]bool, len(segments)+1)
		matched[0] = true
		for _, part := range parts {
			if part == "**" {
				for index := 1; index <= len(segments); index++ {
					matched[index] = matched[index] || matched[index-1]
				}
				continue
			}
			for index := len(segments); index > 0; index-- {
				segmentMatches, _ := path.Match(part, segments[index-1])
				matched[index] = matched[index-1] && segmentMatches
			}
			matched[0] = false
		}
		return matched[len(segments)]
	}, nil
}
