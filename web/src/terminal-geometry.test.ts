import assert from "node:assert/strict";
import test from "node:test";
import {
  hasVisibleTerminalGeometry,
  shouldRefitVisibleTerminal,
  shouldRestoreViewport,
  terminalUserScrollPlan,
  viewportAnchorLine,
  viewportMarkerOffset,
} from "./terminal-geometry.js";

const local = { rows: 36, cols: 120 };
const peer = { rows: 111, cols: 120 };

test("#2347: a visible host reclaims a different peer-owned grid", () => {
  assert.equal(shouldRefitVisibleTerminal({ width: 992, height: 620 }, peer, local), true);
});

test("#2347: zero-size and unresolved hosts never fit", () => {
  assert.equal(shouldRefitVisibleTerminal({ width: 0, height: 620 }, peer, local), false);
  assert.equal(shouldRefitVisibleTerminal({ width: 992, height: 0 }, peer, local), false);
  assert.equal(shouldRefitVisibleTerminal({ width: 992, height: 620 }, peer, undefined), false);
  assert.equal(shouldRefitVisibleTerminal({ width: 992, height: 620 }, peer, { rows: 0, cols: 120 }), false);
  assert.equal(shouldRefitVisibleTerminal({ width: 992, height: 620 }, peer, { rows: 36, cols: 0 }), false);
});

test("#2347: an already-local grid does not emit resize churn", () => {
  assert.equal(shouldRefitVisibleTerminal({ width: 992, height: 620 }, local, local), false);
});

test("#2347: a measured local grid is ready before the first socket resize", () => {
  assert.equal(hasVisibleTerminalGeometry({ width: 992, height: 620 }, local), true);
  assert.equal(hasVisibleTerminalGeometry({ width: 0, height: 620 }, local), false);
  assert.equal(hasVisibleTerminalGeometry({ width: 992, height: 620 }, undefined), false);
});

test("#2347: a scrollback marker anchors the visible line, not a stale bottom distance", () => {
  // Cursor absolute line = baseY + cursorY = 124. The viewport starts at line
  // 91, so a marker 33 lines above the cursor follows that content as output is
  // appended while this client is inactive.
  assert.equal(viewportMarkerOffset({ baseY: 100, cursorY: 24, viewportY: 91 }), -33);
});

test("#2347: a disposed marker falls back to the saved line instead of the top", () => {
  assert.equal(viewportAnchorLine({ atBottom: false, markerLine: 42, fallbackLine: 17 }, 83), 42);
  assert.equal(viewportAnchorLine({ atBottom: false, markerLine: -1, fallbackLine: 17 }, 83), 17);
  assert.equal(viewportAnchorLine({ atBottom: false, markerLine: null, fallbackLine: 17 }, 83), 17);
  assert.equal(viewportAnchorLine({ atBottom: true, markerLine: -1, fallbackLine: 17 }, 83), 83);
});

test("#2347: only user scroll intent cancels a deferred viewport restore", () => {
  assert.equal(shouldRestoreViewport({ scheduledUserScroll: 4, currentUserScroll: 4 }), true);
  assert.equal(shouldRestoreViewport({ scheduledUserScroll: 4, currentUserScroll: 5 }), false);
});

test("#2347: every direct scroll input supersedes the saved peer anchor", () => {
  for (const source of ["wheel", "touch", "scrollbar"] as const) {
    assert.deepEqual(terminalUserScrollPlan(source, false), { cancelScheduledVisibleFit: false });
  }
});

test("#2347: a direct scroll cancels a stale queued activation fit", () => {
  assert.deepEqual(terminalUserScrollPlan("wheel", true), { cancelScheduledVisibleFit: true });
});
