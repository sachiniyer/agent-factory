// Tests for the pure split-layout tree (feat(web): drag-and-drop split tabs). These
// pin the tree transforms the Playwright selftest exercises through the DOM — split,
// replace, close-collapse, resize, and the one-tab-one-pane dedupe — with no DOM and
// no daemon, exactly as nav.test.ts pins the keyboard state machine.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  closeLeaf,
  fitToSize,
  focusSuccessorAfterDrop,
  companionTab,
  findLeaf,
  type LayoutNode,
  leafCount,
  leafSizes,
  leaves,
  minSubtreeSize,
  PANE_DIVIDER_PX,
  PANE_MIN_HEIGHT_PX,
  PANE_MIN_WIDTH_PX,
  remapByIdentity,
  replaceTab,
  resetIds,
  sameLayout,
  setRatio,
  siblingSubtreeOf,
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

// --- siblingSubtreeOf: the sibling subtree closeLeaf substitutes ----------------
//
// closeLeaf collapses the closed leaf's parent split and substitutes the surviving
// sibling, so the sibling is the pane/region that takes over the closed pane's
// screen. siblingSubtreeOf returns that sibling so closePane can focus a leaf
// within the pane that grew, rather than the leftmost leaf of the whole tree (an
// unrelated root branch for a nested close). Reference-stable: the returned node
// survives closeLeaf unchanged.

test("siblingSubtreeOf: null for a single-leaf tree (no sibling exists)", () => {
  resetIds();
  const root = singleLeaf(0);
  assert.equal(siblingSubtreeOf(root, root.id), null);
});

test("siblingSubtreeOf: a 2-pane split returns the other leaf", () => {
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  const [a, b] = leaves(two);
  const sibOfA = siblingSubtreeOf(two, a.id);
  const sibOfB = siblingSubtreeOf(two, b.id);
  assert.ok(sibOfA);
  assert.ok(sibOfB);
  assert.equal(sibOfA.id, b.id, "closing a leaves the sibling b");
  assert.equal(sibOfB.id, a.id, "closing b leaves the sibling a");
});

test("siblingSubtreeOf: a nested close returns the IMMEDIATE sibling, not a root branch", () => {
  // split(row, A, split(col, B, C)): closing B or C must return the other of B/C
  // (the sibling under the inner split), NOT the outer A.
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  const rightId = leaves(two)[1].id;
  const three = splitLeaf(two, rightId, "bottom", 2);
  const [a, b, c] = leaves(three);

  const sibOfB = siblingSubtreeOf(three, b.id);
  const sibOfC = siblingSubtreeOf(three, c.id);
  assert.ok(sibOfB);
  assert.ok(sibOfC);
  assert.equal(sibOfB.id, c.id, "closing nested B returns sibling C, not root-branch A");
  assert.equal(sibOfC.id, b.id, "closing nested C returns sibling B, not root-branch A");

  // Closing the outer A returns the sibling SUBTREE (the inner split), not a leaf.
  const outer = siblingSubtreeOf(three, a.id);
  assert.ok(outer);
  assert.equal(outer.kind, "split", "closing the root's left branch returns the inner split subtree");
  assert.deepEqual(leaves(outer).map((l) => l.id), [b.id, c.id], "the subtree's leaves are B and C");
  void a;
});

