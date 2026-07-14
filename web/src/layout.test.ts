// Tests for the pure split-layout tree (feat(web): drag-and-drop split tabs). These
// pin the tree transforms the Playwright selftest exercises through the DOM — split,
// replace, close-collapse, resize, and the one-tab-one-pane dedupe — with no DOM and
// no daemon, exactly as nav.test.ts pins the keyboard state machine.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  closeLeaf,
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
