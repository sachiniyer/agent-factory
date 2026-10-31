import test from "node:test";
import assert from "node:assert/strict";
import { archiveRepaintNeeded } from "./ui.js";
import { archiveHasMore, emptyArchive, type ArchiveState } from "./archive.js";

// Mirrors loadArchivedPage's store updates: loading on, sessions applied while still
// loading, then ONLY the archive state changes (loading off, cursor stored).
test("an archive-only transition repaints the rail without another change", () => {
  const loaded: ArchiveState = { ...emptyArchive(), lazy: true, total: 120, byProject: { "/r": 120 } };
  const loading: ArchiveState = { ...loaded, loading: true };
  const done: ArchiveState = { ...loading, loading: false, cursors: { "/r": { created_at: "2026-01-01T00:00:00Z", id: "a" } } };
  const failed: ArchiveState = { ...loading, loading: false, error: "boom" };
  const retrying: ArchiveState = { ...failed, loading: true, error: undefined };

  let last: ArchiveState | undefined = loaded;
  for (const next of [loading, done, failed, retrying]) {
    assert.equal(archiveRepaintNeeded(last, { archive: next }), true);
    last = next;
  }
  assert.equal(archiveRepaintNeeded(last, { archive: last }), false, "an unrelated update must not repaint");
  assert.equal(done.loading, false);
  assert.equal(archiveHasMore(done, "/r"), true, "the footer leaves Loading… for the next-page control");
});