test("siblingSubtreeOf: the returned subtree survives closeLeaf unchanged (reference-stable)", () => {
  // closeLeaf hands the sibling back as-is when it collapses the parent, so the
  // node read on the pre-close tree is the same node present in the post-close tree.
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1);
  const rightId = leaves(two)[1].id;
  const three = splitLeaf(two, rightId, "bottom", 2);
  const [, b] = leaves(three);
  const siblingBefore = siblingSubtreeOf(three, b.id);
  const after = closeLeaf(three, b.id);
  assert.ok(siblingBefore);
  assert.ok(after);
  // The sibling node read before the close is the same node present in the
  // post-close tree (reference equality).
  assert.equal(findLeaf(after, siblingBefore.id), siblingBefore);
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

// sameLayout is the guard that decides whether reconcile re-inserts the split DOM,
// and re-inserting it detaches every live pane — which drops the scroll offset of a
// scrolled terminal (#1894). So these pin "would this rebuild?" as a contract, at the
// layer where it is cheap to check; the Playwright selftest proves the visible effect.

test("sameLayout: a tree is the same layout as itself", () => {
  resetIds();
  const root = splitLeaf(singleLeaf(0), leaves(singleLeaf(0))[0].id, "right", 1);
  assert.equal(sameLayout(root, root), true);
});

test("sameLayout: null (nothing built yet) is never the same layout", () => {
  resetIds();
  const root = singleLeaf(0);
  assert.equal(sameLayout(root, null), false);
  assert.equal(sameLayout(null, root), false);
  assert.equal(sameLayout(null, null), true);
});

// THE regression (local Codex review of #1894): setRatio rebuilds every SplitNode it
// walks, so persisting a divider drag returns a fresh root for a layout that is
// already on screen — the drag applied the ratio to the live DOM as it went. A
// reference check calls that a change and rebuilds, rewinding the scrolled terminal
// on the next roster resync. Re-setting the ratio a drag already applied must be a
// no-op to the DOM.
test("sameLayout: setRatio to the ratio already in the DOM is not a layout change", () => {
  resetIds();
  const one = singleLeaf(0);
  const root = splitLeaf(one, leaves(one)[0].id, "right", 1);
  const splitId = root.kind === "split" ? root.id : "";
  // What the drag does: mutate the live node in place (the DOM follows it live)...
  if (root.kind === "split") {
    root.ratio = 0.3;
  }
  // ...then persist it on pointer-up, which allocates a whole new node graph.
  const persisted = setRatio(root, splitId, 0.3);
  assert.notEqual(persisted, root, "setRatio really does allocate a fresh root");
  assert.equal(
    sameLayout(persisted, root),
    true,
    "persisting a ratio the DOM already shows must not force a rebuild",
  );
});

test("sameLayout: a ratio the DOM has NOT applied is a layout change", () => {
  resetIds();
  const one = singleLeaf(0);
  const root = splitLeaf(one, leaves(one)[0].id, "right", 1);
  const splitId = root.kind === "split" ? root.id : "";
  const resized = setRatio(root, splitId, 0.8);
  assert.equal(sameLayout(resized, root), false, "a real ratio change must rebuild to apply it");
});

// A leaf's tab is excluded on purpose: the pane CONTAINER is keyed by leaf id, and
// which tab it streams is settled by reconcile's identity check, which rebuilds the
// terminal inside the container without disturbing the container. If tab counted here,
// any out-of-band reorder would re-insert the DOM and re-arm the rewind.
test("sameLayout: a leaf whose tab merely moved ordinal is not a layout change", () => {
  resetIds();
  const one = singleLeaf(0);
  const root = splitLeaf(one, leaves(one)[0].id, "right", 1);
  // The roster reorders under it: the same tabs, swapped ordinals.
  const remapped = remapByIdentity(root, ["a", "b"], ["b", "a"]);
  assert.deepEqual(tabs(remapped).sort(), tabs(root).sort(), "the same tabs are still shown");
  assert.equal(
    sameLayout(remapped, root),
    true,
    "a reorder rebinds terminals, it does not re-insert the DOM",
  );
});

test("sameLayout: splitting really is a layout change", () => {
  resetIds();
  const root = singleLeaf(0);
  const split = splitLeaf(root, leaves(root)[0].id, "right", 1);
  assert.equal(sameLayout(split, root), false);
  // ...and so is closing a pane back down.
  const closed = closeLeaf(split, leaves(split)[1].id);
  assert.equal(sameLayout(closed, split), false);
});

// --- remapByIdentity + validate: cross-client close of the last tab -------------
//
// split.ts's setSession (same-session resync) runs `remapByIdentity` then `validate`
// (split.ts:469-470). When another client closes the highest-ordinal tab — a PURE
// shrink, no replacement — a dead pane left at its now-out-of-range ordinal used to
// fall through to `validate`, which CLAMPED it down onto the survivor's ordinal and
// (keeping the FIRST leaf in visual order) evicted the survivor when the dead pane
// was visually earlier. The survivor's AttachTerminal was then disposed and its
// scrollback lost; the dead pane was rebound to the survivor's tab. remapByIdentity
// now closes those out-of-range dead leaves itself, so validate never clamps a dead
// leaf onto a survivor.

/** The exact setSession sequence (split.ts:469-470): remap then validate against the
 *  new tab count. `tabCount` mirrors split.ts:446 (1 when the list is empty). */
function setSessionSequence(tree: LayoutNode, prevIds: string[], ids: string[]): LayoutNode {
  const settled = remapByIdentity(tree, prevIds, ids);
  const tabCount = ids.length > 0 ? ids.length : 1;
  return validate(settled, tabCount);
}

test("pure shrink with dead pane visually FIRST: the SURVIVOR must keep its pane (regression)", () => {
  // A split-LEFT puts the new, higher-ordinal tab's pane visually first. The user's
  // original pane (tab 0, "id-a") is visually LAST and holds the scrollback. Another
  // client closes the last tab ("id-b"), a pure 2→1 shrink with no replacement.
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "left", 1); // visual order: [leaf2(tab1), leaf1(tab0)]
  const Lnew = leaves(two)[0]; // shows "id-b" (tab 1) — the soon-to-close pane, visually first
  const Lorig = leaves(two)[1]; // shows "id-a" (tab 0) — the SURVIVOR with scrollback
  assert.equal(Lnew.tab, 1);
  assert.equal(Lorig.tab, 0);

  const out = setSessionSequence(two, ["id-a", "id-b"], ["id-a"]);

  const ls = leaves(out);
  assert.equal(ls.length, 1, "the shrink collapses to a single pane");
  assert.equal(
    ls[0].id,
    Lorig.id,
    "the original 'id-a' pane (the survivor) must be the one kept — it holds the live tab",
  );
  assert.equal(ls[0].tab, 0, "and it stays on its tab's ordinal");
  assert.notEqual(ls[0].id, Lnew.id, "the dead 'id-b' pane must NOT be the one rebound to id-a");
});

