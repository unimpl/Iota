import { test } from 'node:test';
import assert from 'node:assert/strict';
import { inputFromSummaryRequest } from './web/compaction-input.js';
import { contextState } from './web/context-state.js';

test('older logs expose only selected history and preserve current context on failure', () => {
  const old = [{ role: 'user', content: 'old greeting' }, { role: 'assistant', content: 'old reply' }];
  const active = [...old, ...Array.from({ length: 6 }, (_, index) => ({ role: index % 2 ? 'assistant' : 'user', content: 'protected' }))];
  const request = { messages: [{ role: 'user', content: '<conversation>\n' + JSON.stringify(old) + '\n</conversation>\nSummarize.' }] };
  const start = { seq: 7199, type: 'compaction_start', payload: { compaction: { before_messages: 8, keep_recent_turns: 3 } } };
  const end = { seq: 7767, type: 'compaction_end', payload: { is_error: true, error: 'did not reduce context' } };
  const state = contextState([
    { type: 'model_request', payload: { request: { messages: active } } }, start,
    { seq: 7200, type: 'compaction_request', payload: { request } },
    { type: 'compaction_delta', payload: { reason: 'summary_complete', text: 'longer summary' } }, end
  ]);
  assert.deepEqual(state.attempt.input.messages, old);
  assert.equal(state.attempt.input.kept_turns, 3);
  assert.equal(state.attempt.input.kept_messages, 6);
  assert.equal(state.attempt.summary, 'longer summary');
  assert.equal(state.attempt.result.seq, 7767);
  assert.deepEqual(state.context.messages, active);
  assert.equal(state.checkpoint, null);
});

test('prepared event is authoritative; summary request may already contain clipped tools', () => {
  const original = { messages: [{ role: 'tool', content: 'original tool output' }], kept_turns: 3 };
  const state = contextState([
    { type: 'compaction_start' },
    { seq: 2, type: 'compaction_prepared', payload: { compaction_input: original } },
    { type: 'compaction_request', payload: { request: { messages: [{ content: '<conversation>\n[{"role":"tool","content":"clipped"}]\n</conversation>' }] } } }
  ]);
  assert.equal(state.attempt.input, original);
  assert.equal(contextState([{ type: 'compaction_start' }, { type: 'session_reset' }]).attempt, null);
});

test('fragmented or malformed summary input does not claim a complete scope', () => {
  for (const content of ['no tags', '<conversation>\n[{"role":\n</conversation>', '<conversation>\n[null]\n</conversation>']) {
    assert.equal(inputFromSummaryRequest({ messages: [{ content }] }), null);
  }
});
