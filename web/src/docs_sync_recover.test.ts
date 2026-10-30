import assert from "node:assert/strict";
import { test } from "node:test";
import { type Draft, type Event, type Model, initialModel, statusOf, step } from "./docs_sync_model.js";

function run(m: Model, ...events: Event[]): { m: Model; fx: ReturnType<typeof step>[1] } {
  let fx: ReturnType<typeof step>[1] = [];
  for (const e of events) [m, fx] = step(m, e);
  return { m, fx };
}

const open = (doc: { text: string; commit: string }, draft: Draft | null = null, ...between: Event[]): ReturnType<typeof run> =>
  run(initialModel(), { t: "load_ok", doc }, ...between, { t: "draft_read", draft });

const loaded = (): Model => open({ text: "v1", commit: "c1" }).m;

test("a final save that fails keeps the buffer in storage and keeps retrying", () => {
  // The user leaves with an unsaved edit; the final save goes out...
  let r = run(loaded(), { t: "edit", text: "v1 edited" }, { t: "close" });
  assert.deepEqual(r.fx.filter((f) => f.t === "save").map((f) => (f as { final: boolean }).final), [true]);
  assert.ok(r.fx.some((f) => f.t === "stash") || r.m.stashed !== null, "the edit is in storage before the save is sent");
  // ...and fails (a 5xx, or the network).
  r = run(r.m, { t: "save_fail", error: "503 service unavailable" });
  assert.equal(r.m.stashed !== null, true, "the failed save does not clear the stored edit");
  assert.ok(r.fx.some((f) => f.t === "arm"), "it retries while the page is alive, though the tab is closed");
  // The retry fires and this time lands.
  r = run(r.m, { t: "timer" });
  assert.deepEqual(r.fx.filter((f) => f.t === "save").map((f) => (f as { text: string }).text), ["v1 edited"]);
  r = run(r.m, { t: "save_ok", result: { kind: "saved", text: "v1 edited", commit: "c2", merged: false } });
  assert.equal(r.m.stashed, null, "storage is cleared once a save has landed");
  assert.ok(r.fx.some((f) => f.t === "unstash"));
});

test("a tab that opens on stored unsent edits shows them as recovered and saves them again", () => {
  const draft = { buffer: "v1 typed before the page closed", base: "v1", theirs: "", head: "", kind: "unsent" as const };
  const r = open({ text: "v1", commit: "c1" }, draft);
  assert.equal(r.m.buffer, "v1 typed before the page closed");
  assert.equal(statusOf(r.m).status, "recovered");
  assert.ok(r.fx.some((f) => f.t === "arm"), "autosave is armed");
});

test("recovered edits are rebased onto what the daemon holds now", () => {
  const draft = { buffer: "a2\nb\nc", base: "a\nb\nc", theirs: "", head: "", kind: "unsent" as const };
  const r = open({ text: "a\nb\nC", commit: "c9" }, draft);
  assert.equal(r.m.buffer, "a2\nb\nC", "the agent's edit and the recovered one both survive");
  assert.equal(statusOf(r.m).status, "recovered");
});

test("recovered edits that collide with the agent's open a conflict instead of overwriting it", () => {
  const draft = { buffer: "mine", base: "base", theirs: "", head: "", kind: "unsent" as const };
  const r = open({ text: "theirs", commit: "c9" }, draft);
  assert.equal(statusOf(r.m).status, "conflict");
  assert.equal(r.m.buffer, "mine");
});

test("a conflict whose head is our own save with a lost reply is not an agent conflict", () => {
  let r = run(loaded(), { t: "edit", text: "v1 a" }, { t: "timer" });
  r = run(r.m, { t: "save_fail", error: "network error: reply lost" });
  r = run(r.m, { t: "edit", text: "v1 b" }, { t: "timer" });
  r = run(r.m, { t: "save_conflict", result: { kind: "conflict", ours: "v1 b", theirs: "v1 a", base: "v1", head: "c2" } });
  assert.equal(statusOf(r.m).status, "pending", "no banner: the head is our own earlier text");
  assert.equal(r.m.base, "c2");
});

test("a reconnect or remote change while the draft is being read cannot start a reload before the restore", () => {
  const draft = { buffer: "my unresolved text", base: "v0", theirs: "agent text", head: "c1", kind: "conflict" as const };
  // The load returned; before storage is read, the stream reconnects and the agent commits.
  const r = open({ text: "agent text 2", commit: "c2" }, draft, { t: "reconnect" }, { t: "remote", commit: "c3" });
  assert.equal(r.m.phase, "conflict", "the draft is restored, not overwritten by a reload");
  assert.equal(r.m.buffer, "my unresolved text");
  assert.equal(r.fx.some((f) => f.t === "load"), false, "no reload is started in the restoring phase");
  assert.equal(r.m.stale, true, "the change is remembered and drained after the conflict is resolved");
});

test("the restoring phase takes no edits and arms nothing", () => {
  let r = run(initialModel(), { t: "load_ok", doc: { text: "v1", commit: "c1" } });
  assert.equal(r.m.phase, "restoring");
  r = run(r.m, { t: "edit", text: "typed too early" });
  assert.equal(r.m.buffer, "v1");
  assert.equal(r.fx.length, 0);
});
