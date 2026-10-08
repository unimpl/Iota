import { test } from 'node:test';
import assert from 'node:assert/strict';
import { contextState } from './web/context-state.js';
import { usageLabel } from './web/context-usage.js';

test('a later tool-pruning checkpoint retains the active summary file, reset clears it', () => {
  const summary = { seq: 10, type: 'context_compacted', payload: { compaction: {
    stage: 'summary', event_id: 10, summary_path: '/sessions/example.summary.10.md',
    context: { messages: [{ role: 'user', context_summary: true, content: 'old checkpoint' }] }
  } } };
  const pruning = { seq: 20, type: 'context_compacted', payload: { compaction: {
    stage: 'tool_results', context: { messages: [{ role: 'user', context_summary: true, content: 'old checkpoint' }] }
  } } };
  const state = contextState([summary, pruning]);
  assert.equal(state.checkpoint.seq, 20);
  assert.equal(state.summaryCheckpoint.payload.compaction.summary_path, '/sessions/example.summary.10.md');
  assert.equal(contextState([summary, pruning, { type: 'session_reset' }]).summaryCheckpoint, null);
});

test('usage labels distinguish an estimate and an unknown capacity', () => {
  assert.match(usageLabel({ tokens: 2000, estimated: true, remaining_percent: 75 }, 8000), /约 2,000 tokens.*8,000 上限.*约 75\.0% 可用/);
  assert.match(usageLabel({ tokens: 2000, estimated: true }, 0), /可用百分比未知/);
  assert.equal(usageLabel(null, 0), '上下文长度未知');
  assert.equal(usageLabel({ tokens: 0 }, 8000), '上下文长度未知');
});
