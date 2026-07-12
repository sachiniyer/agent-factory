// Tests for the pure session-list reducer (#1592 Phase 5 PR3): how Snapshot +
// /v1/events deltas fold into the rail. These pin the live-update semantics the
// play-test exercises — created/updated upsert, killed removes, archived/restored
// ask for a resync — without a daemon or a DOM.

import { test } from "node:test";
import assert from "node:assert/strict";

import { applyEvent, pickSelection, removeSession, upsertSession } from "./sessions.js";
import { orderedSessions } from "./ui.js";
import { Liveness, type SessionData, type WireEvent } from "./types.js";

function sess(title: string, over: Partial<SessionData> = {}): SessionData {
  return { title, branch: "b", ...over };
}

test("upsert inserts a new title and replaces an existing one without mutating", () => {
  const a = [sess("x", { branch: "old" })];
  const inserted = upsertSession(a, sess("y"));
  assert.deepEqual(inserted.map((s) => s.title), ["x", "y"]);
  assert.equal(a.length, 1, "input array is not mutated");

  const replaced = upsertSession(inserted, sess("x", { branch: "new" }));
  assert.equal(replaced.find((s) => s.title === "x")?.branch, "new");
  assert.equal(replaced.length, 2, "replace does not grow the list");
});

test("remove drops the matching title only", () => {
  const list = [sess("x"), sess("y")];
  assert.deepEqual(removeSession(list, "x").map((s) => s.title), ["y"]);
  assert.deepEqual(removeSession(list, "absent").map((s) => s.title), ["x", "y"]);
});

test("applyEvent: created/updated upsert in place with no resync", () => {
  let list: SessionData[] = [];
  const created: WireEvent = { type: "session.created", data: sess("x", { liveness: Liveness.Running }) };
  let r = applyEvent(list, created);
  assert.equal(r.needsResync, false);
  assert.deepEqual(r.sessions.map((s) => s.title), ["x"]);
  list = r.sessions;

  const updated: WireEvent = { type: "session.updated", data: sess("x", { liveness: Liveness.Ready }) };
  r = applyEvent(list, updated);
  assert.equal(r.needsResync, false);
  assert.equal(r.sessions[0]?.liveness, Liveness.Ready, "status transition applied in place");
});

test("applyEvent: killed removes by title with no resync", () => {
  const list = [sess("x"), sess("y")];
  const r = applyEvent(list, { type: "session.killed", data: sess("x") });
  assert.equal(r.needsResync, false);
  assert.deepEqual(r.sessions.map((s) => s.title), ["y"]);
});

test("applyEvent: archived/restored ask for a resync and leave the list untouched", () => {
  const list = [sess("x")];
  for (const type of ["session.archived", "session.restored"] as const) {
    const r = applyEvent(list, { type, data: sess("x") });
    assert.equal(r.needsResync, true, `${type} → resync`);
    assert.equal(r.sessions, list, "list reference unchanged pending the refetch");
  }
});

test("applyEvent: task.* and dataless events are no-ops", () => {
  const list = [sess("x")];
  assert.equal(applyEvent(list, { type: "task.created" }).sessions, list);
  assert.equal(applyEvent(list, { type: "session.created" }).needsResync, false);
  assert.deepEqual(applyEvent(list, { type: "session.created" }).sessions, list);
});

test("pickSelection keeps a still-present selection, else clears it", () => {
  const list = [sess("x"), sess("y")];
  assert.equal(pickSelection(list, "y"), "y");
  assert.equal(pickSelection(list, "gone"), null);
  assert.equal(pickSelection(list, null), null);
});

test("orderedSessions puts live rows before archived, then by created_at", () => {
  const list = [
    sess("late", { created_at: "2026-01-02T00:00:00Z" }),
    sess("arch", { liveness: Liveness.Archived, created_at: "2026-01-01T00:00:00Z" }),
    sess("early", { created_at: "2026-01-01T00:00:00Z" }),
  ];
  assert.deepEqual(orderedSessions(list).map((s) => s.title), ["early", "late", "arch"]);
});
