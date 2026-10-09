import assert from 'node:assert/strict';
import test from 'node:test';
import { timelineModes } from '../web/timeline-modes.js';

test('mode checkpoints persist across idle events, reset and runs', () => {
  const records = [
    { type: 'session_start' },
    { type: 'mode_changed', payload: { collaboration: { mode: 'plan' } } },
    { type: 'run_start', run_id: 'a' },
    { type: 'plan_saved', run_id: 'a', payload: { collaboration: { mode: 'plan' } } },
    { type: 'session_reset' },
    { type: 'plan_approved', payload: { collaboration: { mode: 'default' } } },
    { type: 'run_start', run_id: 'b' },
    { type: 'progress_updated', run_id: 'b', payload: { collaboration: { mode: 'default' } } },
  ];
  const modes = timelineModes(records);
  assert.deepEqual(records.map(record => modes.get(record).mode), ['default', 'plan', 'plan', 'plan', 'plan', 'default', 'default', 'default']);
  assert.equal(modes.get(records[3]).previous, 'plan');
  assert.equal(modes.get(records[5]).previous, 'plan');
});

test('request mode backfills its own run without recoloring earlier session events', () => {
  const records = [
    { type: 'session_start' },
    { type: 'run_start', run_id: 'plan-run' },
    { type: 'turn_start', run_id: 'plan-run' },
    { type: 'model_request', run_id: 'plan-run', payload: { request: { mode: 'plan' } } },
    { type: 'run_end', run_id: 'plan-run' },
    { type: 'run_start', run_id: 'default-run' },
    { type: 'model_request', run_id: 'default-run', payload: { request: { mode: 'default' } } },
  ];
  const modes = timelineModes(records);
  assert.deepEqual(records.map(record => modes.get(record).mode), ['default', 'plan', 'plan', 'plan', 'plan', 'default', 'default']);
});

test('new checkpoints take effect in order, including within the same run', () => {
  const records = [
    { run_id: 'a', payload: { request: { mode: 'plan' } } },
    { run_id: 'a', payload: { collaboration: { mode: 'default' } } },
    { run_id: 'a' },
  ];
  const modes = timelineModes(records);
  assert.deepEqual(records.map(record => modes.get(record).mode), ['plan', 'default', 'default']);
});

test('legacy and unrecognized modes use default and do not affect valid checkpoints', () => {
  const records = [
    { type: 'session_start' },
    { payload: { request: { mode: 'unknown' } } },
    { payload: { collaboration: { mode: 'plan' } } },
    { payload: { collaboration: { mode: '<script>' } } },
  ];
  const modes = timelineModes(records);
  assert.deepEqual(records.map(record => modes.get(record).mode), ['default', 'default', 'plan', 'plan']);
});
