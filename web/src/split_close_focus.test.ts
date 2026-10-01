// Pins the close-focused-pane focus successor: closing the pane that holds the
// keyboard must move focus to the SIBLING subtree that expands to fill the closed
// pane's space — not to the leftmost leaf of the whole tree, which for a nested
// close in a non-leftmost branch lives in an UNRELATED root branch and intercepts
// the keystrokes (#1737 latent issue).
//
// closeLeaf() (layout.ts) collapses the closed leaf's parent split and substitutes
// the surviving sibling, so the sibling is the pane/region that takes over the
// screen. closePane ignored it and reset focusedId to leaves(this.tree)[0] — the
// leftmost leaf of the ENTIRE updated tree — so refocus() → focus() called
// term.focus() on an unrelated pane whenever the closed focused pane lived in a
// nested split inside a non-leftmost root branch. reconcile()'s validity guard
// (split.ts:786) never corrected it: a mis-targeted but VALID surviving leaf id
// passes `wanted.has(focusedId)`. These stage the pane map directly
// (split_focus.test.ts precedent); the end-to-end behavior through a real xterm
// textarea is pinned by the Playwright selftest in web/selftest/web-driver.spec.ts.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import type { SplitCallbacks, SplitView as SplitViewType } from "./split.js";

// split.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of which plain
// node can load. Stub them out before importing the module (split_focus.test.ts
// precedent), then import dynamically so the hook is registered first.
register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

/** The private state closePane() / refocus() / focus() read and write. Mirrors the
 *  shape of SplitView's fields; the cast is confined here so the tests below read
 *  as ordinary calls (split_focus.test.ts precedent). */
type SplitViewInternals = {
  panes: Map<string, { term: { focus: () => void; blur: () => void } | null }>;
  tree: unknown;
  focusedId: string | null;
  termHoldsFocus: boolean;
  cb: SplitCallbacks;
};

function noopCallbacks(): SplitCallbacks {
  return {
    onStatus: () => {},
    onFocusChange: () => {},
    onLayout: () => {},
  };
}

/** A terminal-pane fake whose term.focus() count is observable. Mirrors fakePane()
 *  in split_focus.test.ts (a bare focus counter; blur is a no-op here because the
 *  sibling path returns true from focus() and never reaches blur()). */
function fakeTerm(): { term: { focus: () => void; blur: () => void }; count: () => number } {
  let n = 0;
  return { term: { focus: () => { n++; }, blur: () => {} }, count: () => n };
}

function closeFocusedPane(view: SplitViewType): void {
  (view as unknown as { closeFocusedPane: () => void }).closeFocusedPane();
}

function closePane(view: SplitViewType, leafId: string): void {
  (view as unknown as { closePane: (id: string) => void }).closePane(leafId);
}

/** Stages the 3-pane tree split(row, A, split(col, B, C)) with a fake terminal per
 *  leaf and `focusedId` preset. ids are deterministic under resetIds(): A=leaf1,
 *  B=leaf2, C=leaf4 (the split node consumes id 3). */
async function stageThreePanes(focusedLeaf: "A" | "B" | "C") {
  const { leaves, resetIds, singleLeaf, splitLeaf } = await import("./layout.js");
  resetIds();
  const one = singleLeaf(0);
  const two = splitLeaf(one, one.id, "right", 1);
  const r = leaves(two)[1];
  const three = splitLeaf(two, r.id, "bottom", 2); // split(row, A, split(col, B, C))
  const [A, B, C] = leaves(three).map((l) => l.id);

  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  const in_ = view as unknown as SplitViewInternals;
  in_.tree = three;
  const tfA = fakeTerm(), tfB = fakeTerm(), tfC = fakeTerm();
  in_.panes.set(A, { term: tfA.term });
  in_.panes.set(B, { term: tfB.term });
  in_.panes.set(C, { term: tfC.term });
  in_.focusedId = { A, B, C }[focusedLeaf];
  return { view, in_, A, B, C, tfA, tfB, tfC };
}

// --- the bug: a nested close in a non-leftmost branch must focus the sibling -----

test("closeFocusedPane: closing focused B lands focus on the sibling C, not leftmost A", async () => {
  const { view, in_, A, B, C, tfA, tfB, tfC } = await stageThreePanes("B");
  // B holds focus; close it. C is the sibling that absorbs B's space.
  closeFocusedPane(view);
  assert.equal(in_.focusedId, C, `after closing focused B, focusedId is ${in_.focusedId} — must be the sibling C, not the leftmost A`);
  assert.equal(tfC.count(), 1, "the sibling C that absorbed the closed pane's space receives term.focus()");
  assert.equal(tfA.count(), 0, "the unrelated leftmost A must NOT receive term.focus() — keystrokes would go to the wrong session");
  assert.equal(tfB.count(), 0, "the disposed pane's terminal is never focused");
});

// --- the cases that already worked by accident must keep working -----------------

test("closeFocusedPane: closing the leftmost branch's pane focuses the sibling subtree's first leaf", async () => {
  // Close A (the root's left branch): the sibling subtree is split(col, B, C), whose
  // first leaf is B — which coincidentally IS leaves(newTree)[0]. Both the old and
  // new formula agree here; the fix must not regress it.
  const { view, in_, A, B, tfB } = await stageThreePanes("A");
  closeFocusedPane(view);
  assert.equal(in_.focusedId, B, "closing the leftmost A focuses the sibling subtree's first leaf B");
  assert.equal(tfB.count(), 1, "the sibling subtree's first leaf (B) receives term.focus()");
  void A;
});

