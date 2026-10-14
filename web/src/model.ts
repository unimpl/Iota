import type { Collaboration, EventBatch, SessionRecord } from "./types";

export interface ToolActivity {
  id: string;
  name: string;
  arguments: string;
  result: string;
  status: "running" | "completed" | "failed" | "interrupted";
}

export interface ChatItem {
  id: string;
  seq: number;
  run_id: string;
  role: "user" | "assistant" | "notice";
  text: string;
  reasoning: string;
  tools: ToolActivity[];
  complete: boolean;
  error?: string;
}

export interface RunIndex { id: string; start_seq: number; end_seq: number; started_at: string; title: string; preview: string; status: string }
export interface ChatModel { items: ChatItem[]; runs: RunIndex[]; loadedRuns: string[]; sequence: number; run: string; turn: number; collaboration?: Collaboration; action?: string }
export interface HistorySnapshot { items: ChatItem[]; runs: RunIndex[]; loaded_runs: string[]; cursor: number; run: string; turn: number; collaboration?: Collaboration; action?: string }
export const emptyModel: ChatModel = { items: [], runs: [], loadedRuns: [], sequence: 0, run: "", turn: 0 };

export function fromHistory(snapshot: HistorySnapshot): ChatModel {
  return { items: snapshot.items, runs: snapshot.runs, loadedRuns: snapshot.loaded_runs, sequence: snapshot.cursor, run: snapshot.run, turn: snapshot.turn, collaboration: snapshot.collaboration, action: snapshot.action };
}

// Older pages never replace live items or advance the SSE cursor. A response can
// include newer index metadata, but SSE remains responsible for live content.
export function mergeHistory(model: ChatModel, snapshot: HistorySnapshot): ChatModel {
  const items = new Map(snapshot.items.map(item => [item.id, item]));
  for (const item of model.items) items.set(item.id, item);
  const runs = new Map(snapshot.runs.map(run => [run.id, run]));
  for (const run of model.runs) {
    if (run.end_seq >= (runs.get(run.id)?.end_seq ?? 0)) runs.set(run.id, run);
  }
  return { ...model, items: [...items.values()].sort((a, b) => a.seq - b.seq), runs: [...runs.values()].sort((a, b) => a.start_seq - b.start_seq), loadedRuns: [...new Set([...model.loadedRuns, ...snapshot.loaded_runs])] };
}

export function historyExcerpt(text: string, limit: number): string {
  const chars = Array.from(text.trim().replace(/\s+/gu, " "));
  return chars.length > limit ? chars.slice(0, limit).join("") + "…" : chars.join("");
}

export function applyBatch(model: ChatModel, batch: EventBatch): ChatModel {
  if (batch.cursor <= model.sequence) return model;
  const next = batch.records.reduce(applyRecord, model);
  return { ...next, sequence: batch.cursor };
}

