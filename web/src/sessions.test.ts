// Tests for the pure session-list reducer (#1592 Phase 5 PR3, id-keyed in PR5):
// how Snapshot + /v1/events deltas fold into the rail. These pin the live-update
// semantics the play-test exercises — created/updated upsert, killed removes,
// archived/restored ask for a resync — without a daemon or a DOM, and pin the
// PR5 structural fix: the list is keyed by the STABLE id, so a cross-repo
// duplicate title disambiguates correctly.

import { test } from "node:test";
import assert from "node:assert/strict";

import { applyEvent, clampActiveTab, pickSelection, removeSession, sessionKey, upsertSession } from "./sessions.js";
import { orderedSessions } from "./ui.js";
import { Liveness, type SessionData, type WireEvent } from "./types.js";

function sess(title: string, over: Partial<SessionData> = {}): SessionData {
  return { title, branch: "b", ...over };
}

test("sessionKey uses the id when present, the title as fallback", () => {
  assert.equal(sessionKey({ id: "abc", title: "x" }), "id abc");
  assert.equal(sessionKey({ id: "", title: "x" }), "title x");
  assert.equal(sessionKey({ title: "x" }), "title x");
});

test("upsert inserts a new id and replaces an existing one without mutating", () => {
  const a = [sess("x", { id: "id-x", branch: "old" })];
  const inserted = upsertSession(a, sess("y", { id: "id-y" }));
  assert.deepEqual(inserted.map((s) => s.title), ["x", "y"]);
  assert.equal(a.length, 1, "input array is not mutated");

  const replaced = upsertSession(inserted, sess("x", { id: "id-x", branch: "new" }));
  assert.equal(replaced.find((s) => s.id === "id-x")?.branch, "new");
  assert.equal(replaced.length, 2, "replace does not grow the list");
});

test("upsert keys by id, so a renamed session (same id) replaces in place", () => {
  const list = [sess("old-name", { id: "id-1" })];
  const renamed = upsertSession(list, sess("new-name", { id: "id-1" }));
  assert.equal(renamed.length, 1, "same id → replace, not append");
  assert.equal(renamed[0]?.title, "new-name");
});

test("remove drops the matching id only", () => {
  const list = [sess("x", { id: "id-x" }), sess("y", { id: "id-y" })];
  assert.deepEqual(removeSession(list, { id: "id-x", title: "x" }).map((s) => s.title), ["y"]);
  assert.deepEqual(
    removeSession(list, { id: "absent", title: "x" }).map((s) => s.title),
    ["x", "y"],
    "id-keyed remove ignores a title match with a different id",
  );
});

test("cross-repo duplicate title: a kill event removes exactly the right row by id", () => {
  // The known af gotcha: two sessions share the title "feature" in different
  // repos. Keyed by title (the pre-PR5 bug) a kill would remove BOTH; keyed by
  // the stable id it removes exactly the one the daemon killed.
  const list = [
    sess("feature", { id: "id-repoA", worktree: { repo_path: "/repos/a" } }),
    sess("feature", { id: "id-repoB", worktree: { repo_path: "/repos/b" } }),
  ];
  const r = applyEvent(list, { type: "session.killed", data: { id: "id-repoA", title: "feature", branch: "" } });
  assert.equal(r.needsResync, false);
  assert.deepEqual(r.sessions.map((s) => s.id), ["id-repoB"], "only the killed id is removed");
});

test("applyEvent: created/updated upsert in place with no resync", () => {
  let list: SessionData[] = [];
  const created: WireEvent = { type: "session.created", data: sess("x", { id: "id-x", liveness: Liveness.Running }) };
  let r = applyEvent(list, created);
  assert.equal(r.needsResync, false);
  assert.deepEqual(r.sessions.map((s) => s.title), ["x"]);
  list = r.sessions;

  const updated: WireEvent = { type: "session.updated", data: sess("x", { id: "id-x", liveness: Liveness.Ready }) };
  r = applyEvent(list, updated);
  assert.equal(r.needsResync, false);
  assert.equal(r.sessions.length, 1, "same id updates in place");
  assert.equal(r.sessions[0]?.liveness, Liveness.Ready, "status transition applied in place");
});

