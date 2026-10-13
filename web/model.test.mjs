import test from "node:test";
import assert from "node:assert/strict";
import { applyRecord, emptyModel } from "./src/model.ts";

test("replay merges streamed and complete messages and ignores duplicate cursors", () => {
  const records = [
    { seq: 1, run_id: "one", type: "run_start" },
    { seq: 2, run_id: "one", type: "message_added", payload: { message: { role: "user", content: "question" } } },
    { seq: 3, run_id: "one", type: "turn_start", payload: { turn: 1 } },
    { seq: 4, run_id: "one", type: "text_delta", payload: { turn: 1, text: "partial" } },
    { seq: 5, run_id: "one", type: "reasoning_delta", payload: { turn: 1, reasoning: "thinking" } },
    { seq: 6, run_id: "one", type: "message_added", payload: { turn: 1, message: { role: "assistant", content: "complete" } } },
  ];
  const model = records.reduce(applyRecord, emptyModel);
  assert.equal(model.items.length, 2);
  assert.equal(model.items[1].text, "complete");
  assert.equal(model.items[1].reasoning, "thinking");
  assert.strictEqual(applyRecord(model, records[5]), model);
  assert.deepEqual([...records, ...records].reduce(applyRecord, emptyModel), model);
});

test("compaction and reset preserve visible history and hide injected skill messages", () => {
  const records = [
    { seq: 1, type: "message_added", payload: { message: { role: "user", content: "original" } } },
    { seq: 2, type: "message_added", payload: { message: { role: "user", content: "skill body", skill: { name: "x" } } } },
    { seq: 3, type: "context_compacted" },
    { seq: 4, type: "session_reset" },
  ];
  const model = records.reduce(applyRecord, emptyModel);
  assert.equal(model.items.length, 3);
  assert.equal(model.items[0].text, "original");
  assert.equal(model.items.filter(item => item.role === "user").length, 1);
});

test("repairing an interrupted tool updates its original activity", () => {
  const records = [
    { seq: 1, run_id: "one", type: "turn_start", payload: { turn: 1 } },
    { seq: 2, run_id: "one", type: "tool_start", payload: { turn: 1, tool_call: { id: "read-1", name: "read", arguments: { path: "file" } } } },
    { seq: 3, type: "message_added", payload: { message: { role: "tool", tool_call_id: "read-1", content: "interrupted", is_error: true } } },
  ];
  const model = records.reduce(applyRecord, emptyModel);
  assert.equal(model.items[0].tools[0].status, "failed");
  assert.equal(model.items[0].tools[0].result, "interrupted");
});

test("SDK and application failure events produce one visible error", () => {
  const records = [
    { seq: 1, run_id: "one", type: "run_end", payload: { error: "provider failed", reason: "error" } },
    { seq: 2, type: "client_result", payload: { status: "failed", error: "provider failed" } },
  ];
  const model = records.reduce(applyRecord, emptyModel);
  assert.equal(model.items.length, 1);
});

test("read-only history recovers collaboration state without opening an Agent", () => {
  const collaboration = { mode: "plan", plan: { id: "plan", path: "/plans/plan.md", content: "# Plan" } };
  const model = applyRecord(emptyModel, { seq: 1, type: "plan_saved", payload: { collaboration } });
  assert.deepEqual(model.collaboration, collaboration);
  const reset = applyRecord(model, { seq: 2, type: "session_reset" });
  assert.deepEqual(reset.collaboration, { mode: "plan" });
});

test("plan approval displays the user action while retaining the SDK prompt in the log", () => {
  const model = [
    { seq: 1, type: "client_request", payload: { action: "execute" } },
    { seq: 2, type: "message_added", payload: { message: { role: "user", content: "internal approved-plan prompt" } } },
  ].reduce(applyRecord, emptyModel);
  assert.equal(model.items[0].text, "批准并执行当前计划");
});

test("reused tool call IDs do not replace an earlier run's results", () => {
  const records = [
    { seq: 1, run_id: "first", type: "tool_start", payload: { turn: 1, tool_call: { id: "read", name: "read", arguments: {} } } },
    { seq: 2, run_id: "first", type: "message_added", payload: { turn: 1, message: { role: "tool", tool_call_id: "read", content: "old result" } } },
    { seq: 3, run_id: "second", type: "tool_start", payload: { turn: 1, tool_call: { id: "read", name: "read", arguments: {} } } },
    { seq: 4, run_id: "second", type: "message_added", payload: { turn: 1, message: { role: "tool", tool_call_id: "read", content: "new result" } } },
  ];
  const model = records.reduce(applyRecord, emptyModel);
  assert.equal(model.items[0].tools[0].result, "old result");
  assert.equal(model.items[1].tools[0].result, "new result");
});