test("pure shrink with dead pane visually LAST: the survivor ALSO keeps its pane (control)", () => {
  // The common split-RIGHT ordering [orig, new] puts the dead pane visually last, where
  // validate's keep-first happens to pick the right pane even without the fix. Pinned so
  // the fix cannot regress the orientation that already worked.
  resetIds();
  const root = singleLeaf(0);
  const two = splitLeaf(root, root.id, "right", 1); // visual order: [leaf1(tab0), leaf2(tab1)]
  const Lorig = leaves(two)[0]; // shows "id-a" (tab 0) — the survivor
  const Lnew = leaves(two)[1]; // shows "id-b" (tab 1) — the soon-to-close pane, visually last

  const out = setSessionSequence(two, ["id-a", "id-b"], ["id-a"]);

  const ls = leaves(out);
  assert.equal(ls.length, 1);
  assert.equal(ls[0].id, Lorig.id, "the survivor keeps its pane regardless of visual order");
  assert.equal(ls[0].tab, 0);
  assert.notEqual(ls[0].id, Lnew.id);
});

test("pure shrink across THREE panes: only the dead pane closes, every survivor keeps its pane", () => {
  // Three panes with the highest-ordinal (dead-after-shrink) pane visually FIRST, so
  // validate's keep-first would otherwise clamp it down and evict a survivor. remap
  // must close ONLY the dead pane; the two survivors stay on their own tabs.
  resetIds();
  const tree: LayoutNode = {
    kind: "split",
    id: "S1",
    dir: "row",
    ratio: 0.5,
    a: { kind: "leaf", id: "Ldead", tab: 2 }, // shows "id-c" — visually FIRST, will close
    b: {
      kind: "split",
      id: "S2",
      dir: "row",
      ratio: 0.5,
      a: { kind: "leaf", id: "La", tab: 0 }, // shows "id-a"
      b: { kind: "leaf", id: "Lb", tab: 1 }, // shows "id-b"
    },
  };

  // Another client closes the last tab "id-c": 3 → 2.
  const out = setSessionSequence(tree, ["id-a", "id-b", "id-c"], ["id-a", "id-b"]);
  const ls = leaves(out);
  assert.deepEqual(
    ls.map((l) => l.id),
    ["La", "Lb"],
    "only the dead pane (Ldead) closes; both survivors keep their panes",
  );
  assert.deepEqual(ls.map((l) => l.tab), [0, 1], "survivors stay on their own tabs' ordinals");
});

test("a close+REPLACE (count-stable) still closes the dead pane via the survivor-wins guard, not the shrink guard", () => {
  // The pre-existing close+replace path: another client closes "id-a" and creates
  // "id-c", so the survivor "id-b" MOVES 2→1 (the dead slot) and claims it. The dead
  // leaf at tab 1 is closed by the claimed-collision loop (its slot still EXISTS), so
  // the new out-of-range loop must NOT also fire — both guards coexist harmlessly.
  resetIds();
  const tree: LayoutNode = {
    kind: "split",
    id: "S1",
    dir: "row",
    ratio: 0.5,
    a: { kind: "leaf", id: "Ldead", tab: 1 }, // shows "id-a" — will close, visually FIRST
    b: { kind: "leaf", id: "Lsurv", tab: 2 }, // shows "id-b" — survivor, moves 2→1
  };
  const prevIds = ["id-agent", "id-a", "id-b"];
  const ids = ["id-agent", "id-b", "id-c"]; // count unchanged (3→3)

  const out = setSessionSequence(tree, prevIds, ids);
  const ls = leaves(out);
  assert.equal(ls.length, 1, "the dead pane closes, leaving the survivor");
  assert.equal(ls[0].id, "Lsurv", "the survivor wins the now-shared ordinal");
  assert.equal(ls[0].tab, 1, "it follows id-b to its new ordinal");
  assert.notEqual(ls[0].id, "Ldead");
});

test("a SOLE dead leaf out-of-range is NOT closed by remapByIdentity — it degrades to validate's clamp", () => {
  // The last pane cannot be closed (closeLeaf returns null). A single dead leaf must
  // fall through to validate, which clamps it to max — the documented degrade for a
  // session that lost its only shown tab. The new out-of-range loop leaves it be.
  resetIds();
  const tree: LayoutNode = { kind: "leaf", id: "Lonly", tab: 1 }; // shows "id-b"

  // remapByIdentity alone: moved.size === 0 (nothing survives), so it returns the
  // tree unchanged — the new loop never runs.
  const remapped = remapByIdentity(tree, ["id-a", "id-b"], ["id-a"]);
  assert.equal(remapped, tree, "a sole dead leaf with no survivor is returned untouched");
  assert.equal(leaves(remapped)[0].tab, 1);

  // The full setSession sequence then degrades via validate's clamp to the only slot.
  const out = setSessionSequence(tree, ["id-a", "id-b"], ["id-a"]);
  assert.equal(leaves(out).length, 1);
  assert.equal(leaves(out)[0].id, "Lonly", "the sole pane is retained");
  assert.equal(leaves(out)[0].tab, 0, "validate clamps it to the only surviving ordinal");
});

