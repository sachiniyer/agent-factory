// Tests for the pure split-layout tree (feat(web): drag-and-drop split tabs). These
// pin the tree transforms the Playwright selftest exercises through the DOM — split,
// replace, close-collapse, resize, and the one-tab-one-pane dedupe — with no DOM and
// no daemon, exactly as nav.test.ts pins the keyboard state machine.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  closeLeaf,
  companionTab,
  findLeaf,
  type LayoutNode,
  leafCount,
  leaves,
  replaceTab,
  resetIds,
  setRatio,
  singleLeaf,
  splitLeaf,
  validate,
} from "./layout.js";

/** The tabs each leaf shows, in visual order. */
function tabs(node: LayoutNode): number[] {
  return leaves(node).map((l) => l.tab);
}

test("singleLeaf: the default layout is one pane bound to the given tab", () => {
  resetIds();
  const root = singleLeaf(0);
  assert.equal(root.kind, "leaf");
  assert.equal(leafCount(root), 1);
  assert.deepEqual(tabs(root), [0]);
});

test("splitLeaf: left/right make a row; the new pane lands on the dragged edge", () => {
  resetIds();
  const root = singleLeaf(0);
  const right = splitLeaf(root, root.id, "right", 1);
  assert.equal(right.kind, "split");
  if (right.kind === "split") {
    assert.equal(right.dir, "row");
    assert.equal(right.ratio, 0.5);
  }
  // right edge → existing pane first (a), new tab second (b).
  assert.deepEqual(tabs(right), [0, 1]);

  resetIds();
  const base = singleLeaf(0);
  const left = splitLeaf(base, base.id, "left", 1);
  // left edge → new tab first.
  assert.deepEqual(tabs(left), [1, 0]);
});

test("splitLeaf: top/bottom make a column", () => {
  resetIds();
  const root = singleLeaf(0);
  const down = splitLeaf(root, root.id, "bottom", 2);
  assert.equal(down.kind, "split");
  if (down.kind === "split") {
    assert.equal(down.dir, "column");
  }
  assert.deepEqual(tabs(down), [0, 2]);
});

test("splitLeaf center is a replace, not a split", () => {
  resetIds();
  const root = singleLeaf(0);
  const replaced = splitLeaf(root, root.id, "center", 3);
  assert.equal(replaced.kind, "leaf");
  assert.deepEqual(tabs(replaced), [3]);
});

test("one tab, one pane: dragging a shown tab MOVES it instead of duplicating", () => {
  resetIds();
  // Two panes: tab 0 | tab 1.
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  const leftId = leaves(two)[0].id;
  // Drag tab 1 (already shown) onto the left pane's bottom edge: it moves there, and
  // its old pane collapses — still exactly two panes, no duplicate tab 1.
  const moved = splitLeaf(two, leftId, "bottom", 1);
  assert.equal(leafCount(moved), 2);
  const shown = tabs(moved).slice().sort((a, b) => a - b);
  assert.deepEqual(shown, [0, 1]);
});

test("replaceTab: rebinds a pane and dedupes the tab from elsewhere", () => {
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1); // 0 | 1
  const rightId = leaves(two)[1].id;
  // Replace the right pane (tab 1) with tab 0: the left pane (also tab 0) collapses,
  // leaving a single pane showing tab 0.
  const result = replaceTab(two, rightId, 0);
  assert.equal(leafCount(result), 1);
  assert.deepEqual(tabs(result), [0]);
});

test("closeLeaf: removing a pane collapses its split so the sibling fills", () => {
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1); // 0 | 1
  const rightId = leaves(two)[1].id;
  const collapsed = closeLeaf(two, rightId);
  assert.ok(collapsed);
  assert.equal(collapsed?.kind, "leaf");
  assert.deepEqual(collapsed ? tabs(collapsed) : [], [0]);
});

test("closeLeaf: the last pane cannot be closed (returns null)", () => {
  resetIds();
  const root = singleLeaf(0);
  assert.equal(closeLeaf(root, root.id), null);
});

test("closeLeaf: a nested split collapses correctly", () => {
  resetIds();
  // 0 | (1 / 2): split root right→1, then split the right pane bottom→2.
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  const rightId = leaves(two)[1].id;
  const three = splitLeaf(two, rightId, "bottom", 2);
  assert.equal(leafCount(three), 3);
  // Close the middle pane (tab 1): its split collapses to tab 2, leaving 0 | 2.
  const midId = leaves(three).find((l) => l.tab === 1)?.id ?? "";
  const after = closeLeaf(three, midId);
  assert.ok(after);
  assert.equal(after ? leafCount(after) : 0, 2);
  assert.deepEqual(after ? tabs(after).slice().sort() : [], [0, 2]);
});

