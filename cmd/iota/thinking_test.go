package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestThinkingCommandValidatesAndRetainsPreference(t *testing.T) {
	lines := thinkingAll
	var output bytes.Buffer
	for _, command := range []struct {
		text string
		want int
	}{
		{"/thinking", thinkingAll},
		{"/thinking line 10", 10},
		{"/thinking line 0", 0},
		{"/thinking line all", thinkingAll},
	} {
		if handled, err := thinkingCommand(command.text, &lines, &output); !handled || err != nil || lines != command.want {
			t.Fatalf("command=%q handled=%v lines=%d err=%v", command.text, handled, lines, err)
		}
	}
	for _, invalid := range []string{"/thinking line", "/thinking rows 10", "/thinking line -1", "/thinking line nope", "/thinking line 10 extra", "/thinking line 999999999999999999999"} {
		if handled, err := thinkingCommand(invalid, &lines, &output); !handled || err == nil || lines != thinkingAll {
			t.Fatalf("invalid command changed preference: %q lines=%d err=%v", invalid, lines, err)
		}
	}
	if handled, err := thinkingCommand("/thinking-about", &lines, &output); handled || err != nil {
		t.Fatal("another command was consumed")
	}
}

func TestRecentThinkingRowsWrapsUnicodeAndBoundsLongLines(t *testing.T) {
	text, rows := recentThinkingRows("甲乙，a\u0301bc", 5, 2)
	if text != "甲乙，a\u0301bc" || !reflect.DeepEqual(rows, []string{"甲乙", "，a\u0301bc"}) {
		t.Fatalf("text=%q rows=%q", text, rows)
	}
	text, rows = recentThinkingRows("one\ntwo\nthree\n", 80, 2)
	if text != "two\nthree\n" || !reflect.DeepEqual(rows, []string{"two", "three"}) {
		t.Fatalf("trailing newline consumed a retained line: text=%q rows=%q", text, rows)
	}
	text, rows = recentThinkingRows(text+"four", 80, 2)
	if text != "three\nfour" || !reflect.DeepEqual(rows, []string{"three", "four"}) {
		t.Fatalf("next delta lost newline continuity: text=%q rows=%q", text, rows)
	}
	text, rows = recentThinkingRows(strings.Repeat("x", 10000), 40, 3)
	if len(text) != 120 || len(rows) != 3 {
		t.Fatalf("unbroken line was not bounded: bytes=%d rows=%d", len(text), len(rows))
	}
}

func TestThinkingRedirectedOutputAndZeroMode(t *testing.T) {
	for _, limit := range []int{0, 2, thinkingAll} {
		var stdout, stderr bytes.Buffer
		printer := eventOutput{stdout: &stdout, stderr: &stderr, thinking: thinkingOutput{lines: &limit}}
		for _, delta := range []string{"one\nt", "wo\nthree"} {
			printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: delta})
		}
		if limit != thinkingAll && stderr.String() != "[thinking]\n" {
			t.Fatal("redirected limited thinking was streamed instead of buffered")
		}
		printer.emit(iota.Event{Type: iota.EventModelResponse})
		want := "[thinking]\n[/thinking]\n"
		if limit == 2 {
			want = "[thinking]\ntwo\nthree\n[/thinking]\n"
		} else if limit == thinkingAll {
			want = "[thinking]\none\ntwo\nthree\n[/thinking]\n"
		}
		if stderr.String() != want || strings.Contains(stderr.String(), "\x1b") {
			t.Fatalf("limit=%d output=%q, want %q", limit, stderr.String(), want)
		}
	}
}

// This small screen interprets the exact cursor operations used by the renderer.
// It catches accidental growth and erasure of historical rows between frames.
type thinkingScreen struct {
	rows   [][]rune
	row    int
	column int
}