test("a pure shrink whose clamp target is UNCLAIMED keeps the dead pane (validate rebinds it, no eviction)", () => {
  // A multi-pane layout showing tabs A (ordinal 0) and C (ordinal 2) while B (ordinal
  // 1) is unshown, shrunk to [A, B] (a lower tab closed, not the highest-ordinal one).
  // C's leaf is out-of-range (tab 2 >= ids.length 2), but the clamp target (ordinal 1)
  // is NOT claimed by any survivor — A is at 0, no pane is at 1. Closing C here would
  // collapse a two-pane layout that the new tab count still admits; validate() rebinds
  // C to the unclaimed ordinal 1 with no duplicate to drop, preserving both panes.
  // The shrink loop fires ONLY when the clamp target is claimed, so it must leave C.
  resetIds();
  const tree: LayoutNode = {
    kind: "split",
    id: "S1",
    dir: "row",
    ratio: 0.5,
    a: { kind: "leaf", id: "La", tab: 0 }, // shows "id-a"
    b: { kind: "leaf", id: "Lc", tab: 2 }, // shows "id-c" — unshown B sits at 1
  };
  // [A, B, C] → [A, B]: the middle tab's slot is the clamp target and it is unclaimed.
  const out = setSessionSequence(tree, ["id-a", "id-b", "id-c"], ["id-a", "id-b"]);
  const ls = leaves(out);
  assert.equal(ls.length, 2, "the unshown-slot pane is NOT closed — validate rebinds it");
  assert.deepEqual(
    ls.map((l) => l.id).sort(),
    ["La", "Lc"],
    "both panes survive; neither is evicted",
  );
  assert.equal(
    ls.find((l) => l.id === "Lc")!.tab,
    1,
    "C is clamped to the now-highest ordinal 1 (validate's harmless rebind)",
  );
});

test("a backfilled survivor (synthesized id → real id) is NOT closed by the shrink loop", () => {
  // A snapshot can replace a tab's synthesized `kind:name` identity with its real id
  // — the SAME tab to ui.ts (the adoption is not a close). `moved` matches by identity
  // string, so the backfilled survivor is NOT in `moved`/`claimed`. A pure shrink that
  // also removes a lower tab leaves that survivor's old ordinal out of range; the
  // shrink loop must NOT close it (its clamp target is unclaimed), or the pane the
  // user still has open is dropped and its terminal state lost. ui.ts tracks the
  // synthesized kind:name separately from the resolved id for exactly this case.
  //   prevIds: [id-agent, id-a, 1:shell]  →  ids: [id-agent, id-shell]
  //   panes:  id-agent at 0 (survivor), 1:shell at 2 (backfilled to id-shell at 1)
  //   the agent claims 0; the shell's clamp target (1) is unclaimed, so it rebinds.
  resetIds();
  const tree: LayoutNode = {
    kind: "split",
    id: "S1",
    dir: "row",
    ratio: 0.5,
    a: { kind: "leaf", id: "Lagent", tab: 0 }, // shows "id-agent"
    b: { kind: "leaf", id: "Lshell", tab: 2 }, // shows "1:shell", backfilled to id-shell
  };
  const out = setSessionSequence(tree, ["id-agent", "id-a", "1:shell"], ["id-agent", "id-shell"]);
  const ls = leaves(out);
  assert.equal(ls.length, 2, "the backfilled shell pane is kept — its clamp target is unclaimed");
  assert.deepEqual(
    ls.map((l) => l.id).sort(),
    ["Lagent", "Lshell"],
    "both the agent and the backfilled shell survive",
  );
  assert.equal(
    ls.find((l) => l.id === "Lshell")!.tab,
    1,
    "the shell is clamped to its tab's new ordinal (validate's rebind, not a close)",
  );
});

test("a backfilled survivor at the clamp ordinal is preserved when a different dead leaf is out of range", () => {
  // The backfill case the `claimed.has(max)` guard missed: a snapshot replaces a
  // tab's synthesized `kind:name` with its real id (the SAME tab to ui.ts, #1779)
  // AND removes a different highest-ordinal tab, so the backfilled survivor sits AT
  // the clamp target `max` (in range, so validate() keeps it there) while a
  // different dead leaf is out of range. `moved` matches by identity string, so the
  // backfilled survivor falls out of `moved`/`claimed` and `claimed.has(max)` is
  // false even though a real pane occupies `max`; the dead leaf was therefore left
  // for validate(), which clamped it onto `max` and — keeping the FIRST in visual
  // order — evicted the backfilled survivor when the dead leaf was visually earlier.
  // The shrink loop now detects the holder structurally (any leaf at `max`) and
  // closes the out-of-range dead leaf, so the survivor keeps its pane.
  //   prevIds: [id-agent, 1:shell, id-x]  →  ids: [id-agent, id-shell]
  //   panes:  dead X at ordinal 2 (visually FIRST), agent at 0, shell at 1 (max)
  //   X is out of range (2 >= 2); shell is the backfilled survivor at the clamp target.
  resetIds();
  const tree: LayoutNode = {
    kind: "split",
    id: "S1",
    dir: "row",
    ratio: 0.5,
    a: { kind: "leaf", id: "Ldead", tab: 2 }, // shows "id-x" — visually FIRST, out of range
    b: {
      kind: "split",
      id: "S2",
      dir: "row",
      ratio: 0.5,
      a: { kind: "leaf", id: "Lagent", tab: 0 }, // shows "id-agent"
      b: { kind: "leaf", id: "Lshell", tab: 1 }, // shows "1:shell", backfilled to id-shell at max
    },
  };
  const out = setSessionSequence(tree, ["id-agent", "1:shell", "id-x"], ["id-agent", "id-shell"]);
  const ls = leaves(out);
  assert.deepEqual(
    ls.map((l) => l.id).sort(),
    ["Lagent", "Lshell"],
    "the dead X pane closes; the agent and the backfilled shell both keep their panes",
  );
  assert.notEqual(
    ls.find((l) => l.id === "Lshell"),
    undefined,
    "the backfilled shell (the survivor at max) is preserved, not the dead X",
  );
  assert.equal(
    ls.find((l) => l.id === "Lshell")!.tab,
    1,
    "the shell stays at its tab's new ordinal (max), clamped nowhere",
  );
});

