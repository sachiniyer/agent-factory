// Parity tests for the status-dot + title mapping (#1592 Phase 5 PR3). They pin
// web/src/status.ts to ui/tree/render.go: every Liveness value and every in-flight
// op maps to the SAME dot color-bucket and the SAME [lost]/[deleting]/[limit]/
// [remote] title prefix the TUI paints. If the Go renderer's mapping changes, these
// must change with it — the two clients cannot diverge in status semantics (§3).

import { test } from "node:test";
import assert from "node:assert/strict";

import { type DotKind, isArchived, isLimitReached, isWorking, rowStatus, rowTitle } from "./status.js";
import { InFlightOp, Liveness, Status, type SessionData } from "./types.js";

function sess(over: Partial<SessionData> = {}): SessionData {
  return { title: "s", branch: "b", ...over };
}

test("liveness → dot kind mirrors render.go's TOTAL switch", () => {
  // Working (LiveRunning / the Unset sentinel) shows NO dot (#1766): null kind, empty
  // glyph. Ready is the only positive/green dot; the error states keep static glyphs.
  const cases: Array<[number, DotKind | null, string]> = [
    [Liveness.Ready, "ready", "●"],
    [Liveness.Lost, "lost", "◌"],
    [Liveness.Dead, "dead", "○"],
    [Liveness.Archived, "archived", "▧"],
    [Liveness.LimitReached, "limit", "◆"],
    [Liveness.Running, null, ""],
    [Liveness.Unset, null, ""], // stray zero renders like Running (render.go:297)
  ];
  for (const [lv, kind, glyph] of cases) {
    const st = rowStatus(sess({ liveness: lv }));
    assert.equal(st.kind, kind, `liveness ${lv} → ${kind}`);
    assert.equal(st.glyph, glyph, `liveness ${lv} glyph`);
  }
});

test("a working row has no dot; Ready/error states do (#1766)", () => {
  const running = rowStatus(sess({ liveness: Liveness.Running }));
  assert.equal(running.kind, null, "Running is working → no dot");
  assert.equal(running.glyph, "", "a working row draws no glyph");
  assert.equal(isWorking(sess({ liveness: Liveness.Running })), true);
  assert.equal(isWorking(sess({ liveness: Liveness.Unset })), true);
  assert.equal(isWorking(sess({ liveness: Liveness.Ready })), false);
  assert.equal(isWorking(sess({ liveness: Liveness.Lost })), false);
  assert.notEqual(rowStatus(sess({ liveness: Liveness.Ready })).kind, null, "Ready keeps its dot");
});

test("any in-flight op overlays the liveness and reads as working (no dot)", () => {
  for (const op of [InFlightOp.Creating, InFlightOp.Killing, InFlightOp.Archiving, InFlightOp.Restoring]) {
    // Even a Ready liveness reads as working while an op is in flight (render.go:280).
    const st = rowStatus(sess({ liveness: Liveness.Ready, in_flight_op: op }));
    assert.equal(st.kind, null, `op ${op} → working (no dot)`);
    assert.equal(st.glyph, "", `op ${op} draws no glyph`);
    assert.equal(isWorking(sess({ liveness: Liveness.Ready, in_flight_op: op })), true);
  }
});

test("liveness absent falls back to the legacy status int", () => {
  assert.equal(rowStatus(sess({ status: Status.Ready })).kind, "ready");
  assert.equal(rowStatus(sess({ status: Status.Lost })).kind, "lost");
  assert.equal(rowStatus(sess({ status: Status.Dead })).kind, "dead");
  assert.equal(rowStatus(sess({ status: Status.Archived })).kind, "archived");
  assert.equal(rowStatus(sess({ status: Status.Running })).kind, null);
});

test("title prefixes match render.go precedence", () => {
  // [lost] on a Lost row (render.go:315).
  assert.equal(rowTitle(sess({ title: "w", liveness: Liveness.Lost })), "[lost] w");
  // [deleting] while killing/archiving, and it beats the [lost] marker.
  assert.equal(
    rowTitle(sess({ title: "w", liveness: Liveness.Lost, in_flight_op: InFlightOp.Killing })),
    "[deleting] w",
  );
  assert.equal(rowTitle(sess({ title: "w", in_flight_op: InFlightOp.Archiving })), "[deleting] w");
  // Archived carries NO word prefix (render.go:326-338) — the glyph + dimming say it.
  assert.equal(rowTitle(sess({ title: "w", liveness: Liveness.Archived })), "w");
  // [remote] is outermost.
  assert.equal(
    rowTitle(sess({ title: "w", liveness: Liveness.Lost, backend_type: "remote" })),
    "[remote] [lost] w",
  );
});

test("[limit] badge shows the reset time like render.go's limitBadgePrefix", () => {
  const noReset = rowTitle(sess({ title: "w", liveness: Liveness.LimitReached }));
  assert.equal(noReset, "[limit] w");
  const withReset = rowTitle(
    sess({ title: "w", liveness: Liveness.LimitReached, limit_reset_at: isoTodayAt(15, 0) }),
  );
  assert.match(withReset, /^\[limit\] resets 3pm w$/);
  const withMinutes = rowTitle(
    sess({ title: "w", liveness: Liveness.LimitReached, limit_reset_at: isoTodayAt(15, 4) }),
  );
  assert.match(withMinutes, /^\[limit\] resets 3:04pm w$/);
});

test("isArchived reads the liveness (and the legacy fallback)", () => {
  assert.equal(isArchived(sess({ liveness: Liveness.Archived })), true);
  assert.equal(isArchived(sess({ liveness: Liveness.Ready })), false);
  assert.equal(isArchived(sess({ status: Status.Archived })), true);
});

// isLimitReached gates the web's Retry action (#1934). It is the predicate that
// decides whether a stuck session offers a way out at all, so its edges matter more
// than a boolean helper's usually would.
test("isLimitReached reads the liveness, so Retry appears exactly when the session is parked", () => {
  assert.equal(isLimitReached(sess({ liveness: Liveness.LimitReached })), true);
  assert.equal(isLimitReached(sess({ liveness: Liveness.Ready })), false);
  assert.equal(isLimitReached(sess({ liveness: Liveness.Running })), false);
  assert.equal(isLimitReached(sess({ liveness: Liveness.Archived })), false);
  assert.equal(isLimitReached(sess({ liveness: Liveness.Lost })), false);
  assert.equal(isLimitReached(sess()), false, "a projection with no liveness is not limit-blocked");
});

// The reset timestamp is NOT the signal, and conflating them breaks both ways.
// A session can be limit-blocked before its reset time has been parsed (the banner
// matched, the time did not), and a RESUMED session keeps its stale limit_reset_at
// in the projection — so keying the button off the timestamp would hide Retry on a
// genuinely parked session and show it on a running one.
test("isLimitReached ignores limit_reset_at, which outlives the state it described", () => {
  assert.equal(
    isLimitReached(sess({ liveness: Liveness.LimitReached })),
    true,
    "parked with no parsed reset time still offers Retry",
  );
  assert.equal(
    isLimitReached(sess({ liveness: Liveness.Ready, limit_reset_at: "2026-07-18T15:04:00Z" })),
    false,
    "a resumed session carrying a stale reset time must not offer Retry",
  );
});

/** Builds an RFC3339 timestamp for today at the given local hour/min so the
 *  formatLimitReset "same day → bare clock" branch is exercised deterministically. */
function isoTodayAt(hour: number, min: number): string {
  const d = new Date();
  d.setHours(hour, min, 0, 0);
  return d.toISOString();
}