func (s *thinkingScreen) write(text string) {
	for len(text) > 0 {
		if strings.HasPrefix(text, "\x1b[") {
			end := 2
			for end < len(text) && (text[end] < 0x40 || text[end] > 0x7e) {
				end++
			}
			if end == len(text) {
				panic("incomplete renderer control sequence")
			}
			parameter := text[2:end]
			count, _ := strconv.Atoi(parameter)
			switch text[end] {
			case 'A':
				s.row -= max(1, count)
				if s.row < 0 {
					panic("renderer moved above screen history")
				}
			case 'K':
				if s.row < len(s.rows) {
					s.rows[s.row] = nil
				}
			case 'm':
			default:
				panic("unexpected renderer control sequence")
			}
			text = text[end+1:]
			continue
		}
		runes := []rune(text)
		r := runes[0]
		text = string(runes[1:])
		switch r {
		case '\r':
			s.column = 0
		case '\n':
			s.row++
			s.column = 0 // terminal ONLCR behavior for normal Fprintln output
		default:
			for len(s.rows) <= s.row {
				s.rows = append(s.rows, nil)
			}
			for len(s.rows[s.row]) <= s.column {
				s.rows[s.row] = append(s.rows[s.row], ' ')
			}
			s.rows[s.row][s.column] = r
			s.column++
		}
	}
}

func (s *thinkingScreen) text() string {
	rows := make([]string, len(s.rows))
	for i, row := range s.rows {
		rows[i] = string(row)
	}
	return strings.Join(rows, "\n")
}

func TestThinkingRollingTerminalPreservesHistoryAndToolTransitions(t *testing.T) {
	t.Setenv("TERM", "xterm")
	for _, colored := range []bool{false, true} {
		var output bytes.Buffer
		limit := 2
		style := terminalStyle{terminal: true, colored: colored}
		printer := eventOutput{stdout: &output, stderr: &output, stdoutStyle: style, stderrStyle: style,
			thinking: thinkingOutput{lines: &limit, size: func() (int, int) { return 40, 24 }}}
		screen := &thinkingScreen{}
		screen.write("previous answer\nprevious tool\n")
		for _, delta := range []string{"old line\n", "second line\n", "third", " line\nfourth line"} {
			printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: delta})
			screen.write(output.String())
			output.Reset()
			if !strings.HasPrefix(screen.text(), "previous answer\nprevious tool\n[thinking]\n") || len(screen.rows) > 5 {
				t.Fatalf("thinking erased history or grew past its window: %q", screen.text())
			}
		}
		if screen.text() != "previous answer\nprevious tool\n[thinking]\nthird line\nfourth line" {
			t.Fatalf("screen=%q", screen.text())
		}
		call := &iota.ToolCall{ID: "tool", Name: "bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}
		// No model_response event: tool_start must still close the editable area.
		printer.emit(iota.Event{Type: iota.EventToolStart, ToolCall: call})
		printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: "later reasoning"})
		printer.emit(iota.Event{Type: iota.EventRunEnd, Reason: "aborted"})
		screen.write(output.String())
		want := "previous answer\nprevious tool\n[thinking]\nthird line\nfourth line\n[/thinking]\n[bash] pwd\n[thinking]\nlater reasoning\n[/thinking]"
		if screen.text() != want {
			t.Fatalf("thinking overwrote tool output or failed to reset: %q", screen.text())
		}
	}
}

func TestThinkingTerminalHeightControlsAndDumbFallback(t *testing.T) {
	t.Setenv("TERM", "xterm")
	limit := 100
	var output bytes.Buffer
	printer := eventOutput{stdout: io.Discard, stderr: &output, stderrStyle: terminalStyle{terminal: true},
		thinking: thinkingOutput{lines: &limit, size: func() (int, int) { return 40, 6 }}}
	printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: "one\ntwo\nthree\nfour\nfive"})
	if !reflect.DeepEqual(printer.thinking.rows, []string{"three", "four", "five"}) {
		t.Fatal("window is taller than the available terminal")
	}
	printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: "\x1b[2J\r\b"})
	if strings.Contains(output.String(), "\x1b[2J") || strings.Contains(output.String(), "\b") {
		t.Fatal("model text could control the editable terminal area")
	}
	t.Setenv("TERM", "dumb")
	output.Reset()
	printer = eventOutput{stdout: io.Discard, stderr: &output, stderrStyle: terminalStyle{terminal: true}, thinking: thinkingOutput{lines: &limit}}
	printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: "kept content"})
	printer.emit(iota.Event{Type: iota.EventRunEnd})
	if strings.Contains(output.String(), "\x1b") || !strings.Contains(output.String(), "kept content") {
		t.Fatal("dumb terminal received cursor controls or lost retained content")
	}
}

