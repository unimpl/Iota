// Reconstruct the effective context without mixing raw history with checkpoints.
export function contextState(records) {
  let context = null;
  let checkpoint = null;
  let summaryCheckpoint = null;
  let status = 'idle';
  let error = '';
  let count = 0;
  for (const record of records) {
    const payload = record.payload || {};
    switch (record.type) {
      case 'session_reset':
        context = context ? { ...context, messages: [] } : null;
        checkpoint = null;
        summaryCheckpoint = null;
        status = 'idle';
        error = '';
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
        break;
    }
  }
  return { context, checkpoint, summaryCheckpoint, status, error, count };
}
