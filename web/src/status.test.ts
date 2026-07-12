// Parity tests for the status-dot + title mapping (#1592 Phase 5 PR3). They pin
// web/src/status.ts to ui/tree/render.go: every Liveness value and every in-flight
// op maps to the SAME dot color-bucket and the SAME [lost]/[deleting]/[limit]/
// [remote] title prefix the TUI paints. If the Go renderer's mapping changes, these
// must change with it — the two clients cannot diverge in status semantics (§3).

import { test } from "node:test";
import assert from "node:assert/strict";

import { isArchived, rowStatus, rowTitle } from "./status.js";
import { InFlightOp, Liveness, Status, type SessionData } from "./types.js";

function sess(over: Partial<SessionData> = {}): SessionData {
  return { title: "s", branch: "b", ...over };
}

test("liveness → dot kind mirrors render.go's TOTAL switch", () => {
  const cases: Array<[number, string, string]> = [
    [Liveness.Ready, "ready", "●"],
    [Liveness.Lost, "lost", "◌"],
    [Liveness.Dead, "dead", "○"],
    [Liveness.Archived, "archived", "▧"],
    [Liveness.LimitReached, "limit", "◆"],
    [Liveness.Running, "working", "●"],
    [Liveness.Unset, "working", "●"], // stray zero renders like Running (render.go:297)
  ];
  for (const [lv, kind, glyph] of cases) {
    const st = rowStatus(sess({ liveness: lv }));
    assert.equal(st.kind, kind, `liveness ${lv} → ${kind}`);
    assert.equal(st.glyph, glyph, `liveness ${lv} glyph`);
  }
});

test("only Ready/Running/Unset spin like the TUI (Ready is a settled dot)", () => {
  assert.equal(rowStatus(sess({ liveness: Liveness.Running })).spinning, true);
  assert.equal(rowStatus(sess({ liveness: Liveness.Ready })).spinning, false);
  assert.equal(rowStatus(sess({ liveness: Liveness.Lost })).spinning, false);
});

test("any in-flight op overlays the liveness and wins the working dot", () => {
  for (const op of [InFlightOp.Creating, InFlightOp.Killing, InFlightOp.Archiving, InFlightOp.Restoring]) {
    // Even a Ready liveness reads as working while an op is in flight (render.go:280).
    const st = rowStatus(sess({ liveness: Liveness.Ready, in_flight_op: op }));
    assert.equal(st.kind, "working", `op ${op} → working`);
    assert.equal(st.spinning, true);
  }
});

test("liveness absent falls back to the legacy status int", () => {
  assert.equal(rowStatus(sess({ status: Status.Ready })).kind, "ready");
  assert.equal(rowStatus(sess({ status: Status.Lost })).kind, "lost");
  assert.equal(rowStatus(sess({ status: Status.Dead })).kind, "dead");
  assert.equal(rowStatus(sess({ status: Status.Archived })).kind, "archived");
  assert.equal(rowStatus(sess({ status: Status.Running })).kind, "working");
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

/** Builds an RFC3339 timestamp for today at the given local hour/min so the
 *  formatLimitReset "same day → bare clock" branch is exercised deterministically. */
function isoTodayAt(hour: number, min: number): string {
  const d = new Date();
  d.setHours(hour, min, 0, 0);
  return d.toISOString();
}