test("applyEvent: killed removes by id with no resync", () => {
  const list = [sess("x", { id: "id-x" }), sess("y", { id: "id-y" })];
  const r = applyEvent(list, { type: "session.killed", data: { id: "id-x", title: "x", branch: "" } });
  assert.equal(r.needsResync, false);
  assert.deepEqual(r.sessions.map((s) => s.title), ["y"]);
});

test("applyEvent: an id-less killed event falls back to the title", () => {
  // Legacy/disk-only records carry no id; the daemon stamps the title fallback, so
  // the reducer keys by title in that case (matching the daemon's own fallback).
  const list = [sess("x", { id: "" }), sess("y", { id: "id-y" })];
  const r = applyEvent(list, { type: "session.killed", data: { title: "x", branch: "" } });
  assert.deepEqual(r.sessions.map((s) => s.title), ["y"]);
});

test("applyEvent: archived/restored ask for a resync and leave the list untouched", () => {
  const list = [sess("x", { id: "id-x" })];
  for (const type of ["session.archived", "session.restored"] as const) {
    const r = applyEvent(list, { type, data: { id: "id-x", title: "x", branch: "" } });
    assert.equal(r.needsResync, true, `${type} → resync`);
    assert.equal(r.sessions, list, "list reference unchanged pending the refetch");
  }
});

test("applyEvent: task.* and dataless events are no-ops", () => {
  const list = [sess("x", { id: "id-x" })];
  assert.equal(applyEvent(list, { type: "task.created" }).sessions, list);
  assert.equal(applyEvent(list, { type: "session.created" }).needsResync, false);
  assert.deepEqual(applyEvent(list, { type: "session.created" }).sessions, list);
});

test("pickSelection keeps a still-present selection by id, else clears it", () => {
  const list = [sess("x", { id: "id-x" }), sess("y", { id: "id-y" })];
  assert.equal(pickSelection(list, "id-y"), "id-y");
  assert.equal(pickSelection(list, "gone"), null);
  assert.equal(pickSelection(list, null), null);
});

test("clampActiveTab keeps the active tab in range as the live tab list changes", () => {
  const threeTabs = sess("x", {
    id: "id-x",
    tabs: [
      { name: "agent", kind: 0 },
      { name: "shell", kind: 1 },
      { name: "shell-2", kind: 1 },
    ],
  });
  const oneTab = sess("x", { id: "id-x", tabs: [{ name: "agent", kind: 0 }] });

  // In range → unchanged.
  assert.equal(clampActiveTab([threeTabs], "id-x", 2), 2);
  // The list shrank under the client (a tab closed out-of-band) → clamp to the last.
  assert.equal(clampActiveTab([oneTab], "id-x", 2), 0, "vanished tab falls back to the agent tab");
  // No selection, an unknown selection, or a tab-less record → the agent tab (0).
  assert.equal(clampActiveTab([threeTabs], null, 2), 0);
  assert.equal(clampActiveTab([threeTabs], "gone", 2), 0);
  assert.equal(clampActiveTab([sess("y", { id: "id-y" })], "id-y", 3), 0, "no tabs → one implicit agent tab");
  // A negative index floors at 0.
  assert.equal(clampActiveTab([threeTabs], "id-x", -1), 0);
});

test("orderedSessions puts live rows before archived, then by created_at", () => {
  const list = [
    sess("late", { created_at: "2026-01-02T00:00:00Z" }),
    sess("arch", { liveness: Liveness.Archived, created_at: "2026-01-01T00:00:00Z" }),
    sess("early", { created_at: "2026-01-01T00:00:00Z" }),
  ];
  assert.deepEqual(orderedSessions(list).map((s) => s.title), ["early", "late", "arch"]);
});