test("a backfilled survivor out of range is preserved when the dead leaf HOLDS max", () => {
  // The case `maxHeld` (any leaf at `max`) got wrong: the snapshot replaces a tab's
  // synthesized `kind:name` with its real id (#1779) AND removes a different LOWER
  // tab, so the backfilled survivor's OLD ordinal falls out of range while a DIFFERENT
  // dead leaf sits AT the clamp target `max`. `moved` matches by identity string, so
  // the backfilled survivor (synthesized id gone, real id adopted) falls out of
  // `moved`/`claimed`; the dead leaf at `max` also falls out of `moved`. The prior
  // guard read "any leaf at `max`" as "a survivor to protect" and closed the
  // out-of-range leaf — which here is the survivor — then validate() clamped the dead
  // leaf (kept at `max`) and handed the survivor's pane to the dead tab. The dead
  // leaf is the one to drop: its tab is gone (a real id absent from `ids`), while the
  // out-of-range leaf's identity is a synthesized `kind:name` a backfill adopted.
  //   prevIds: [id-agent, id-x, 1:shell]  →  ids: [id-agent, id-shell]
  //   panes:  shell at 2 (visually FIRST, out of range, backfilled), X at 1 (dead, at
  //   max), agent at 0. X is dead (id-x gone); shell is the backfilled survivor
  //   (1:shell → id-shell). Closing the out-of-range shell would discard its live
  //   terminal; the dead X at `max` is the leaf to close so shell rebinds to `max`.
  resetIds();
  const tree: LayoutNode = {
    kind: "split",
    id: "S1",
    dir: "row",
    ratio: 0.5,
    a: { kind: "leaf", id: "Lshell", tab: 2 }, // shows "1:shell", backfilled to id-shell — visually FIRST, out of range
    b: {
      kind: "split",
      id: "S2",
      dir: "row",
      ratio: 0.5,
      a: { kind: "leaf", id: "Lx", tab: 1 }, // shows "id-x" — dead, HOLDS max
      b: { kind: "leaf", id: "Lagent", tab: 0 }, // shows "id-agent"
    },
  };
  const out = setSessionSequence(tree, ["id-agent", "id-x", "1:shell"], ["id-agent", "id-shell"]);
  const ls = leaves(out);
  assert.deepEqual(
    ls.map((l) => l.id).sort(),
    ["Lagent", "Lshell"],
    "the dead X pane closes; the agent and the backfilled shell both keep their panes",
  );
  assert.notEqual(
    ls.find((l) => l.id === "Lshell"),
    undefined,
    "the backfilled shell (the out-of-range survivor) is preserved, not the dead X",
  );
  assert.equal(
    ls.find((l) => l.id === "Lx"),
    undefined,
    "the dead X pane (the holder at max) is dropped, not the backfilled survivor",
  );
  assert.equal(
    ls.find((l) => l.id === "Lshell")!.tab,
    1,
    "the shell rebinds to its tab's new ordinal (max) via validate's clamp",
  );
});

// #5982: a backfill must be matched to the synthesized tab it replaced. The old
// session-wide flag read "some real id appeared" as "every missing synthesized tab
// was adopted", so a CLOSED legacy tab stayed alive and the clamp evicted the real
// survivor's pane.
test("a backfill of one synthesized tab does not keep a different closed synthesized tab alive (#5982)", () => {
  resetIds();
  const seq = (leaf: string, tab: number) => ({ kind: "leaf", id: leaf, tab }) as LayoutNode;
  // Visual order: the closed tab's pane (ordinal 3) FIRST, then agent, legacy, survivor.
  const tree: LayoutNode = {
    kind: "split",
    id: "S1",
    dir: "row",
    ratio: 0.5,
    a: seq("Lclosed", 3),
    b: {
      kind: "split",
      id: "S2",
      dir: "row",
      ratio: 0.5,
      a: seq("Lagent", 0),
      b: { kind: "split", id: "S3", dir: "row", ratio: 0.5, a: seq("Llegacy", 1), b: seq("Lsurv", 2) },
    },
  };
  const out = setSessionSequence(
    tree,
    ["id-agent", "0:legacy", "id-survivor", "1:closed"],
    ["id-agent", "id-backfill", "id-survivor"],
  );
  const ls = leaves(out);
  assert.ok(ls.some((l) => l.id === "Lsurv" && l.tab === 2), "the real survivor keeps its pane at its ordinal");
  assert.ok(ls.some((l) => l.id === "Llegacy" && l.tab === 1), "the adopted legacy tab keeps its pane");
  assert.ok(!ls.some((l) => l.id === "Lclosed"), "the closed legacy tab's pane is the one that goes");
});

