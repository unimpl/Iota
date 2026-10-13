import type { Collaboration, SessionRecord } from "./types";

export interface ToolActivity {
  id: string;
  name: string;
  arguments: string;
  result: string;
  status: "running" | "completed" | "failed" | "interrupted";
}

export interface ChatItem {
  id: string;
  role: "user" | "assistant" | "notice";
  text: string;
  reasoning: string;
  tools: ToolActivity[];
  complete: boolean;
  error?: string;
}

export interface ChatModel { items: ChatItem[]; sequence: number; run: string; turn: number; collaboration?: Collaboration; action?: string }
export const emptyModel: ChatModel = { items: [], sequence: 0, run: "", turn: 0 };

// Build visible conversation from the immutable log, never compacted model context.
// Full assistant messages replace their streamed draft instead of duplicating it.
export function applyRecord(model: ChatModel, record: SessionRecord): ChatModel {
  if (record.seq <= model.sequence) return model;
  const payload = record.payload ?? {};
  const next = { ...model, items: model.items, sequence: record.seq };
  if (record.run_id) next.run = record.run_id;
  if (record.type === "run_start") next.turn = 0;
  if (record.type === "turn_start") next.turn = payload.turn ?? next.turn + 1;
  if (record.type === "client_request") next.action = payload.action;
  if (payload.collaboration) next.collaboration = payload.collaboration;
  if (record.type === "session_reset") next.collaboration = { mode: model.collaboration?.mode ?? "default" };
  const turn = payload.turn ?? next.turn;
  const assistantID = `assistant-${next.run}-${turn}`;
  const updateAssistant = (update: (item: ChatItem) => ChatItem) => {
    const index = next.items.findIndex(item => item.id === assistantID);
    const item = index < 0 ? { id: assistantID, role: "assistant" as const, text: "", reasoning: "", tools: [], complete: false } : next.items[index];
    next.items = [...next.items];
    if (index < 0) next.items.push(update(item));
    else next.items[index] = update(item);
  };
  const notice = (text: string, error?: string) => {
    next.items = [...next.items, { id: `notice-${record.seq}`, role: "notice", text, reasoning: "", tools: [], complete: true, error }];
  };
  switch (record.type) {
    case "message_added": {
      const message = payload.message;
      if (!message || message.skill || message.context_summary || message.runtime_context) break;
      if (message.role === "user") {
        next.items = [...next.items, { id: `user-${record.seq}`, role: "user", text: next.action === "execute" ? "批准并执行当前计划" : message.content ?? "", reasoning: "", tools: [], complete: true }];
        next.action = undefined;
      } else if (message.role === "assistant") {
        updateAssistant(item => ({ ...item, text: message.content ?? "", complete: true }));
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
