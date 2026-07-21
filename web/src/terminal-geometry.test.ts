import assert from "node:assert/strict";
import test from "node:test";
import {
  hasVisibleTerminalGeometry,
  shouldRefitVisibleTerminal,
  shouldRestoreViewport,
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

test("#2347: a wheel movement wins over a deferred viewport restore", () => {
  assert.equal(shouldRestoreViewport(12, 12), true);
  assert.equal(shouldRestoreViewport(12, 11), false);
  assert.equal(shouldRestoreViewport(12, 13), false);
});