test("setRatio: sets a split's ratio, clamped to [0.1, 0.9]", () => {
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  assert.equal(two.kind, "split");
  const splitId = two.kind === "split" ? two.id : "";
  const ratioOf = (n: LayoutNode) => (n.kind === "split" ? n.ratio : Number.NaN);
  assert.equal(ratioOf(setRatio(two, splitId, 0.7)), 0.7);
  assert.equal(ratioOf(setRatio(two, splitId, 0.02)), 0.1, "clamped low");
  assert.equal(ratioOf(setRatio(two, splitId, 0.99)), 0.9, "clamped high");
});

test("validate: clamps out-of-range tabs and dedupes the collapse", () => {
  resetIds();
  // 0 | 1 | 3 across three panes, then the session shrinks to 2 tabs (0,1).
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  const rightId = leaves(two)[1].id;
  const three = splitLeaf(two, rightId, "right", 3);
  const clamped = validate(three, 2);
  // tab 3 clamps to 1, which duplicates the existing tab-1 pane → collapses. Left with
  // 0 and 1.
  assert.deepEqual(tabs(clamped).slice().sort(), [0, 1]);
});

test("findLeaf: locates a leaf by id, or null", () => {
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  const id = leaves(two)[1].id;
  assert.equal(findLeaf(two, id)?.tab, 1);
  assert.equal(findLeaf(two, "nope"), null);
});

// --- companionTab: the self-split (#1901) ------------------------------------
//
// The bug: splitting a pane with the tab it ALREADY shows binds that tab to both
// halves, so the one-tab-one-pane dedupe closes the original and the split collapses
// back — the drag reads as a no-op. companionTab is what the drop asks for a
// DIFFERENT tab to put in the new half.

test("companionTab: the self-split reproduces as a no-op without it (#1901 repro)", () => {
  resetIds();
  // A single pane on tab 1, in a 2-tab session. Splitting it with its OWN tab is the
  // gesture the user makes — and this is what it does unaided.
  const root = singleLeaf(1);
  const collapsed = splitLeaf(root, root.id, "right", 1);
  assert.equal(leafCount(collapsed), 1, "the dedupe collapses the self-split — the reported no-op");
  assert.deepEqual(tabs(collapsed), [1]);

  // With a companion, the same gesture splits into two DISTINCT tabs.
  const companion = companionTab(root, root.id, 1, 2);
  assert.equal(companion, 0);
  const split = splitLeaf(root, root.id, "right", companion ?? -1);
  assert.equal(leafCount(split), 2);
  assert.deepEqual(tabs(split), [1, 0], "the dragged tab stays put; the new right half opens the other tab");
});

test("companionTab: prefers the recently-focused tab over the next in order", () => {
  resetIds();
  // A 4-tab session showing tab 0; focus last passed through tab 2.
  const root = singleLeaf(0);
  assert.equal(companionTab(root, root.id, 0, 4, [2]), 2);
  // The preference is ordered, most-recent first.
  assert.equal(companionTab(root, root.id, 0, 4, [3, 2]), 3);
  // A stale preference (the dragged tab itself, or out of range) is skipped, not bound.
  assert.equal(companionTab(root, root.id, 0, 4, [0, 9, -1, 2]), 2);
});

test("companionTab: with no preference it walks to the next tab in order, wrapping", () => {
  resetIds();
  const root = singleLeaf(0);
  assert.equal(companionTab(root, root.id, 0, 3), 1, "next in order");
  // From the LAST tab the walk wraps — which is "the first other tab".
  const last = singleLeaf(2);
  assert.equal(companionTab(last, last.id, 2, 3), 0, "wraps to the first other tab");
});

test("companionTab: never steals a tab that another pane is already showing", () => {
  resetIds();
  // Two panes in a 3-tab session: 0 | 1. Self-split the left pane (tab 0).
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  const leftId = leaves(two)[0].id;
  // Tab 1 is live in the right pane, so the walk skips it and lands on tab 2. Binding 1
  // would MOVE it here and collapse the pane the user opened it in.
  assert.equal(companionTab(two, leftId, 0, 3, [1]), 2, "a preferred-but-visible tab is skipped");
  const split = splitLeaf(two, leftId, "bottom", 2);
  assert.equal(leafCount(split), 3, "the existing tab-1 pane survives");
  assert.deepEqual(tabs(split).slice().sort(), [0, 1, 2]);
});

test("companionTab: null when the session has no other tab to show", () => {
  resetIds();
  // A single-tab session: there is nothing else to open, so the drag stays a no-op.
  const only = singleLeaf(0);
  assert.equal(companionTab(only, only.id, 0, 1), null);
  // And when every other tab is ALREADY on screen: 0 | 1 in a 2-tab session.
  const two = splitLeaf(only, only.id, "right", 1);
  const leftId = leaves(two)[0].id;
  assert.equal(companionTab(two, leftId, 0, 2), null);
});
