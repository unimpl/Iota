import { test } from 'node:test';
import assert from 'node:assert/strict';
import { contextState } from './web/context-state.js';

const checkpoint = {
  type: 'context_compacted', seq: 10,
  payload: { compaction: { stage: 'summary', keep_recent_turns: 3, context: {
    mode: 'plan', system_prompt: 'current plan rules', tools: [{ name: 'read' }],
    messages: [{ role: 'user', context_summary: true, content: 'checkpoint summary' }, { role: 'user', content: 'recent' }]
  } } }
};

test('checkpoint replaces history; later messages append without mutating the log', () => {
  const records = [
    { type: 'message_added', payload: { message: { role: 'user', content: 'raw old history' } } },
    checkpoint,
    { type: 'compaction_end', payload: {} },
    { type: 'message_added', payload: { message: { role: 'assistant', content: 'continued' } } }
  ];
  const state = contextState(records);
  assert.equal(state.context.messages.length, 3);
  assert.equal(state.context.messages[0].context_summary, true);
  assert.equal(state.context.system_prompt, 'current plan rules');
  assert.equal(state.checkpoint.seq, 10);
  assert.equal(state.count, 1);
  assert.equal(checkpoint.payload.compaction.context.messages.length, 2);
  assert.equal(contextState(records).context.messages.length, 3);
});

test('failed compression preserves the last checkpoint, reset clears it', () => {
  const records = [checkpoint, { type: 'compaction_start' }, { type: 'compaction_end', payload: { is_error: true, error: 'canceled' } }];
  const state = contextState(records);
  assert.equal(state.status, 'failed');
  assert.equal(state.error, 'canceled');
  assert.equal(state.context.messages.length, 2);
  const reset = contextState([...records, { type: 'session_reset' }]);
  assert.equal(reset.checkpoint, null);
  assert.deepEqual(reset.context.messages, []);
  assert.equal(reset.status, 'idle');
});

test('summary requests cannot replace actual context, normal requests can', () => {
  const state = contextState([checkpoint,
    { type: 'compaction_request', payload: { request: { messages: [{ role: 'user', content: 'summarize' }] } } },
    { type: 'compaction_start' }
  ]);
  assert.equal(state.status, 'running');
  assert.equal(state.context.messages[0].content, 'checkpoint summary');
  const next = contextState([checkpoint, { type: 'model_request', payload: { request: { mode: 'default', system_prompt: 'refreshed rules', messages: [{ role: 'user', content: 'new' }] } } }]);
  assert.equal(next.context.mode, 'default');
  assert.equal(next.context.system_prompt, 'refreshed rules');
  assert.equal(next.context.messages.length, 1);
});
