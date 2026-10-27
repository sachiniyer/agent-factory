// Surface-parity guard for the web's hand-mirror of the daemon's SnapshotResponse
// (#4742). The web's types.ts is a HAND-MIRROR of daemon/snapshot.go, pinned by its
// own header comment to "match the Go struct exactly" — but the parity suite audits
// only REQUEST-side fields (parity/field_coverage_test.go), so a Go RESPONSE field
// added without mirroring it here is invisible to CI. That is exactly how
// SnapshotResponse.skipped_repos slipped in (commit 872dcd32 added it to the daemon
// and CLI but not to web/): the web silently decoded only `instances` and rendered a
// truncated session list as complete.
//
// This test parses types.ts as text (interfaces are erased at runtime, so import
// alone cannot assert a field's presence) and fails if the mirror drifts again. It
// mirrors the lightweight, dependency-free style of connect_expired_auth.test.ts.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const TYPES_SRC = readFileSync(new URL("./types.ts", import.meta.url), "utf8");

/** Extracts the body of an `export interface NAME { ... }` block from types.ts. */
function interfaceBody(name: string): string {
  const head = `export interface ${name} {`;
  const start = TYPES_SRC.indexOf(head);
  assert.ok(start >= 0, `web/src/types.ts is missing \`export interface ${name}\` — the hand-mirror of daemon/snapshot.go has drifted (see #4742)`);
  const end = TYPES_SRC.indexOf("\n}", start);
  assert.ok(end >= 0, `web/src/types.ts: unterminated \`export interface ${name}\``);
  return TYPES_SRC.slice(start, end);
}

test("SnapshotResponse mirrors the daemon's skipped_repos incompleteness channel (#4742)", () => {
  const body = interfaceBody("SnapshotResponse");
  // The field MUST be present and typed as SkippedRepo[] — the rail renders its
  // persistent incompleteness notice from it. An absent field is the original bug;
  // a wrong type (e.g. unknown[]) would force a cast at the consume site.
  assert.match(body, /skipped_repos\?:\s*SkippedRepo\[/,
    "SnapshotResponse must declare `skipped_repos?: SkippedRepo[]` to mirror daemon/snapshot.go");
});

test("SkippedRepo mirrors the daemon's per-repo skip shape (#4742)", () => {
  const body = interfaceBody("SkippedRepo");
  // repo_id is non-optional on the Go side (SkippedRepo.RepoID string) and is the
  // stable identity the rail names; reason and failed_rows are omitempty there.
  assert.match(body, /repo_id:\s*string/, "SkippedRepo.repo_id is the stable repo identity (required)");
  assert.match(body, /reason\?:\s*string/, "SkippedRepo.reason carries the daemon's stable code (omitempty)");
  assert.match(body, /failed_rows\?:\s*number/, "SkippedRepo.failed_rows counts unloadable rows (omitempty)");
});

test("SnapshotResponse still mirrors the fields the rail already rendered (no regression)", () => {
  const body = interfaceBody("SnapshotResponse");
  // The pre-existing fields must stay mirrored: this guard exists because a NEW
  // field was missed, so it must not swallow a regression that drops an old one.
  assert.match(body, /instances:\s*SessionData\[\]\s*\|\s*null/, "instances is the authoritative projection");
  assert.match(body, /boot_id\?:\s*string/, "boot_id identifies the daemon process");
  assert.match(body, /operation_lock_timeout_ms\?:\s*number/);
  assert.match(body, /operation_clock_ms\?:\s*number/);
});
