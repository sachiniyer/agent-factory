// Pins the hold-vs-scroll split behind the tab bar's touch drag (#2899).
//
// The property that matters most is the NEGATIVE one: a finger scrolling the
// overflowed tab bar must never be read as picking a tab up. Getting that wrong does
// not merely misfire — picking up suspends the bar's touch-action, so a false pick-up
// leaves the bar unable to scroll, which is worse than the silence being fixed.

import { test } from "node:test";
import assert from "node:assert/strict";

import { pressDistance, TAB_PRESS_LIMITS, tabPressVerdict } from "./tabtouch.js";

const limits = { holdMs: 500, slopPx: 10 };

test("a settled long press picks the tab up", () => {
  assert.equal(tabPressVerdict({ heldMs: 500, movedPx: 0 }, limits), "pickUp");
  assert.equal(tabPressVerdict({ heldMs: 900, movedPx: 9 }, limits), "pickUp", "within slop still counts as settled");
});

test("a tap is over before the hold and never picks anything up", () => {
  assert.equal(tabPressVerdict({ heldMs: 80, movedPx: 0 }, limits), "waiting");
  assert.equal(tabPressVerdict({ heldMs: 499, movedPx: 0 }, limits), "waiting");
});

test("a finger that MOVES is scrolling the bar — hand it back, however long it is held", () => {
  assert.equal(tabPressVerdict({ heldMs: 20, movedPx: 40 }, limits), "abandon");
  // The load-bearing case: slop is checked BEFORE the hold, so a slow scroll that
  // takes longer than holdMs is still a scroll and must not raise the hint.
  assert.equal(
    tabPressVerdict({ heldMs: 5_000, movedPx: 11 }, limits),
    "abandon",
    "a slow drag is a scroll, not a hold — this is what keeps the bar scrollable",
  );
});

test("distance is straight-line, so a diagonal scroll counts as movement", () => {
  assert.equal(pressDistance(0, 0, 3, 4), 5);
  assert.equal(pressDistance(10, 10, 10, 10), 0);
  assert.equal(
    tabPressVerdict({ heldMs: 800, movedPx: pressDistance(0, 0, 8, 8) }, limits),
    "abandon",
    "8px on each axis is ~11px of travel — past slop even though neither axis is",
  );
});

test("the shipped limits are longer than a tap and than the browser's own long press", () => {
  assert.ok(TAB_PRESS_LIMITS.holdMs >= 400, "must not fire on a slow tap");
  assert.ok(TAB_PRESS_LIMITS.slopPx > 0 && TAB_PRESS_LIMITS.slopPx <= 16, "slop must tolerate a jitter, not a swipe");
});
