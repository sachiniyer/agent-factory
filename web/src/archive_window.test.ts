import { test } from "node:test";
import assert from "node:assert/strict";
import { OptimisticSessions } from "./optimistic.js";
import { Liveness, type SessionData, type SnapshotCursor } from "./types.js";
import { REREAD_BATCH, aheadOfCursor, discoverHead, discoveryNeeded, reconcileWindow, rereadHeld } from "./archive_window.js";

function row(id: string, created: string, liveness: number = Liveness.Archived): SessionData {
  return { id, title: id, branch: id, liveness, created_at: created, worktree: { repo_path: "/p/a" } } as SessionData;
}
const killed = (r: SessionData) => ({ type: "session.killed", data: r }) as never;

// #6209: a by-id read (route lookup or page) that raced a kill must not resurrect it.
test("#6209: an event newer than the read's fence beats the loaded row, either order", () => {
  const model = new OptimisticSessions();
  model.reset([]);
  const fence = model.snapshotFence();
  const gone = row("gone", "2026-01-02T00:00:00Z");
  model.event(killed(gone)); // arrives while the read is in flight
  assert.deepEqual(model.adopt([gone, row("kept", "2026-01-01T00:00:00Z")], fence).map((r) => r.id), ["kept"]);
  assert.deepEqual(model.authoritativeRows().map((r) => r.id), ["kept"]);
  // a read started AFTER the kill may legitimately return the id again (re-created)
  assert.equal(model.adopt([gone], model.snapshotFence()).length, 1);
});

// #6207: a resync fetched before an adopt must fail its fence, not overwrite the page.
test("#6207: adopting a page advances the revision so an older snapshot is refused", () => {
  const model = new OptimisticSessions();
  model.reset([row("live", "2026-01-05T00:00:00Z", Liveness.Running)]);
  const resyncFence = model.snapshotFence();
  const pageFence = model.snapshotFence();
  assert.equal(model.adopt([row("p1", "2026-01-01T00:00:00Z")], pageFence).length, 1);
  assert.equal(model.snapshot([row("live", "2026-01-05T00:00:00Z", Liveness.Running)], resyncFence, true), false);
  assert.ok(model.authoritativeRows().some((r) => r.id === "p1"), "the adopted page survives");
  assert.equal(model.snapshot([row("live", "2026-01-05T00:00:00Z", Liveness.Running), row("p1", "2026-01-01T00:00:00Z")],
    model.snapshotFence(), true), true, "the retried resync, which holds the page, commits");
});

// #6208: no size above which reconciliation turns off.
test("#6208: a window of any size is re-read in bounded batches and deleted rows drop out", async () => {
  const n = REREAD_BATCH * 2 + 123;
  const ids = Array.from({ length: n }, (_, i) => `s${i}`);
  const deleted = new Set(["s3", `s${n - 1}`]);
  const sizes: number[] = [];
  const out = await rereadHeld(ids, async (batch) => {
    sizes.push(batch.length);
    return batch.filter((id) => !deleted.has(id)).map((id) => row(id, "2026-01-01T00:00:00Z"));
  });
  assert.deepEqual(sizes, [REREAD_BATCH, REREAD_BATCH, 123]);
  assert.equal(out.length, n - 2);
  assert.ok(!out.some((r) => deleted.has(r.id!)));
});

// #6206: rows archived while disconnected are found, behind an exhausted cursor and
// ahead of a partly consumed one.
test("#6206: head re-paging finds rows archived while away, for an exhausted and a partial cursor", async () => {
  const all = ["e", "d", "c", "b", "a"].map((id, i) => row(id, `2026-01-0${5 - i}T00:00:00Z`)); // newest first
  const page = (rows: SessionData[]) => async (before: SnapshotCursor | null, limit: number) => {
    const start = before ? rows.findIndex((r) => r.id === before.id) + 1 : 0;
    const slice = rows.slice(start, start + limit);
    const last = slice[slice.length - 1];
    return { sessions: slice, next: start + limit < rows.length && last ? { created_at: last.created_at!, id: last.id! } : null };
  };
  // exhausted: the daemon now has a row "f" the client never saw
  const withNew = [row("f", "2026-01-06T00:00:00Z"), ...all];
  const exhausted = (await discoverHead(null, async (b, l) => page(withNew)(b, Math.min(l, 2)), 100)).rows;
  assert.ok(exhausted.some((r) => r.id === "f"));
  // partial: the window ends at "c"; "f" sorts ahead of that boundary, "a" is beyond it
  const cursor = { created_at: "2026-01-03T00:00:00Z", id: "c" };
  const partial = (await discoverHead(cursor, async (b, l) => page(withNew)(b, Math.min(l, 2)), 100)).rows;
  assert.ok(partial.some((r) => r.id === "f"));
  assert.ok(!partial.some((r) => r.id === "a"), "paging stops once the boundary is crossed");
  assert.equal(aheadOfCursor(row("f", "2026-01-06T00:00:00Z"), cursor), true);
  assert.equal(aheadOfCursor(row("a", "2026-01-01T00:00:00Z"), cursor), false);
});

