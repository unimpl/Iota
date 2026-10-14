package server

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestEventBatchesMatchFrontendReplay(t *testing.T) {
	data, err := os.ReadFile("testdata/event-batches.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Records []Record     `json:"records"`
		Batches []eventBatch `json:"batches"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var buffer eventBuffer
	var batches []eventBatch
	for _, record := range fixture.Records {
		immediate, err := buffer.add(record)
		if err != nil {
			t.Fatal(err)
		}
		if immediate {
			batches = append(batches, buffer.take())
		}
	}
	// Compare JSON semantically; field formatting in persisted payloads varies.
	actual, err := json.Marshal(batches)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(fixture.Batches)
	if err != nil {
		t.Fatal(err)
	}
	var actualJSON, expectedJSON interface{}
	if err := json.Unmarshal(actual, &actualJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &expectedJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualJSON, expectedJSON) {
		t.Fatalf("batch differs from frontend fixture: %s", actual)
	}
	if bytes.Contains(actual, []byte("raw-provider")) || bytes.Contains(actual, []byte("model_stream")) {
		t.Fatalf("provider traces leaked: %s", actual)
	}
}

func TestDeltaBatchSeparatesRunsAndTurnsAndAdvancesOmittedCursor(t *testing.T) {
	var buffer eventBuffer
	for _, record := range []Record{
		{Sequence: 1, RunID: "first", Type: "text_delta", Payload: json.RawMessage(`{"turn":1,"text":"a"}`)},
		{Sequence: 2, RunID: "first", Type: "text_delta", Payload: json.RawMessage(`{"turn":2,"text":"b"}`)},
		{Sequence: 3, RunID: "second", Type: "text_delta", Payload: json.RawMessage(`{"turn":1,"text":"c"}`)},
		{Sequence: 4, RunID: "second", Type: "model_stream_other"},
	} {
		if _, err := buffer.add(record); err != nil {
			t.Fatal(err)
		}
	}
	batch := buffer.take()
	if batch.Cursor != 4 || len(batch.Records) != 3 {
		t.Fatalf("batch=%+v", batch)
	}
	for index, record := range batch.Records {
		if record.Sequence != uint64(index+1) {
			t.Fatalf("record=%+v", record)
		}
	}
	if _, err := buffer.add(Record{Sequence: 5, Type: "model_stream_finish"}); err != nil {
		t.Fatal(err)
	}
	batch = buffer.take()
	if batch.Cursor != 5 || len(batch.Records) != 0 {
		t.Fatalf("checkpoint=%+v", batch)
	}
}
