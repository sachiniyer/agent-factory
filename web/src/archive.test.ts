// The lazy archive (#6178): what the rail knows about archived sessions it has not
// loaded, and the rules that keep a live-only startup snapshot from changing what
// the archived figures, the empty-project notice and a resync show.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  archiveFromSnapshot,
  archiveHasMore,
  archiveNeedsFirstPage,
  archivedCount,
  emptyArchive,
  railKindCounts,
} from "./archive.js";
import { OptimisticSessions } from "./optimistic.js";
import { Liveness, type SessionData } from "./types.js";

function row(id: string, liveness: number, repo = "/p/a"): SessionData {
  return { id, title: id, branch: id, liveness, worktree: { repo_path: repo } } as SessionData;
}

const lazy = () => archiveFromSnapshot(emptyArchive(), { total: 120, byProject: { "/p/a": 100, "/p/b": 20 } });

test("a daemon without the lazy shape leaves the archive logic inert", () => {
  const a = archiveFromSnapshot(emptyArchive(), undefined);
  assert.equal(a.lazy, false);
  const scoped = [row("x", Liveness.Archived), row("y", Liveness.Running)];
  assert.equal(archivedCount(a, "/p/a", scoped), 1, "counts the rows it has, as before");
  assert.equal(archiveHasMore(a, "/p/a"), false);
  assert.equal(archiveNeedsFirstPage(a, "/p/a"), false);
});

test("the archived figure is the daemon's count, not the rows loaded so far", () => {
  const a = lazy();
  const scoped = [row("x", Liveness.Running)];
  assert.equal(archivedCount(a, "/p/a", scoped), 100);
  assert.equal(railKindCounts(a, "/p/a", scoped).archived, 100);
  assert.equal(railKindCounts(a, "/p/a", scoped).working, 1, "the live row still counts under its own kind");
  assert.equal(archivedCount(a, null, scoped), 120, "no project: the daemon total");
});

test("rows loaded ahead of the next resync are never counted below what is on screen", () => {
  const a = archiveFromSnapshot(emptyArchive(), { total: 1, byProject: { "/p/a": 1 } });
  const scoped = [row("x", Liveness.Archived), row("y", Liveness.Archived), row("z", Liveness.Archived)];
  assert.equal(archivedCount(a, "/p/a", scoped), 3);
});

test("a project's first page is wanted once, only when it has an archive, and an error parks it", () => {
  const a = lazy();
  assert.equal(archiveNeedsFirstPage(a, "/p/a"), true);
  assert.equal(archiveNeedsFirstPage(a, "/p/none"), false, "nothing archived there");
  assert.equal(archiveNeedsFirstPage({ ...a, loading: true }, "/p/a"), false);
  assert.equal(archiveNeedsFirstPage({ ...a, error: "boom" }, "/p/a"), false, "no automatic retry loop");
  assert.equal(archiveNeedsFirstPage({ ...a, cursors: { "/p/a": null } }, "/p/a"), false, "fully loaded");
});

test("a resync keeps the page cursors and loading flag while the counts refresh", () => {
  const cursor = { created_at: "2026-01-01T00:00:00Z", id: "k" };
  const before = { ...lazy(), cursors: { "/p/a": cursor }, loading: true };
  const after = archiveFromSnapshot(before, { total: 119, byProject: { "/p/a": 99 } });
  assert.deepEqual(after.cursors, { "/p/a": cursor });
  assert.equal(after.loading, true);
  assert.equal(after.total, 119);
  assert.equal(archiveHasMore(after, "/p/a"), true);
  assert.equal(archiveHasMore({ ...after, cursors: { "/p/a": null } }, "/p/a"), false);
});

test("adopted archived rows survive a live-only snapshot only when the caller folds them back in", () => {
  const model = new OptimisticSessions();
  model.reset([row("live", Liveness.Running)]);
  model.adopt([row("old", Liveness.Archived)], model.snapshotFence());
  assert.deepEqual(model.authoritativeRows().map((s) => s.id), ["live", "old"]);
  model.adopt([row("old", Liveness.Archived)], model.snapshotFence());
  assert.equal(model.authoritativeRows().length, 2, "adopting a known row replaces it in place");
});

test("an archive request settles on the row leaving a live-only snapshot", () => {
  const model = new OptimisticSessions();
  const target = row("t", Liveness.Running);
  model.reset([target]);
  const ticket = model.begin("archive", target);
  assert.ok(ticket);
  assert.equal(model.succeed(ticket!), true);
  assert.equal(model.isCurrent(ticket!), true, "acknowledged but unconfirmed: waiting on evidence");
  // A full snapshot would show the row archived; a live-only one simply omits it.
  assert.equal(model.snapshot([], model.snapshotFence(), true), true);
  assert.equal(model.isCurrent(ticket!), false, "the live-only snapshot confirmed it");
});
