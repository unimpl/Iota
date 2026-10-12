package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/chzyer/readline"
	"github.com/rivo/uniseg"
)

const thinkingAll = -1

// thinkingCommand only changes this CLI's display preference, never model input.
func thinkingCommand(line string, lines *int, output io.Writer) (bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "/thinking" {
		return false, nil
	}
	if len(fields) != 1 {
		if len(fields) != 3 || fields[1] != "line" {
			return true, errors.New("usage: /thinking line <nonnegative integer|all>")
		}
		next := thinkingAll
		if fields[2] != "all" {
			var err error
			next, err = strconv.Atoi(fields[2])
			if err != nil || next < 0 {
				return true, errors.New("thinking line must be a nonnegative integer or all")
			}
		}
		*lines = next
	}
	value := "all"
	if *lines >= 0 {
		value = strconv.Itoa(*lines)
	}
	printLine(output, colorThinking, "thinking line: "+value)
	return true, nil
}

// thinkingOutput keeps only the current display window. The SDK still receives
// and records every original reasoning delta before this presentation layer.
type thinkingOutput struct {
	lines     *int // nil retains the existing unlimited output
	text      string
	rows      []string
	drawnRows int
	size      func() (int, int)
}

func (t *thinkingOutput) append(output io.Writer, style terminalStyle, text string) {
	if t.lines == nil || *t.lines == thinkingAll {
		fmt.Fprint(output, style.text(colorThinking, text))
		return
	}
	if *t.lines == 0 {
		return
	}
	columns, height := 80, 24
	if t.size != nil {
		columns, height = t.size()
	} else if file, ok := output.(*os.File); ok && style.terminal {
		if width, rows, err := readline.GetSize(int(file.Fd())); err == nil {
			columns, height = width, rows
		}
	}
	columns = max(1, columns)
	limit := *t.lines
	live := style.terminal && os.Getenv("TERM") != "dumb"
	if live {
		// Leave space for the header, closing marker, and current cursor row.
		limit = min(limit, max(1, height-3))
	}
	// Terminal controls from model text must not move the cursor out of our area.
	text = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	text = strings.ReplaceAll(text, "\t", "    ")
	t.text, t.rows = recentThinkingRows(t.text+text, columns, limit)
	if live {
		// Each frame uses explicit hard line breaks. Never infer additional old
		// rows from a narrower width: moving farther up could erase history.
		oldRows := min(t.drawnRows, max(1, height-2))
		if oldRows > 0 {
			fmt.Fprintf(output, "\x1b[%dA\r", oldRows)
		}
		for i := 0; i < max(oldRows, len(t.rows)); i++ {
			fmt.Fprint(output, "\r\x1b[2K")
			if i < len(t.rows) {
				fmt.Fprint(output, style.text(colorThinking, t.rows[i]))
			}
			// CR cancels delayed wrap at the right margin before advancing a row.
			fmt.Fprint(output, "\r\n")
		}
		if oldRows > len(t.rows) {
			fmt.Fprintf(output, "\x1b[%dA\r", oldRows-len(t.rows))
		}
		t.drawnRows = len(t.rows)
	}
}

func (t *thinkingOutput) finish(output io.Writer, style terminalStyle) {
	if t.lines == nil || *t.lines == thinkingAll {
		fmt.Fprintln(output, style.text(colorThinking, "\n[/thinking]"))
	} else {
		if *t.lines > 0 && t.drawnRows == 0 {
			for _, row := range t.rows {
				fmt.Fprintln(output, style.text(colorThinking, row))
			}
		}
		fmt.Fprintln(output, style.text(colorThinking, "[/thinking]"))
	}
	t.text, t.rows, t.drawnRows = "", nil, 0
}

// Keep a suffix at Unicode grapheme boundaries, counting terminal rows rather
// than bytes or newlines. A completed trailing newline is not an extra row.
func recentThinkingRows(text string, columns, limit int) (string, []string) {
	type row struct {
		start int
		text  string
	}
	var rows []row
	var current strings.Builder
	start, width := 0, 0
	flush := func() {
		rows = append(rows, row{start: start, text: current.String()})
		if len(rows) > limit {
			copy(rows, rows[1:])
			rows = rows[:limit]
		}
		current.Reset()
		width = 0
	}
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		from, to := graphemes.Positions()
		cluster := graphemes.Str()
		if cluster == "\n" {
			flush()
			start = to
			continue
		}
		cells := graphemes.Width()
		if width+cells > columns && current.Len() > 0 {
			flush()
			start = from
		}
		if cells > columns {
			cluster, cells = "?", 1
		}
		current.WriteString(cluster)
		width += cells
	}
	if current.Len() > 0 || len(rows) == 0 {
		flush()
	}
	result := make([]string, len(rows))
	for i, row := range rows {
		result[i] = row.text
	}
	return strings.Clone(text[rows[0].start:]), result
}
