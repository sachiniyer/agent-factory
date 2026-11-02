import assert from "node:assert/strict";
import { test } from "node:test";
import { activeRevealKey, scrollDeltaToReveal, shouldRevealActive, tabOverflowFlags } from "./tabscroll.js";

// The bar spans x 422–879 in the play-test that found #6420.
test("a tab already inside the bar needs no scroll", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 422, 504), 0);
  assert.equal(scrollDeltaToReveal(422, 879, 700, 879), 0);
});

test("a tab past the end scrolls forward by exactly its overhang", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 2575, 2714), 2714 - 879);
  assert.equal(scrollDeltaToReveal(422, 879, 800, 940), 940 - 879);
});

test("sub-pixel overhang is not an overhang", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 421.6, 504), 0);
  assert.equal(scrollDeltaToReveal(422, 879, 700, 879.4), 0);
});

test("a tab before the start scrolls back by exactly its overhang", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 300, 440), 300 - 422);
});

test("a tab wider than the bar aligns its start, so the label stays readable", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 500, 1100), 500 - 422);
  assert.equal(scrollDeltaToReveal(422, 879, 300, 700), 300 - 422, "start hidden, end inside: show the label");
});

test("a tab wider than the bar that already fills the viewport is left alone (#6420 fix-forward)", () => {
  // Selftest geometry: a 142px strip showing the middle/end of a 250px tab.
  assert.equal(scrollDeltaToReveal(626, 768, 626, 876), 0, "starts at the left edge");
  assert.equal(scrollDeltaToReveal(626, 768, 500, 900), 0, "spans both edges");
  assert.equal(scrollDeltaToReveal(626, 768, 518, 767.6), 0, "sub-pixel short of the end is still filling it");
  assert.equal(scrollDeltaToReveal(626, 768, 700, 950), 700 - 626, "starts inside the bar: align the label");
});

test("overflow flags mark exactly the edges that still hide content (#6408)", () => {
  // A ~600px strip showing 15 tabs: only the right edge hides tabs at first.
  assert.deepEqual(tabOverflowFlags(0, 600, 1400), { left: false, right: true });
  assert.deepEqual(tabOverflowFlags(300, 600, 1400), { left: true, right: true });
  assert.deepEqual(tabOverflowFlags(800, 600, 1400), { left: true, right: false }, "scrolled to the end");
  // Everything fits: no cue at all — the flag must not cry wolf.
  assert.deepEqual(tabOverflowFlags(0, 1400, 1400), { left: false, right: false });
  // Sub-pixel scroll geometry must not flicker the cue at the exact ends.
  assert.deepEqual(tabOverflowFlags(0.4, 600, 1400), { left: false, right: true });
  assert.deepEqual(tabOverflowFlags(799.6, 600, 1400), { left: true, right: false });
});

test("reveal fires on a new active tab or session, never on a plain re-render", () => {
  assert.equal(shouldRevealActive(null, "s1", 0), true);
  const key = activeRevealKey("s1", 3);
  assert.equal(shouldRevealActive(key, "s1", 3), false, "same active tab: leave the user's scroll alone");
  assert.equal(shouldRevealActive(key, "s1", 4), true, "active tab moved");
  assert.equal(shouldRevealActive(key, "s2", 3), true, "different session, same index");
});
