import assert from "node:assert/strict";
import { test } from "node:test";
import { activeRevealKey, scrollDeltaToReveal, shouldRevealActive } from "./tabscroll.js";

// The bar spans x 422–879 in the play-test that found #6420.
test("a tab already inside the bar needs no scroll", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 422, 504), 0);
  assert.equal(scrollDeltaToReveal(422, 879, 700, 879), 0);
});

test("a tab past the end scrolls forward by exactly its overhang", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 2575, 2714), 2714 - 879);
  assert.equal(scrollDeltaToReveal(422, 879, 800, 940), 940 - 879);
});

test("a tab before the start scrolls back by exactly its overhang", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 300, 440), 300 - 422);
});

test("a tab wider than the bar aligns its start, so the label stays readable", () => {
  assert.equal(scrollDeltaToReveal(422, 879, 500, 1100), 500 - 422);
  assert.equal(scrollDeltaToReveal(422, 879, 100, 900), 100 - 422);
});

test("reveal fires on a new active tab or session, never on a plain re-render", () => {
  assert.equal(shouldRevealActive(null, "s1", 0), true);
  const key = activeRevealKey("s1", 3);
  assert.equal(shouldRevealActive(key, "s1", 3), false, "same active tab: leave the user's scroll alone");
  assert.equal(shouldRevealActive(key, "s1", 4), true, "active tab moved");
  assert.equal(shouldRevealActive(key, "s2", 3), true, "different session, same index");
});
