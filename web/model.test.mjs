import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { applyBatch, applyRecord, emptyModel, fromHistory, mergeHistory } from "./src/model.ts";

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

test("server snapshot matches event replay and resumes an unfinished reply without gaps", () => {
  const fixture = JSON.parse(readFileSync(new URL("../internal/server/testdata/history.json", import.meta.url), "utf8"));
  const normalize = model => JSON.parse(JSON.stringify(model));
  const replay = fixture.records.reduce(applyRecord, emptyModel);
  const snapshot = fromHistory(fixture.snapshot);
  assert.deepEqual(normalize(snapshot), normalize(replay));
  const live = [
    fixture.records.at(-1), // A repeated cursor must be ignored.
    { seq: 34, run_id: "third", type: "reasoning_delta", payload: { reasoning: " continued" } },
    { seq: 35, run_id: "third", type: "text_delta", payload: { text: " continued" } },
    { seq: 36, run_id: "third", type: "tool_end", payload: { tool_call: { id: "read", name: "read", arguments: {} }, tool_result: "latest result" } },
    { seq: 37, run_id: "third", type: "message_added", payload: { message: { role: "assistant", content: "finished" } } },
  ];
  const resumed = live.reduce(applyRecord, snapshot);
  assert.deepEqual(normalize(resumed), normalize(live.reduce(applyRecord, replay)));
  assert.equal(resumed.items.length, snapshot.items.length);
  assert.equal(resumed.items.at(-1).text, "finished");
  assert.equal(resumed.items.at(-1).reasoning, "still thinking continued");
  assert.equal(resumed.items.at(-1).tools[0].result, "latest result");
  assert.equal(resumed.items[1].tools[0].result, "old result");
});

test("merged batches preserve replay, deduplicate frames and advance omitted event cursors", () => {
  const fixture = JSON.parse(readFileSync(new URL("../internal/server/testdata/event-batches.json", import.meta.url), "utf8"));
  // Merged drafts carry the last fragment's seq; visible content and ordering
  // are identical even though the first retained fragment has a different seq.
  const normalize = model => JSON.parse(JSON.stringify({ ...model, items: model.items.map(({ seq, ...item }) => item) }));
  const replay = fixture.records.reduce(applyRecord, emptyModel);
  const batched = fixture.batches.reduce(applyBatch, emptyModel);
  assert.deepEqual(normalize(batched), normalize(replay));
  assert.equal(batched.items[0].reasoning, "thinking");
  assert.equal(batched.items[0].text, "hello world");
  assert.equal(batched.items[0].tools[0].result, "result");
  assert.strictEqual(applyBatch(batched, fixture.batches.at(-1)), batched);
  const checkpoint = applyBatch(batched, { records: [], cursor: 20 });
  assert.equal(checkpoint.sequence, 20);
  assert.strictEqual(checkpoint.items, batched.items);
  assert.strictEqual(applyBatch(checkpoint, fixture.batches[2]), checkpoint);
});

test("older pages merge in log order without overwriting live replies or the stream checkpoint", () => {
  const fixture = JSON.parse(readFileSync(new URL("../internal/server/testdata/history.json", import.meta.url), "utf8"));
  const latest = { ...fixture.snapshot, items: fixture.snapshot.items.filter(item => item.run_id === "third"), loaded_runs: ["third"] };
  const live = applyRecord(fromHistory(latest), { seq: 34, run_id: "third", type: "text_delta", payload: { text: " live" } });
  const old = { ...fixture.snapshot, items: fixture.snapshot.items.filter(item => item.run_id !== "third"), loaded_runs: ["first", "second"] };
  const merged = mergeHistory(live, old);
  assert.equal(merged.sequence, 34);
  assert.equal(merged.run, "third");
  assert.equal(merged.turn, live.turn);
  assert.deepEqual(merged.collaboration, live.collaboration);
  assert.equal(merged.items[0].text, "question");
  assert.equal(merged.items.at(-1).text, "partial live");
  assert.equal(merged.runs.at(-1).end_seq, 34);
  assert.deepEqual(mergeHistory(merged, old), merged);
  assert.deepEqual(mergeHistory(merged, fixture.snapshot).items, merged.items);
});

test("loading a page with a newer index never suppresses the upcoming live run", () => {
  const base = fromHistory({ items: [], runs: [], loaded_runs: [], cursor: 1, run: "", turn: 0 });
  const future = { id: "future", start_seq: 2, end_seq: 5, started_at: "", title: "question", preview: "answer", status: "completed" };
  const indexed = mergeHistory(base, { items: [], runs: [future], loaded_runs: [], cursor: 5, run: "future", turn: 1 });
  assert.equal(indexed.sequence, 1);
  const live = applyBatch(indexed, { cursor: 3, records: [{ seq: 2, run_id: "future", type: "run_start" }, { seq: 3, run_id: "future", type: "message_added", payload: { message: { role: "user", content: "question" } } }] });
  assert.deepEqual(live.loadedRuns, ["future"]);
  assert.equal(live.items[0].text, "question");
});
