package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chzyer/readline"
	iota "github.com/unimpl/Iota"
)

type userInputUI struct {
	editor *readline.Instance
	input  *questionInput
	output io.Writer
}

func (ui *userInputUI) handle(ctx context.Context, request iota.UserInputRequest) (iota.UserInputResponse, error) {
	response := iota.UserInputResponse{Answers: make(map[string]iota.UserInputAnswer)}
	if ui.editor == nil {
		return response, errors.New("interactive input is unavailable")
	}
	ui.input.asking.Store(true)
	defer ui.input.asking.Store(false)
	stop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			ui.input.interrupt()
		case <-stop:
		}
	}()
	defer func() {
		close(stop)
		<-watchDone
		select {
		case <-ui.input.interrupted:
		default:
		}
	}()
	for index, question := range request.Questions {
		if err := ctx.Err(); err != nil {
			response.Cancelled = true
			return response, err
		}
		options := make([]iota.UserInputOption, 0, len(question.Options))
		for _, option := range question.Options {
			if option.ID == question.RecommendedOptionID {
				options = append(options, option)
			}
		}
		for _, option := range question.Options {
			if option.ID != question.RecommendedOptionID {
				options = append(options, option)
			}
		}
		style := styleFor(ui.output)
		renderUserInputQuestion(ui.output, style, request, index, options)
		ui.editor.SetPrompt(style.text(colorUserLabel, "[you:answer] > "))
		line, err := ui.editor.Readline()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, readline.ErrInterrupt) {
				err = context.Canceled
			}
			response.Cancelled = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			message := "╰─ Input failed."
			if response.Cancelled {
				message = "╰─ Input cancelled; this run ends."
			}
			fmt.Fprintln(ui.output, style.text(colorThinking, message))
			return response, err
		}
		answer := iota.UserInputAnswer{Source: "custom", Value: line}
		line = strings.TrimSpace(line)
		if request.Format == iota.UserInputStructured {
			answer.Value = line
		}
		if line == "" {
			answer = iota.UserInputAnswer{Source: "unanswered"}
			if request.Format == iota.UserInputStructured {
				answer = iota.UserInputAnswer{Source: "recommended", OptionID: options[0].ID, Value: options[0].Label}
			}
		} else if number, err := strconv.Atoi(line); request.Format == iota.UserInputStructured && err == nil && number >= 1 && number <= len(options) {
			answer = iota.UserInputAnswer{Source: "selected", OptionID: options[number-1].ID, Value: options[number-1].Label}
		}
		response.Answers[question.ID] = answer
		message := "╰─ Reply recorded: " + answer.Value
		if answer.Source == "unanswered" {
			message = "╰─ No answer provided."
		}
		fmt.Fprintln(ui.output, style.text(colorUser, message))
	}
	return response, nil
}

func renderUserInputQuestion(output io.Writer, style terminalStyle, request iota.UserInputRequest, index int, options []iota.UserInputOption) {
	question := request.Questions[index]
	fmt.Fprintln(output, "\n"+style.text(colorReplyLabel, "╭─ Assistant question"))
	fmt.Fprintln(output, style.text(colorThinking, fmt.Sprintf("│ Input %d/%d · Question %d/%d", request.CallIndex, request.CallCount, index+1, len(request.Questions))))
	fmt.Fprintln(output, "│")
	for _, line := range strings.Split(question.Question, "\n") {
		fmt.Fprintln(output, "│ "+style.text(colorAssistant, line))
	}
	fmt.Fprintln(output, "│")
	if request.Format == iota.UserInputStructured {
		for optionIndex, option := range options {
			label := option.Label
			if option.ID == question.RecommendedOptionID {
				label += " (recommended)"
			}
			fmt.Fprintf(output, "│ %s\n", style.text(colorAssistant, fmt.Sprintf("%d. %s", optionIndex+1, label)))
			for _, line := range strings.Split(option.Description, "\n") {
				fmt.Fprintln(output, "│    "+style.text(colorThinking, line))
			}
		}
		fmt.Fprintln(output, "│")
		fmt.Fprintln(output, style.text(colorThinking, "│ Enter: use "+options[0].Label+"; number: select; text: custom answer."))
	} else {
		fmt.Fprintln(output, style.text(colorThinking, "│ Write your reply. Empty Enter means no answer provided."))
	}
	fmt.Fprintln(output, style.text(colorThinking, "│ Esc/Ctrl+C: cancel this run."))
}

type inputByte struct {
	value byte
	err   error
}

// questionInput avoids prefetching future prompts and translates a lone Esc to
// readline's interrupt key while preserving cursor-key escape sequences.
type questionInput struct {
	source      *readline.CancelableStdin
	asking      atomic.Bool
	interrupted chan struct{}
	closed      chan struct{}
	closeOnce   sync.Once
	pending     chan inputByte
	buffer      []byte
}

func newQuestionInput(source io.Reader) *questionInput {
	return &questionInput{source: readline.NewCancelableStdin(source), interrupted: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (input *questionInput) interrupt() {
	select {
	case input.interrupted <- struct{}{}:
	default:
	}
}

func (input *questionInput) startRead() {
	if input.pending != nil {
		return
	}
	input.pending = make(chan inputByte, 1)
	pending := input.pending
	go func() {
		var data [1]byte
		_, err := io.ReadFull(input.source, data[:])
		pending <- inputByte{data[0], err}
	}()
}

func (input *questionInput) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if len(input.buffer) > 0 {
		data[0] = input.buffer[0]
		input.buffer = input.buffer[1:]
		return 1, nil
	}
	input.startRead()
	var read inputByte
	select {
	case <-input.closed:
		return 0, io.EOF
	case <-input.interrupted:
		data[0] = readline.CharInterrupt
		return 1, nil
	case read = <-input.pending:
		input.pending = nil
	}
	if read.err != nil {
		return 0, read.err
	}
	data[0] = read.value
	if read.value != readline.CharEsc || !input.asking.Load() {
		return 1, nil
	}
	input.startRead()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-input.closed:
		return 0, io.EOF
	case <-input.interrupted:
		data[0] = readline.CharInterrupt
	case <-timer.C:
		data[0] = readline.CharInterrupt
	case next := <-input.pending:
		input.pending = nil
		if next.err == nil {
			input.buffer = append(input.buffer, next.value)
		}
		if next.err != nil || (next.value != '[' && next.value != 'O') {
			data[0] = readline.CharInterrupt
		}
	}
	return 1, nil
}

func (input *questionInput) Close() error {
	input.closeOnce.Do(func() { close(input.closed); input.source.Close() })
	return nil
}