// #5983: the out-of-band close successor is closePane's sibling rule.
test("focusSuccessorAfterDrop: the sibling that grew into the dropped leaf's region, not the root-first leaf (#5983)", () => {
  resetIds();
  const a = singleLeaf(0);
  const two = splitLeaf(a, a.id, "right", 1);
  const r = leaves(two)[1];
  const three = splitLeaf(two, r.id, "bottom", 2); // A | (B / C)
  const [A, B, C] = leaves(three).map((l) => l.id);
  const survivors = new Set([A, C]);
  assert.equal(focusSuccessorAfterDrop(three, B, survivors), C, "B dropped: C took its space, not A");
  // The whole sibling gone too: the next ancestor's sibling (A) is the closest survivor.
  assert.equal(focusSuccessorAfterDrop(three, B, new Set([A])), A);
  // Sibling subtree with several leaves: its first surviving leaf.
  assert.equal(focusSuccessorAfterDrop(three, A, new Set([B, C])), B);
  assert.equal(focusSuccessorAfterDrop(three, A, new Set([C])), C, "dead first leaf of the sibling is skipped");
  assert.equal(focusSuccessorAfterDrop(null, B, survivors), null);
  assert.equal(focusSuccessorAfterDrop(a, a.id, new Set()), null);
});

// #6001: a lower legacy tab closes while a later legacy tab receives its real id.
// Positional pairing bound the new id to the CLOSED tab and dropped the live pane.
function twoLegacyTree(closedFirst: boolean): LayoutNode {
  resetIds();
  const l = (id: string, tab: number) => ({ kind: "leaf", id, tab }) as LayoutNode;
  const inner: LayoutNode = { kind: "split", id: "S2", dir: "row", ratio: 0.5, a: l("Lclosed", 1), b: l("Lshell", 2) };
  const swapped: LayoutNode = { kind: "split", id: "S2", dir: "row", ratio: 0.5, a: l("Lshell", 2), b: l("Lclosed", 1) };
  return { kind: "split", id: "S1", dir: "row", ratio: 0.5, a: l("Lagent", 0), b: closedFirst ? inner : swapped };
}

for (const closedFirst of [true, false]) {
  test(`a closed legacy tab is never paired with a new real id — names pair exactly (#6001, closed ${closedFirst ? "first" : "last"})`, () => {
    const prev = ["id-agent", "1:closed", "1:shell"];
    const next = ["id-agent", "id-shell"];
    const out = validate(
      remapByIdentity(twoLegacyTree(closedFirst), prev, next, ["0:agent", "1:shell"]),
      next.length,
    );
    const ls = leaves(out);
    assert.ok(ls.some((l) => l.id === "Lshell"), "the live shell pane survives");
    assert.ok(!ls.some((l) => l.id === "Lclosed"), "the closed legacy tab's pane goes");
    assert.equal(ls.find((l) => l.id === "Lshell")!.tab, 1);
  });
}

test("without names, an ambiguous gap keeps every candidate alive instead of guessing a pairing (#6001)", () => {
  const prev = ["id-agent", "1:closed", "1:shell"];
  const next = ["id-agent", "id-shell"];
  const ls = leaves(validate(remapByIdentity(twoLegacyTree(false), prev, next), next.length));
  assert.ok(ls.some((l) => l.id === "Lshell"), "the live shell pane is not dropped by a wrong pairing");
});

test("a renamed-then-backfilled tab falls back to the gap rule; a gap with no new id still closes (#6001)", () => {
  // The id's kind:name no longer matches (renamed), so no exact pair — but a new real
  // id sits in the gap, so the candidate stays alive; the trailing gap has none.
  resetIds();
  const l = (id: string, tab: number) => ({ kind: "leaf", id, tab }) as LayoutNode;
  const tree: LayoutNode = { kind: "split", id: "S", dir: "row", ratio: 0.5, a: l("Lold", 1), b: l("Lagent", 0) };
  const out = remapByIdentity(tree, ["id-agent", "1:old"], ["id-agent", "id-new"], ["0:agent", "1:renamed"]);
  assert.equal(leaves(out).length, 2, "the renamed-backfilled pane is kept");
});

