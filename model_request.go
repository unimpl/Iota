package iota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

func (a *Agent) requestWithCompaction(ctx context.Context, turn int, emit EmitFunc, record func(Event) error) (Response, error) {
	forceSummary := false
	for recovery := 0; ; recovery++ {
		request, err := a.currentRequest(ctx)
		if err != nil {
			return Response{}, err
		}
		response, err := a.callModel(ctx, request, turn, false, emit)
		if err == nil && response.StopReason == "model_context_window_exceeded" {
			err = errors.New("provider stopped: model_context_window_exceeded")
		}
		limit, overflow := contextOverflow(err)
		if !overflow || ctx.Err() != nil {
			return response, err
		}
		if limit > 0 {
			a.mu.Lock()
			a.contextLimitTokens = limit
			a.mu.Unlock()
		}
		if recovery >= 2 || forceSummary {
			return response, fmt.Errorf("context still exceeds capacity after compaction: %w", err)
		}
		state, compactErr := a.compact(ctx, "", "overflow", recovery > 0, turn, emit, record)
		if compactErr != nil {
			return response, errors.Join(err, compactErr)
		}
		forceSummary = state.Stage == "summary"
	}
}

// callModel separates summary traffic from ordinary assistant output while recording both.
func (a *Agent) callModel(ctx context.Context, request Request, turn int, summary bool, emit EmitFunc) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	requestEvent := request
	requestEvent.Messages = cloneMessages(request.Messages)
	requestEvent.Tools = append([]ToolDefinition(nil), request.Tools...)
	for i := range requestEvent.Tools {
		requestEvent.Tools[i].Schema = append(json.RawMessage(nil), request.Tools[i].Schema...)
	}
	kind := EventModelRequest
	if summary {
		kind = EventCompactionRequest
	}
	requestRecord := Event{Type: kind, Turn: turn, Request: &requestEvent}
	if encoder, ok := a.provider.(RequestEncoder); ok {
		body, err := encoder.EncodeRequest(request)
		if err != nil {
			return Response{}, err
		}
		requestRecord.RawRequest = string(body)
	}
	emitEvent(emit, requestRecord)
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	response, err := a.provider.Stream(ctx, request, func(delta Delta) {
		if summary {
			emitEvent(emit, Event{Type: EventCompactionDelta, Turn: turn, Text: delta.Text, Reasoning: delta.Reasoning,
				RawChunk: delta.RawChunk, Reason: delta.FinishReason, Usage: delta.Usage})
			return
		}
		if delta.Reasoning != "" {
			emitEvent(emit, Event{Type: EventReasoningDelta, Turn: turn, Reasoning: delta.Reasoning, RawChunk: delta.RawChunk})
		}
		if delta.Text != "" {
			emitEvent(emit, Event{Type: EventTextDelta, Turn: turn, Text: delta.Text, RawChunk: delta.RawChunk})
		}
		if delta.ToolCall != nil {
			call := *delta.ToolCall
			emitEvent(emit, Event{Type: EventToolCallDelta, Turn: turn, ToolCallDelta: &call, RawChunk: delta.RawChunk})
		}
		if delta.FinishReason != "" {
			emitEvent(emit, Event{Type: EventStreamFinish, Turn: turn, Reason: delta.FinishReason, Usage: delta.Usage, RawChunk: delta.RawChunk})
		}
		if delta.StreamDone {
			emitEvent(emit, Event{Type: EventStreamDone, Turn: turn, RawChunk: delta.RawChunk})
		}
		if delta.RawChunk != "" && delta.Reasoning == "" && delta.Text == "" && delta.ToolCall == nil && delta.FinishReason == "" && !delta.StreamDone {
			emitEvent(emit, Event{Type: EventStreamOther, Turn: turn, RawChunk: delta.RawChunk})
		}
	})
	if err != nil {
		kind := EventModelError
		if summary {
			kind = EventCompactionDelta
		}
		emitEvent(emit, Event{Type: kind, Turn: turn, IsError: true, Error: err.Error()})
	}
	return response, err
}