// --- 4-pane: closing in the right root branch trips the bug, left branch does not -

/** Stages split(row, split(col, A, B), split(col, C, D)) with fake terminals. */
async function stageFourPanes(focusedLeaf: "A" | "B" | "C" | "D") {
  const { leaves, resetIds, singleLeaf, splitLeaf } = await import("./layout.js");
  resetIds();
  const one = singleLeaf(0);
  const two = splitLeaf(one, one.id, "right", 1); // split(row, leaf1, leaf2)
  const [leftId, rightId] = leaves(two).map((l) => l.id);
  const leftThree = splitLeaf(two, leftId, "bottom", 2); // left → split(col, A, B)
  const four = splitLeaf(leftThree, rightId, "bottom", 3); // right → split(col, C, D)
  const [A, B, C, D] = leaves(four).map((l) => l.id);

  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  const in_ = view as unknown as SplitViewInternals;
  in_.tree = four;
  const tfA = fakeTerm(), tfB = fakeTerm(), tfC = fakeTerm(), tfD = fakeTerm();
  in_.panes.set(A, { term: tfA.term });
  in_.panes.set(B, { term: tfB.term });
  in_.panes.set(C, { term: tfC.term });
  in_.panes.set(D, { term: tfD.term });
  in_.focusedId = { A, B, C, D }[focusedLeaf];
  return { view, in_, A, B, C, D, tfA, tfB, tfC, tfD };
}

test("closeFocusedPane: 4-pane, closing focused C in the right branch focuses sibling D", async () => {
  const { view, in_, A, B, C, D, tfA, tfD } = await stageFourPanes("C");
  closeFocusedPane(view);
  assert.equal(in_.focusedId, D, "closing C focuses its sibling D, not the leftmost A/B");
  assert.equal(tfD.count(), 1, "the sibling D receives term.focus()");
  assert.equal(tfA.count(), 0, "leftmost A in a different root branch stays unfocused");
  void B; void C;
});

test("closeFocusedPane: 4-pane, closing focused B in the LEFT branch focuses sibling A", async () => {
  // The left branch's leaves ARE the leftmost-of-whole-tree, so the old formula
  // already picked A here; the fix must keep it.
  const { view, in_, A, B, tfA } = await stageFourPanes("B");
  closeFocusedPane(view);
  assert.equal(in_.focusedId, A, "closing B in the left branch focuses its sibling A");
  assert.equal(tfA.count(), 1);
  void B;
});

// --- a 2-pane close: the sibling IS the whole new tree (no nesting) --------------

test("closeFocusedPane: 2-pane close focuses the surviving sibling", async () => {
  const { leaves, resetIds, singleLeaf, splitLeaf } = await import("./layout.js");
  resetIds();
  const one = singleLeaf(0);
  const two = splitLeaf(one, one.id, "right", 1);
  const [A, B] = leaves(two).map((l) => l.id);

  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  const in_ = view as unknown as SplitViewInternals;
  in_.tree = two;
  const tfA = fakeTerm(), tfB = fakeTerm();
  in_.panes.set(A, { term: tfA.term });
  in_.panes.set(B, { term: tfB.term });
  in_.focusedId = B;

  closeFocusedPane(view);

  assert.equal(in_.focusedId, A, "closing the only sibling focuses it");
  assert.equal(tfA.count(), 1, "the surviving sibling receives term.focus()");
  assert.equal(tfB.count(), 0);
});

// --- closing a NON-focused pane must not move focus ------------------------------

test("closePane: closing a pane that did NOT hold focus leaves focus on the current pane", async () => {
  // Focus is on A; close B (a non-focused nested pane in the right branch). Focus
  // must stay on A, and refocus() re-focuses A's terminal (the focus re-point branch
  // is skipped because the closed pane did not hold focus).
  const { view, in_, A, B, tfA } = await stageThreePanes("A");
  closePane(view, B);
  assert.equal(in_.focusedId, A, "focus stays on A when an unrelated pane is closed");
  assert.equal(tfA.count(), 1, "A's terminal is re-focused (refocus always runs)");
});

// --- the last pane can't be closed ------------------------------------------------

test("closeFocusedPane: closing the only pane is a no-op (the last pane can't be closed)", async () => {
  const { resetIds, singleLeaf } = await import("./layout.js");
  resetIds();
  const t = singleLeaf(0);
  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  const in_ = view as unknown as SplitViewInternals;
  in_.tree = t;
  const tf = fakeTerm();
  in_.panes.set(t.id, { term: tf.term });
  in_.focusedId = t.id;
  closeFocusedPane(view);
  assert.equal(in_.focusedId, t.id, "the single pane stays focused");
  assert.equal(tf.count(), 0, "nothing is focused because the close was a no-op");
});

test("closeFocusedPane: closing the focused terminal onto a web-pane sibling reports rail mode", async () => {
  const { view, in_, A, B, C, tfA } = await stageThreePanes("B");
  in_.panes.set(C, { term: null });
  in_.termHoldsFocus = true;
  let lastFocus: boolean | null = null;
  in_.cb.onFocusChange = (f) => { lastFocus = f; };

  closeFocusedPane(view);

  assert.equal(in_.focusedId, C, "focus moves to the sibling subtree's first leaf C");
  assert.equal(lastFocus, false, "rail mode is reported through onFocusChange(false)");
  assert.equal(in_.termHoldsFocus, false, "termHoldsFocus is cleared to match the body-focused DOM");
  assert.equal(tfA.count(), 0, "the unrelated A terminal is never focused");
  void A; void B;
});