// #6025: an ambiguous gap-fallback candidate must never displace a pane whose
// backfill was matched exactly.
for (const closedFirst of [true, false]) {
  test(`fallback candidate does not evict an exactly-matched live pane (#6025, closed ${closedFirst ? "first" : "last"})`, () => {
    resetIds();
    const l = (id: string, tab: number) => ({ kind: "leaf", id, tab }) as LayoutNode;
    const rest: LayoutNode = {
      kind: "split", id: "S2", dir: "row", ratio: 0.5,
      a: l("Lagent", 0),
      b: { kind: "split", id: "S3", dir: "row", ratio: 0.5, a: l("Lold", 1), b: l("Lshell", 2) },
    };
    const tree: LayoutNode = closedFirst
      ? { kind: "split", id: "S1", dir: "row", ratio: 0.5, a: l("Lclosed", 3), b: rest }
      : { kind: "split", id: "S1", dir: "row", ratio: 0.5, a: rest, b: l("Lclosed", 3) };
    const prev = ["id-agent", "1:old", "1:shell", "1:closed"];
    const next = ["id-agent", "id-renamed", "id-shell"];
    const out = validate(remapByIdentity(tree, prev, next, ["0:agent", "1:renamed", "1:shell"]), next.length);
    const ls = leaves(out);
    assert.ok(ls.some((x) => x.id === "Lshell" && x.tab === 2), "the exactly-matched shell pane keeps its slot");
    assert.ok(!ls.some((x) => x.id === "Lclosed"), "the closed tab's pane goes");
    assert.ok(ls.some((x) => x.id === "Lold"), "the renamed-backfilled pane is kept by the fallback");
    assert.ok(ls.some((x) => x.id === "Lagent"));
  });
}

// --- Pane minimums (#6429) -----------------------------------------------------
//
// The web port of the TUI's pane floor (ui/layout/grid.go PaneMinWidth=40): the
// fit hides whatever cannot keep the floor rather than letting flex render
// slivers, and split.ts reports the hidden count through .af-split-narrow.

const leaf = (id: string, tab = 0): LayoutNode => ({ kind: "leaf", id, tab }) as LayoutNode;
const row = (id: string, a: LayoutNode, b: LayoutNode, ratio = 0.5): LayoutNode =>
  ({ kind: "split", id, dir: "row", ratio, a, b }) as LayoutNode;
const col = (id: string, a: LayoutNode, b: LayoutNode, ratio = 0.5): LayoutNode =>
  ({ kind: "split", id, dir: "column", ratio, a, b }) as LayoutNode;

test("leafSizes: splits divide along their axis, reserving the divider", () => {
  const tree = row("S", leaf("a"), leaf("b"), 0.5);
  const sizes = leafSizes(tree, 1008, 600, PANE_DIVIDER_PX);
  // (1008 - 4) / 2 each; height passes through a row split untouched.
  assert.equal(sizes.get("a")?.w, 502);
  assert.equal(sizes.get("b")?.w, 502);
  assert.equal(sizes.get("a")?.h, 600);

  const stacked = col("S", leaf("a"), leaf("b"), 0.25);
  const colSizes = leafSizes(stacked, 800, 404, PANE_DIVIDER_PX);
  assert.equal(colSizes.get("a")?.h, 100); // (404 - 4) * 0.25
  assert.equal(colSizes.get("b")?.h, 300);
  assert.equal(colSizes.get("a")?.w, 800);
});

test("minSubtreeSize: a subtree's floor covers every leaf inside it at the CURRENT ratio", () => {
  const nested = row("S1", leaf("a"), row("S2", leaf("b"), leaf("c")));
  // Along "row" the nested pair's floor is max(min/r, min/(1-r)) + div — 644
  // at the balanced ratio. The root must then cover leaf a in its own ratio
  // share and the pair's 644 in the other share at ITS 0.5: the pair's demand
  // (644/0.5) dominates, so the root's floor is 1292 — not the naive 968 a
  // ratio-blind sum would claim. That larger number is what keeps an ancestor
  // divider drag from shrinking the pair into a sliver (#6560).
  assert.equal(
    minSubtreeSize(nested, "row", PANE_MIN_WIDTH_PX, PANE_DIVIDER_PX),
    (PANE_MIN_WIDTH_PX + PANE_DIVIDER_PX + PANE_MIN_WIDTH_PX) / 0.5 + PANE_DIVIDER_PX,
  );
  // A skewed split raises its own floor further: at ratio 0.25 the a-child
  // only ever gets a quarter of the content span, so it needs min/0.25 before
  // the sibling's min/0.75 is even the binding constraint.
  const skewed = row("S3", leaf("d"), leaf("e"), 0.25);
  assert.equal(
    minSubtreeSize(skewed, "row", PANE_MIN_WIDTH_PX, PANE_DIVIDER_PX),
    PANE_MIN_WIDTH_PX / 0.25 + PANE_DIVIDER_PX,
  );
  // Across the row axis the children overlap, so the larger floor governs.
  assert.equal(
    minSubtreeSize(nested, "column", PANE_MIN_HEIGHT_PX, PANE_DIVIDER_PX),
    PANE_MIN_HEIGHT_PX,
  );
});