// The refinement: a live-connected resync issues no archive reads.
test("a live-connected resync with unchanged archive counts needs no by-id or head reads", () => {
  const summary = { total: 3, byProject: { "/p/a": 3 } };
  assert.equal(discoveryNeeded(false, summary, { total: 3, byProject: { "/p/a": 3 } }), false);
  assert.equal(discoveryNeeded(true, summary, summary), true, "the stream was not live since the last reconcile");
  assert.equal(discoveryNeeded(false, summary, { total: 4, byProject: { "/p/a": 4 } }), true, "the counts moved");
  assert.equal(discoveryNeeded(false, summary, { total: 3, byProject: { "/p/b": 3 } }), true, "a project's count moved");
});

test("reconcileWindow: a live-connected resync issues no by-id or page reads; a missed one issues both", async () => {
  let reads = 0;
  const held = [row("h1", "2026-01-02T00:00:00Z"), row("h2", "2026-01-01T00:00:00Z")];
  const base = {
    held, live: new Set<string | undefined>(), isArchived: (s: SessionData) => s.liveness === Liveness.Archived,
    archive: { total: 2, byProject: { "/p/a": 2 }, cursors: { "/p/a": null } as Record<string, SnapshotCursor | null> },
    summary: { total: 2, byProject: { "/p/a": 2 } },
    fetchById: async (ids: string[]) => { reads++; return held.filter((h) => ids.includes(h.id!)).slice(0, 1); },
    fetchPage: async () => { reads++; return { sessions: [row("new", "2026-01-03T00:00:00Z"), ...held], next: null }; },
  };
  assert.deepEqual((await reconcileWindow({ ...base, dirty: false })).rows.map((r) => r.id), ["h1", "h2"]);
  assert.equal(reads, 0, "no archive reads while the stream stayed live");
  const out = await reconcileWindow({ ...base, dirty: true });
  assert.ok(reads >= 2);
  assert.ok(out.rows.some((r) => r.id === "new"), "the row archived while away is found");
});

// Codex P2 (#6212): sub-millisecond instants order as the daemon orders them.
test("aheadOfCursor keeps the daemon's timestamp precision", () => {
  const cursor = { created_at: "2026-01-01T00:00:00.123100Z", id: "z" };
  assert.equal(aheadOfCursor(row("a", "2026-01-01T00:00:00.123900Z"), cursor), true, "later within the same millisecond");
  assert.equal(aheadOfCursor(row("z", "2026-01-01T00:00:00.123100Z"), cursor), false, "same instant and id is not ahead");
  assert.equal(aheadOfCursor(row("a", "2026-01-01T00:00:00.1230Z"), { created_at: "2026-01-01T00:00:00.123100Z", id: "a" }), false);
});

// Codex P2 (#6212): a discovery that runs out of pages is reported incomplete, and
// the page budget scales with the archive instead of stopping at a constant.
test("discoverHead reports an exhausted budget; reconcileWindow sizes the budget from the total", async () => {
  const many = Array.from({ length: 10 }, (_, n) => row(`r${n}`, `2026-01-${String(20 - n).padStart(2, "0")}T00:00:00Z`));
  const fetchPage = async (before: SnapshotCursor | null, limit: number) => {
    const start = before === null ? 0 : many.findIndex((r) => r.id === before.id) + 1;
    const sessions = many.slice(start, start + Math.min(limit, 2));
    const last = sessions[sessions.length - 1];
    return { sessions, next: start + sessions.length >= many.length ? null : { created_at: last.created_at!, id: last.id! } };
  };
  const cut = await discoverHead(null, fetchPage, 2);
  assert.equal(cut.complete, false);
  assert.equal(cut.rows.length, 4);
  assert.equal((await discoverHead(null, fetchPage, 10)).complete, true);
});
