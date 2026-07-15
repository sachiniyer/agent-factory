// Tests for the pure session-list reducer (#1592 Phase 5 PR3, id-keyed in PR5):
// how Snapshot + /v1/events deltas fold into the rail. These pin the live-update
// semantics the play-test exercises — created/updated upsert, killed removes,
// archived/restored ask for a resync — without a daemon or a DOM, and pin the
// PR5 structural fix: the list is keyed by the STABLE id, so a cross-repo
// duplicate title disambiguates correctly.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  applyEvent,
  clampActiveTab,
  pickSelection,
  removeSession,
  sessionKey,
  tabToKeepOnClose,
  upsertSession,
} from "./sessions.js";
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

test("orderedSessions: live rows oldest-first, archived group last newest-first (#1605/#1674)", () => {
  const list = [
    sess("live-late", { created_at: "2026-01-02T00:00:00Z" }),
    sess("arch-old", { liveness: Liveness.Archived, created_at: "2026-01-01T00:00:00Z" }),
    sess("live-early", { created_at: "2026-01-01T00:00:00Z" }),
    sess("arch-new", { liveness: Liveness.Archived, created_at: "2026-01-03T00:00:00Z" }),
  ];
  // Live rows keep the projection's oldest-first order; the archived group sorts
  // NEWEST-created first, mirroring the TUI sidebar (partitionByArchived, #1605) —
  // the web previously sorted archived oldest-first too (#1674 PR3 review).
  assert.deepEqual(orderedSessions(list).map((s) => s.title), [
    "live-early",
    "live-late",
    "arch-new",
    "arch-old",
  ]);
});

test("orderedSessions: title breaks a created_at tie in the archived group (total order)", () => {
  const list = [
    sess("arch-b", { liveness: Liveness.Archived, created_at: "2026-01-01T00:00:00Z" }),
    sess("arch-a", { liveness: Liveness.Archived, created_at: "2026-01-01T00:00:00Z" }),
  ];
  assert.deepEqual(orderedSessions(list).map((s) => s.title), ["arch-a", "arch-b"]);
});

// --- tabToKeepOnClose: a pane follows a TAB, never a slot -------------------
//
// The post-merge Codex finding on #1815. closeSessionTab used to re-point the pane
// by subtracting the close's shift from the active index. That arithmetic is only
// valid if the roster it was computed against is the roster it lands on — and #1815
// made the opposite reachable, by delivering another client's tab changes live and
// mid-flight. These pin the decision against both rosters.

test("closing a LOWER tab keeps the active tab, by identity", () => {
  // [Agent, A, B] with B active; A closes. B survives, so the pane follows B —
  // wherever the shrunk roster puts it.
  assert.equal(tabToKeepOnClose(["id-agent", "id-a", "id-b"], 1, 2), "id-b");
});

test("closing a HIGHER tab keeps the active tab, by identity", () => {
  assert.equal(tabToKeepOnClose(["id-agent", "id-a", "id-b"], 2, 1), "id-a");
});

test("closing the ACTIVE tab falls back to its LEFT NEIGHBOUR in the pre-close list", () => {
  // Nothing to follow: the tab the pane was on is the one going away. The neighbour
  // is named from the pre-close roster, where "left of the closed tab" still means
  // something — the post-close list no longer contains the closed tab to count from.
  assert.equal(tabToKeepOnClose(["id-agent", "id-a", "id-b"], 2, 2), "id-a");
});

test("closing the tab right above Agent falls back to Agent", () => {
  assert.equal(tabToKeepOnClose(["id-agent", "id-a"], 1, 1), "id-agent");
});

test("a concurrent out-of-band close cannot re-point the pane to a neighbour (#1815 follow-up)", () => {
  // Codex's exact scenario: tabs [Agent,A,B,C,D,E,F] with E active; this window
  // closes D while ANOTHER client closes B first. The old code computed next from
  // the stale ordinal (cur=5 → 4) and applied it to the post-close roster, landing on
  // F — a tab the user never picked, tearing down E's pane to show its neighbour.
  const before = ["id-agent", "id-a", "id-b", "id-c", "id-d", "id-e", "id-f"];
  const keep = tabToKeepOnClose(before, 4, 5);
  assert.equal(keep, "id-e");

  // The roster after BOTH closes (B out-of-band, then D). Resolving the identity —
  // what closeSessionTab does with this value — follows E to its real new index.
  const after = ["id-agent", "id-a", "id-c", "id-e", "id-f"];
  assert.equal(after.indexOf(keep), 3, "the pane must follow E, not land on the old ordinal");
  assert.notEqual(after.indexOf(keep), 4, "index 4 is F — the neighbour the ordinal arithmetic picked");
});

test("a kept tab closed out-of-band mid-flight resolves to -1, not a guess", () => {
  // The caller treats -1 as "leave the pane where the roster remap already settled
  // it": re-pointing from a dead identity could only misroute.
  const keep = tabToKeepOnClose(["id-agent", "id-a", "id-b"], 1, 2);
  assert.equal(["id-agent", "id-a"].indexOf(keep), -1);
});

test("an out-of-range active index yields no tab to follow", () => {
  assert.equal(tabToKeepOnClose(["id-agent"], 1, 5), "");
  // Identities are never empty (ui.tabIdentity synthesizes a kind:name fallback), so
  // "" can't accidentally match a real tab when the caller resolves it.
  assert.equal(["id-agent", "id-a"].indexOf(""), -1);
});
