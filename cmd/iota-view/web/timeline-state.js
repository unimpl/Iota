import { createContextState } from './context-state.js';
import { timelineModes } from './timeline-modes.js';

// Legacy raw reasoning is parsed once when received, not on every display tick.
export function reasoningChunk(record) {
  if (record.type === 'reasoning_delta') return record.payload?.reasoning || '';
  if (record.type !== 'model_stream_other') return '';
  const raw = record.payload?.raw_chunk || '';
  if (!raw.startsWith('data:')) return '';
  try {
    const chunk = JSON.parse(raw.slice(5).trim());
    return (chunk.choices || []).map(choice => {
      const delta = choice.delta || {};
      if (typeof delta.reasoning === 'string' && delta.reasoning) return delta.reasoning;
      if (typeof delta.reasoning_content === 'string' && delta.reasoning_content) return delta.reasoning_content;
      return (Array.isArray(delta.reasoning_details) ? delta.reasoning_details : [])
        .filter(detail => typeof detail?.text === 'string').map(detail => detail.text).join('');
    }).join('');
  } catch {
    return '';
  }
}

function recordedMode(record) {
  const mode = record.payload?.collaboration?.mode || record.payload?.request?.mode;
  return mode === 'default' || mode === 'plan' ? mode : null;
}

// Keep all raw records addressable through groups, but process each only once.
export class TimelineState {
  constructor() {
    this.groups = [];
    this.dirty = new Set();
    this.events = 0;
    this.lastSequence = 0;
    this.counts = { runs: 0, turns: 0, tools: 0 };
    this.metadata = null;
    this.context = createContextState();
    this.contextVersion = 0;
    this.mode = 'default';
    this.runModes = new Map();
    this.observedModes = new Map();
    this.activeTurn = null;
  }

  append(record) {
    if (Number.isSafeInteger(record.seq) && record.seq > 0) {
      if (record.seq <= this.lastSequence) return false;
      this.lastSequence = record.seq;
    }
    this.events++;
    if (!this.metadata && record.type === 'session_start') this.metadata = record;
    if (record.type === 'run_start') this.counts.runs++;
    if (record.type === 'turn_start') this.counts.turns++;
    if (record.type === 'tool_start') this.counts.tools++;
    if (this.context.append(record)) this.contextVersion++;

    const reasoning = reasoningChunk(record);
    const kind = record.type === 'compaction_delta' && !record.payload?.is_error && record.payload?.reason !== 'summary_complete'
      ? 'compaction_stream' : reasoning ? 'reasoning' : record.type;
    const streamed = ['text_delta', 'tool_call_delta', 'reasoning', 'compaction_stream'].includes(kind);
    const previous = this.groups.at(-1);
    const turn = record.payload?.turn;
    const mode = recordedMode(record);
    const previousMode = this.mode;
    this.mode = mode || (record.run_id && (this.observedModes.get(record.run_id) || this.runModes.get(record.run_id))) || this.mode;
    if (record.run_id) this.observedModes.set(record.run_id, this.mode);

    if (record.type === 'turn_start' && turn) {
      this.activeTurn = { key: record.run_id + ':' + record.seq, runID: record.run_id, turn };
    } else if (!this.activeTurn || record.run_id !== this.activeTurn.runID || turn !== this.activeTurn.turn || record.type === 'run_end') {
      this.activeTurn = null;
    }
    let group;
    if (streamed && previous?.kind === kind && previous.runID === record.run_id && previous.turn === turn) {
      group = previous;
      group.records.push(record);
    } else {
      const compaction = record.type.startsWith('compaction_') || record.type === 'context_compacted';
      group = { kind: streamed ? kind : 'event', record, records: streamed ? [record] : undefined,
        runID: record.run_id, turn, text: '', calls: new Map(), modeState: { mode: this.mode, previous: previousMode },
        turnKey: this.activeTurn?.key, inTurn: Boolean(this.activeTurn && !compaction) };
      this.groups.push(group);
    }
    if (kind === 'reasoning') group.text += reasoning;
    if (kind === 'text_delta') group.text += record.payload?.text || '';
    if (kind === 'compaction_stream') group.text += record.payload?.text || record.payload?.reasoning || '';
    if (kind === 'tool_call_delta') {
      const delta = record.payload?.tool_call_delta || {};
      const call = group.calls.get(delta.index) || { index: delta.index, id: '', name: '', arguments: '' };
      if (delta.id) call.id = delta.id;
      call.name += delta.name || '';
      call.arguments += delta.arguments || '';
      group.calls.set(delta.index, call);
    }
    this.dirty.add(group);

    // A run's first mode can arrive after its start. Backfill only when that
    // mode is first learned, over grouped entries rather than all raw deltas.
    if (record.run_id && mode && !this.runModes.has(record.run_id)) {
      this.runModes.set(record.run_id, mode);
      const modes = timelineModes(this.groups.map(item => item.record));
      for (const item of this.groups) {
        const next = modes.get(item.record);
        if (next.mode !== item.modeState.mode || next.previous !== item.modeState.previous) {
          item.modeState = next;
          this.dirty.add(item);
        }
        if (item.runID) this.observedModes.set(item.runID, next.mode);
      }
      this.mode = this.groups.at(-1).modeState.mode;
    }
    return true;
  }
}
