import assert from "node:assert/strict";
import test from "node:test";
import { shouldRefitVisibleTerminal, viewportLineFromBottom } from "./terminal-geometry.js";

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

test("#2347: a peer reflow restores the user's distance from the bottom", () => {
  assert.equal(viewportLineFromBottom(24, 0), 24);
  assert.equal(viewportLineFromBottom(24, 5), 19);
  assert.equal(viewportLineFromBottom(24, 99), 0);
  assert.equal(viewportLineFromBottom(24, -3), 24);
});
