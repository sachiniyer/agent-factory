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
  remapByIdentity,
  replaceTab,
  resetIds,
  sameLayout,
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