// Build visible conversation from the immutable log, never compacted model context.
// Full assistant messages replace their streamed draft instead of duplicating it.
export function applyRecord(model: ChatModel, record: SessionRecord): ChatModel {
  if (record.seq <= model.sequence) return model;
  const payload = record.payload ?? {};
  const next = { ...model, items: model.items, sequence: record.seq };
  if (record.run_id) next.run = record.run_id;
  if (record.run_id && !next.loadedRuns.includes(record.run_id)) next.loadedRuns = [...next.loadedRuns, record.run_id];
  const position = next.runs.findIndex(run => run.id === next.run);
  if (next.run) {
    next.runs = [...next.runs];
    if (position < 0) {
      next.runs.push({ id: next.run, start_seq: record.seq, end_seq: record.seq, started_at: record.timestamp ?? "", title: "未命名任务", preview: "", status: "running" });
      if (!next.loadedRuns.includes(next.run)) next.loadedRuns = [...next.loadedRuns, next.run];
    } else next.runs[position] = { ...next.runs[position], end_seq: record.seq };
  }
  if (record.type === "run_start") next.turn = 0;
  if (record.type === "turn_start") next.turn = payload.turn ?? next.turn + 1;
  if (record.type === "client_request") next.action = payload.action;
  if (payload.collaboration) next.collaboration = payload.collaboration;
  if (record.type === "session_reset") next.collaboration = { mode: model.collaboration?.mode ?? "default" };
  const turn = payload.turn ?? next.turn;
  const assistantID = `assistant-${next.run}-${turn}`;
  const updateAssistant = (update: (item: ChatItem) => ChatItem) => {
    const index = next.items.findIndex(item => item.id === assistantID);
    const item = index < 0 ? { id: assistantID, seq: record.seq, run_id: next.run, role: "assistant" as const, text: "", reasoning: "", tools: [], complete: false } : next.items[index];
    next.items = [...next.items];
    if (index < 0) next.items.push(update(item));
    else next.items[index] = update(item);
  };
  const notice = (text: string, error?: string) => {
    next.items = [...next.items, { id: `notice-${record.seq}`, seq: record.seq, run_id: next.run, role: "notice", text, reasoning: "", tools: [], complete: true, error }];
  };
  switch (record.type) {
    case "message_added": {
      const message = payload.message;
      if (!message || message.skill || message.context_summary || message.runtime_context) break;
      if (message.role === "user") {
        const text = next.action === "execute" ? "批准并执行当前计划" : message.content ?? "";
        next.items = [...next.items, { id: `user-${record.seq}`, seq: record.seq, run_id: next.run, role: "user", text, reasoning: "", tools: [], complete: true }];
        const run = next.runs.find(run => run.id === next.run);
        if (run?.title === "未命名任务") run.title = historyExcerpt(text, 80);
        next.action = undefined;
      } else if (message.role === "assistant") {
        updateAssistant(item => ({ ...item, text: message.content ?? "", complete: true }));
        const run = next.runs.find(run => run.id === next.run);
        if (run && message.content?.trim()) run.preview = historyExcerpt(message.content, 160);
      } else if (message.role === "tool") {
        // Opening a crashed session may append repaired results without run/turn IDs.
        let target = record.run_id ? next.items.findIndex(item => item.id === assistantID) : -1;
        if (!record.run_id) {
          for (let index = next.items.length - 1; index >= 0; index--) {
            if (next.items[index].tools.some(tool => tool.id === message.tool_call_id && tool.status === "running")) { target = index; break; }
          }
        }
        if (target >= 0) {
          next.items = [...next.items];
          const item = next.items[target];
          next.items[target] = { ...item, tools: item.tools.map(tool => tool.id === message.tool_call_id ? { ...tool, result: message.content ?? "", status: message.is_error ? "failed" : "completed" } : tool) };
        }
      }
      break;
    }
    case "text_delta": updateAssistant(item => ({ ...item, text: item.text + (payload.text ?? "") })); break;
    case "reasoning_delta": updateAssistant(item => ({ ...item, reasoning: item.reasoning + (payload.reasoning ?? "") })); break;
    case "tool_start": {
      const call = payload.tool_call;
      if (call) updateAssistant(item => ({ ...item, tools: [...item.tools, { id: call.id, name: call.name, arguments: JSON.stringify(call.arguments, null, 2), result: "", status: "running" }] }));
      break;
    }
    case "tool_end": {
      const call = payload.tool_call;
      if (call) updateAssistant(item => ({ ...item, tools: item.tools.map(tool => tool.id === call.id ? { ...tool, result: payload.tool_result ?? "", status: payload.is_error ? "failed" : "completed" } : tool) }));
      break;
    }
    case "run_end":
      next.runs = next.runs.map(run => run.id === next.run ? { ...run, status: payload.reason === "error" ? "failed" : payload.reason === "aborted" ? "aborted" : "completed" } : run);
      next.items = next.items.map(item => item.id.startsWith(`assistant-${next.run}-`) ? { ...item, complete: true, error: !item.complete ? (payload.reason === "aborted" ? "回复已中断" : payload.error) : item.error, tools: item.tools.map(tool => tool.status === "running" ? { ...tool, status: "interrupted" } : tool) } : item);
      if (payload.error) notice(payload.reason === "aborted" ? "已停止本次任务" : "本次任务失败", payload.reason === "aborted" ? undefined : payload.error);
      break;
    case "session_reset": notice("已重置上下文。之前的对话保留在历史中。 "); break;
    case "context_compacted": notice("已压缩模型上下文，完整对话仍保留。 "); break;
    case "client_result":
      if (payload.output) notice(payload.output);
      // Also captures storage/command failures that have no agent run_end event.
      if (payload.status === "failed" && payload.error && next.items.at(-1)?.error !== payload.error) notice("操作失败", payload.error);
      break;
  }
  return next;
}