test("minSubtreeSize: the visible set prices out fit-hidden leaves (#6573)", () => {
  // The #6573 deadlock: [(a|b)|(c|d)] at ~700px fits down to a,c — but the
  // divider's retained floors sum both leaves per side (644px each) against a
  // 696px span, so the clamp inverts and the drag can never move. With the
  // visible set each side asks only its one rendered leaf's floor.
  const tree = row("S1", row("S2", leaf("a"), leaf("b")), row("S3", leaf("c"), leaf("d")));
  const visible = new Set(["a", "c"]);
  assert.equal(
    minSubtreeSize(tree.a, "row", PANE_MIN_WIDTH_PX, PANE_DIVIDER_PX, visible),
    PANE_MIN_WIDTH_PX,
  );
  // A side whose leaves all hide contributes nothing — it takes no flex share
  // and its divider hides with it, so the other side's floor alone governs.
  const oneSided = row("S4", leaf("e"), leaf("f"));
  assert.equal(
    minSubtreeSize(oneSided, "row", PANE_MIN_WIDTH_PX, PANE_DIVIDER_PX, new Set(["e"])),
    PANE_MIN_WIDTH_PX,
  );
  // And the skewed-ratio math still applies among the visible leaves: hiding
  // the dominant child of a skewed pair drops the pair's demand to the
  // survivor's floor, not min/0.25.
  const skewed = row("S5", leaf("g"), leaf("h"), 0.25);
  assert.equal(
    minSubtreeSize(skewed, "row", PANE_MIN_WIDTH_PX, PANE_DIVIDER_PX, new Set(["h"])),
    PANE_MIN_WIDTH_PX,
  );
  // Both visible: ratio-aware coexistence plus the divider, as without a set.
  assert.equal(
    minSubtreeSize(skewed, "row", PANE_MIN_WIDTH_PX, PANE_DIVIDER_PX, new Set(["g", "h"])),
    PANE_MIN_WIDTH_PX / 0.25 + PANE_DIVIDER_PX,
  );
});

test("fitToSize: leaves under the floor drop out, rightmost violator first", () => {
  // Four equal row panes into 700px of host: each would get ~174px — under the
  // floor. d goes first; then c fills the freed right half at 348px (healthy)
  // while a,b remain 174px slivers, so b goes next — keeping a,c is the
  // minimal-hide outcome (hiding c instead would cost the same count and fix
  // nothing extra).
  const tree = row("S1", row("S2", leaf("a"), leaf("b")), row("S3", leaf("c"), leaf("d")));
  const { tree: fit, hidden } = fitToSize(tree, 700, 600, null);
  assert.deepEqual(leaves(fit).map((l) => l.id), ["a", "c"]);
  assert.deepEqual([...hidden].sort(), ["b", "d"]);
});

test("fitToSize: the protected (focused) leaf hides last", () => {
  const tree = row("S1", row("S2", leaf("a"), leaf("b")), row("S3", leaf("c"), leaf("d")));
  const { tree: fit, hidden } = fitToSize(tree, 700, 600, "c");
  const kept = leaves(fit).map((l) => l.id);
  assert.ok(kept.includes("c"), "the focused pane is never the one that disappears");
  assert.equal(kept.length, 2);
  assert.equal(hidden.length, 2);
});

test("fitToSize: a single pane is exempt — it always shows", () => {
  const tree = leaf("only");
  const { tree: fit, hidden } = fitToSize(tree, 100, 50, "only");
  assert.equal(fit, tree);
  assert.deepEqual(hidden, []);
});

test("fitToSize: a violating leaf drops before a healthy one pays for it", () => {
  // 0.15/0.85 into 800px: a is 120px (a sliver), b is 676px — dropping the
  // sliver restores the floor; b must not be hidden to save a.
  const tree = row("S", leaf("a"), leaf("b"), 0.15);
  const { tree: fit, hidden } = fitToSize(tree, 800, 600, null);
  assert.deepEqual(leaves(fit).map((l) => l.id), ["b"]);
  assert.deepEqual(hidden, ["a"]);
});

test("fitToSize: a healthy leaf CAN hide when the protected one needs the space", () => {
  // a is the focused 120px sliver; dropping a cannot help (it IS protected),
  // so the healthy b is the one that goes — the TUI hides unfocused panes to
  // keep the user's attention on screen.
  const tree = row("S", leaf("a"), leaf("b"), 0.15);
  const { tree: fit, hidden } = fitToSize(tree, 800, 600, "a");
  assert.deepEqual(leaves(fit).map((l) => l.id), ["a"]);
  assert.deepEqual(hidden, ["b"]);
});

test("fitToSize: column splits fit on the height floor", () => {
  // Three stacked panes into 200px: a,b get 49px and c gets 98px — a,b are the
  // slivers, so b (the bottom-most violator) goes and a,c split the height.
  const tree = col("S1", col("S2", leaf("a"), leaf("b")), leaf("c"));
  const { tree: fit, hidden } = fitToSize(tree, 800, 200, null);
  assert.deepEqual(leaves(fit).map((l) => l.id), ["a", "c"]);
  assert.deepEqual(hidden, ["b"]);
});

test("fitToSize: when every leaf clears the floor nothing is hidden", () => {
  const tree = row("S", leaf("a"), leaf("b"));
  const { tree: fit, hidden } = fitToSize(tree, 1200, 600, null);
  assert.equal(fit, tree);
  assert.deepEqual(hidden, []);
});
