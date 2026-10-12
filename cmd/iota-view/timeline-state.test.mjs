import test from 'node:test';
import assert from 'node:assert/strict';
import { TimelineState } from './web/timeline-state.js';
import { timelineModes } from './web/timeline-modes.js';

function record(seq, type, payload = {}, runID = 'run') {
  return { seq, type, payload, run_id: runID };
}

test('a live tail updates only its existing group and does not replay context', () => {
  const state = new TimelineState();
  const messages = [{ role: 'user', content: 'history' }];
  state.append(record(1, 'session_start', { model: 'test' }));
  state.append(record(2, 'run_start'));
  state.append(record(3, 'turn_start', { turn: 1 }));
  state.append(record(4, 'model_request', { turn: 1, request: { mode: 'default', messages } }));
  state.append(record(5, 'reasoning_delta', { turn: 1, reasoning: 'first ' }));
  const historical = state.groups[0];
  const tail = state.groups.at(-1);
  const context = state.context.state.context;
  const version = state.contextVersion;
  state.dirty.clear();
  for (let seq = 6; seq < 10006; seq++) state.append(record(seq, 'reasoning_delta', { turn: 1, reasoning: 'next ' }));
  assert.equal(state.groups.length, 5);
  assert.equal(state.groups[0], historical);
  assert.equal(state.groups.at(-1), tail);
  assert.equal(tail.records.length, 10001);
  assert.equal(state.dirty.size, 1);
  assert.equal(state.context.state.context, context);
  assert.equal(state.contextVersion, version);
  assert.equal(messages.length, 1);
  assert.equal(state.events, 10005);
  assert.deepEqual(state.counts, { runs: 1, turns: 1, tools: 0 });
});

test('replayed sequence IDs are ignored before counters, groups or context change', () => {
  const state = new TimelineState();
  const event = record(1, 'message_added', { message: { role: 'user', content: 'one' } });
  assert.equal(state.append(event), true);
  state.dirty.clear();
  assert.equal(state.append(event), false);
  assert.equal(state.events, 1);
  assert.equal(state.groups.length, 1);
  assert.equal(state.context.state.context.messages.length, 1);
  assert.equal(state.dirty.size, 0);
  const reset = new TimelineState();
  assert.equal(reset.append(event), true);
});

test('mode backfill matches full replay including mode changes within a run', () => {
  const records = [
    record(1, 'session_start', {}, undefined),
    record(2, 'run_start'),
    record(3, 'turn_start', { turn: 1 }),
    record(4, 'model_request', { turn: 1, request: { mode: 'plan', messages: [] } }),
    record(5, 'reasoning_delta', { turn: 1, reasoning: 'plan ' }),
    record(6, 'reasoning_delta', { turn: 1, reasoning: 'more ' }),
    record(7, 'mode_changed', { collaboration: { mode: 'default' } }),
    record(8, 'session_reset'),
    record(9, 'run_end'),
    record(10, 'run_start', {}, 'next'),
    record(11, 'model_request', { request: { mode: 'default', messages: [] } }, 'next'),
  ];
  const state = new TimelineState();
  for (const item of records) state.append(item);
  const expected = timelineModes(records);
  for (const group of state.groups) assert.deepEqual(group.modeState, expected.get(group.record));
  assert.equal(state.mode, 'default');
  assert.equal(state.groups[1].modeState.mode, 'plan');
});

test('grouping preserves boundaries, raw records and accumulated tool arguments', () => {
  const state = new TimelineState();
  const records = [
    record(1, 'tool_call_delta', { turn: 1, tool_call_delta: { index: 0, id: 'call', name: 're', arguments: '{"pa' } }),
    record(2, 'tool_call_delta', { turn: 1, tool_call_delta: { index: 0, name: 'ad', arguments: 'th":"file"}' } }),
    record(3, 'tool_call_delta', { turn: 2, tool_call_delta: { index: 1, name: 'list' } }),
    record(4, 'model_stream_other', { turn: 2, raw_chunk: 'data: {"choices":[{"delta":{"reasoning_content":"legacy thought"}}]}' }),
    record(5, 'compaction_delta', { text: 'partial ' }),
    record(6, 'compaction_delta', { reasoning: 'summary' }),
    record(7, 'compaction_delta', { reason: 'summary_complete', text: 'complete' }),
  ];
  for (const item of records) state.append(item);
  assert.equal(state.groups.length, 5);
  assert.deepEqual(state.groups[0].records, records.slice(0, 2));
  assert.deepEqual(state.groups[0].calls.get(0), { index: 0, id: 'call', name: 'read', arguments: '{"path":"file"}' });
  assert.equal(state.groups[2].text, 'legacy thought');
  assert.equal(state.groups[3].text, 'partial summary');
  assert.equal(state.groups[4].kind, 'event');
});

test('compaction is visible inside collapsed turns and context stays independent', () => {
  const state = new TimelineState();
  const source = [{ role: 'user', context_summary: true, content: 'checkpoint' }];
  const records = [
    record(1, 'turn_start', { turn: 1 }),
    record(2, 'model_request', { turn: 1, request: { mode: 'default', messages: [] } }),
    record(3, 'compaction_start', { turn: 1 }),
    record(4, 'compaction_delta', { turn: 1, text: 'partial' }),
    record(5, 'context_compacted', { turn: 1, compaction: { stage: 'summary', context: { messages: source } } }),
    record(6, 'compaction_end', { turn: 1 }),
    record(7, 'message_added', { turn: 1, message: { role: 'assistant', content: 'continue' } }),
  ];
  for (const item of records) state.append(item);
  assert.equal(state.groups[1].inTurn, true);
  for (const group of state.groups.slice(2, 6)) assert.equal(group.inTurn, false);
  assert.equal(state.groups[6].turnKey, 'run:1');
  assert.equal(state.context.state.context.messages.length, 2);
  assert.equal(source.length, 1);
  assert.equal(state.context.state.count, 1);
});
