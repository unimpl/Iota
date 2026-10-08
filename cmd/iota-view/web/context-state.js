import { inputFromSummaryRequest } from './compaction-input.js';

// Reconstruct the effective context without mixing raw history with checkpoints.
export function contextState(records) {
  let context = null;
  let checkpoint = null;
  let summaryCheckpoint = null;
  let status = 'idle';
  let error = '';
  let count = 0;
  let attempt = null;
  for (const record of records) {
    const payload = record.payload || {};
    switch (record.type) {
      case 'session_reset':
        context = context ? { ...context, messages: [] } : null;
        checkpoint = null;
        summaryCheckpoint = null;
        status = 'idle';
        error = '';
        attempt = null;
        break;
      case 'model_request':
        context = { ...payload.request, messages: [...(payload.request?.messages || [])] };
        break;
      case 'message_added':
        if (payload.message) {
          context ||= { messages: [] };
          context.messages.push(payload.message);
        }
        break;
      case 'compaction_start':
        status = 'running';
        error = '';
        attempt = { start: record, input: null, inputSeq: record.seq, result: null, summary: '' };
        break;
      case 'compaction_prepared':
        if (attempt) {
          attempt.input = payload.compaction_input;
          attempt.inputSeq = record.seq;
        }
        break;
      case 'compaction_request':
        if (attempt && !attempt.input) {
          attempt.input = inputFromSummaryRequest(payload.request, attempt.start?.payload?.compaction);
          attempt.inputSeq = record.seq;
        }
        break;
      case 'compaction_delta':
        if (attempt && payload.reason === 'summary_complete') attempt.summary = payload.text || '';
        break;
      case 'context_compacted':
        if (payload.compaction?.context) {
          context = { ...payload.compaction.context, messages: [...(payload.compaction.context.messages || [])] };
          checkpoint = record;
          if (payload.compaction.stage === 'summary') summaryCheckpoint = record;
          count++;
        }
        break;
      case 'compaction_end':
        status = payload.is_error ? 'failed' : 'idle';
        error = payload.error || '';
        if (attempt) attempt.result = record;
        break;
    }
  }
  return { context, checkpoint, summaryCheckpoint, status, error, count, attempt };
}