func TestThinkingResizeDoesNotMoveAboveOwnedRows(t *testing.T) {
	t.Setenv("TERM", "xterm")
	limit, columns := 2, 40
	var output bytes.Buffer
	printer := eventOutput{stdout: io.Discard, stderr: &output, stderrStyle: terminalStyle{terminal: true},
		thinking: thinkingOutput{lines: &limit, size: func() (int, int) { return columns, 24 }}}
	screen := &thinkingScreen{}
	screen.write("historical answer\nhistorical tool\n")
	printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: "a fairly long thinking line\nsecond thinking line"})
	screen.write(output.String())
	output.Reset()
	columns = 10
	printer.emit(iota.Event{Type: iota.EventReasoningDelta, Reasoning: " continued"})
	screen.write(output.String())
	if !strings.HasPrefix(screen.text(), "historical answer\nhistorical tool\n[thinking]\n") || printer.thinking.drawnRows > limit {
		t.Fatalf("resize erased history or exceeded the limit: %q", screen.text())
	}
}

type thinkingTestProvider struct {
	prompts []string
	fail    bool
}

func (p *thinkingTestProvider) Stream(_ context.Context, request iota.Request, emit func(iota.Delta)) (iota.Response, error) {
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == iota.RoleUser && !request.Messages[i].RuntimeContext {
			p.prompts = append(p.prompts, request.Messages[i].Content)
			break
		}
	}
	emit(iota.Delta{Reasoning: "first reasoning\nsecond reasoning\nthird reasoning"})
	if p.fail {
		return iota.Response{}, errors.New("stream failed")
	}
	emit(iota.Delta{Text: "answer"})
	return iota.Response{Content: "answer", StopReason: "stop"}, nil
}

func TestInteractiveThinkingCommandsApplyAcrossRunsAndReset(t *testing.T) {
	provider := &thinkingTestProvider{}
	agent, err := iota.New(iota.Config{Provider: provider, Model: "test", DisableSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := io.WriteString(input, "/thinking line 0\none\n/reset\ntwo\n/thinking line 2\nthree\n/thinking line all\nfour\n/exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := interactive(agent, make(chan os.Signal), input, &stdout, &stderr, nil, nil); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !reflect.DeepEqual(provider.prompts, []string{"one", "two", "three", "four"}) || strings.Count(stderr.String(), "first reasoning") != 1 || strings.Count(stderr.String(), "second reasoning") != 2 {
		t.Fatalf("commands reached the model or preference was lost: prompts=%q output=%s", provider.prompts, stderr.String())
	}
}

func TestThinkingDisplayLimitPreservesSessionEventsAndClosesOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		provider := &thinkingTestProvider{fail: fail}
		agent, err := iota.New(iota.Config{Provider: provider, Model: "test", DisableSkills: true})
		if err != nil {
			t.Fatal(err)
		}
		session, err := iota.NewSession(t.TempDir(), iota.SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := execute(agent, "question", make(chan os.Signal), &stdout, &stderr, false, session, 0)
		if fail != (code == 1) || !strings.Contains(stderr.String(), "[/thinking]") || strings.Contains(stderr.String(), "first reasoning") {
			t.Fatalf("fail=%v code=%d stderr=%q", fail, code, stderr.String())
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(session.Path())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var record struct {
				Type    string     `json:"type"`
				Payload iota.Event `json:"payload"`
			}
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			if record.Type == string(iota.EventReasoningDelta) && record.Payload.Reasoning == "first reasoning\nsecond reasoning\nthird reasoning" {
				found = true
			}
		}
		if !found {
			t.Fatal("display limiting changed persisted reasoning")
		}
	}
}
