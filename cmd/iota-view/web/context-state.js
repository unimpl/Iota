import { inputFromSummaryRequest } from './compaction-input.js';

// Reconstruct the effective context without mixing raw history with checkpoints.
export function contextState(records) {
  const reducer = createContextState();
  for (const record of records) reducer.append(record);
  return reducer.state;
}

// Only semantic context events change this state; stream fragments are O(1)
// no-ops and do not clone earlier requests or rebuild the context panel.
export function createContextState() {
  const state = { context: null, checkpoint: null, summaryCheckpoint: null, status: 'idle', error: '', count: 0, attempt: null };
  const append = record => {
    const payload = record.payload || {};
    switch (record.type) {
      case 'session_reset':
        state.context = state.context ? { ...state.context, messages: [] } : null;
        state.checkpoint = null;
        state.summaryCheckpoint = null;
        state.status = 'idle';
        state.error = '';
        state.attempt = null;
        break;
      case 'model_request':
        state.context = { ...payload.request, messages: [...(payload.request?.messages || [])] };
        break;
      case 'message_added':
        if (payload.message) {
          state.context ||= { messages: [] };
          state.context.messages.push(payload.message);
        }
        break;
      case 'compaction_start':
        state.status = 'running';
        state.error = '';
        state.attempt = { start: record, input: null, inputSeq: record.seq, result: null, summary: '' };
        break;
      case 'compaction_prepared':
        if (state.attempt) {
          state.attempt.input = payload.compaction_input;
          state.attempt.inputSeq = record.seq;
        }
        break;
      case 'compaction_request':
        if (state.attempt && !state.attempt.input) {
          state.attempt.input = inputFromSummaryRequest(payload.request, state.attempt.start?.payload?.compaction);
          state.attempt.inputSeq = record.seq;
        } else {
          return false;
        }
        break;
      case 'compaction_delta':
        if (!state.attempt || payload.reason !== 'summary_complete') return false;
        state.attempt.summary = payload.text || '';
        break;
      case 'context_compacted':
        if (payload.compaction?.context) {
          state.context = { ...payload.compaction.context, messages: [...(payload.compaction.context.messages || [])] };
          state.checkpoint = record;
          if (payload.compaction.stage === 'summary') state.summaryCheckpoint = record;
          state.count++;
        }
        break;
      case 'compaction_end':
        state.status = payload.is_error ? 'failed' : 'idle';
        state.error = payload.error || '';
        if (state.attempt) state.attempt.result = record;
        break;
      default:
        return false;
    }
    return true;
  };
  return { state, append };
}
