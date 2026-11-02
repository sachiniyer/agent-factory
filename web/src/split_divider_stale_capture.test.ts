// Pins the fix for the stale-capture divergence in the split-pane divider drag
// (#6555 class): after a first drag persists (rebuilding this.tree via setRatio,
// which allocates fresh split-node objects), a SECOND drag of that divider (or
// any other divider) used to mutate only the stale node captured in the closure
// — the node this.builtTree still references — diverging its ratio from
// this.tree. Any reconcile() that fired mid-gesture (ResizeObserver → rAF, or an
// out-of-band setSession roster update) then failed the sameLayout gate and ran
// host.replaceChildren(rootEl), rebuilding the whole split DOM during the drag:
// re-inserting pane containers destroys <iframe> browsing contexts, rewinds xterm
// viewports, detaches the actively-dragged divider (so pointerup never fires, the
// ratio is not persisted, and the af-resizing-* body classes stay stuck — forcing
// cursor: col/row-resize !important and user-select: none over the entire app).
//
// The fix re-resolves the split node from the CURRENT this.tree by id inside
// pointerdown (findSplitById), so the in-place ratio write lands on the live tree
// object — and onUp aliases this.builtTree back to the post-setRatio tree, keeping
// sameLayout closed across consecutive drags. A pointercancel handler is added so
// the body classes and listeners can never leak even if the browser cancels the
// gesture.
//
// The test installs a hand-rolled DOM (same pattern as split_preview_reachable),
// stages the post-reconcile state (this.tree === this.builtTree), obtains a divider
// from the REAL SplitView.buildDivider, drives two complete real drags, and
// asserts the gate stays closed mid-gesture — plus a reconcile() that reaches the
// same gate and must NOT call host.replaceChildren.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import { findSplitById, leaves, type LayoutNode, resetIds, sameLayout, setRatio, singleLeaf, type SplitNode, splitLeaf } from "./layout.js";
import type { SplitCallbacks, SplitView as SplitViewType } from "./split.js";

// split.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of which plain
// node can load. Stub them out before importing the module (split_focus.test.ts
// precedent), then import dynamically so the hook is registered first.
register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

// --- hand-rolled DOM stub ------------------------------------------------------

/** A minimal element stub that supports the surface buildDivider / reconcile()
 *  touch: addEventListener/removeEventListener (with a dispatch helper),
 *  setAttribute/getAttribute, style, classList, append/replaceChildren, remove,
 *  getBoundingClientRect, setPointerCapture/releasePointerCapture, parentElement,
 *  contains, textContent, hidden, dataset. */
function makeEl(tag = "div"): any {
  const listeners = new Map<string, Set<(e: any) => void>>();
  const classSet = new Set<string>();
  const children: any[] = [];
  const style: Record<string, string> = {};
  const el: any = {
    tagName: tag,
    nodeName: tag.toUpperCase(),
    className: "",
    style,
    hidden: false,
    textContent: "",
    dataset: {},
    parentElement: null as any,
    attrs: {} as Record<string, string>,
    classList: {
      add: (...cs: string[]) => void cs.forEach((c) => classSet.add(c)),
      remove: (...cs: string[]) => void cs.forEach((c) => classSet.delete(c)),
      toggle: (c: string, on?: boolean) => {
        if (on === undefined) on = !classSet.has(c);
        classSet[on ? "add" : "delete"](c);
      },
      contains: (c: string) => classSet.has(c),
    },
    setAttribute: (k: string, v: string) => { el.attrs[k] = String(v); },
    getAttribute: (k: string) => el.attrs[k] ?? null,
    removeAttribute: (k: string) => { delete el.attrs[k]; },
    addEventListener: (type: string, fn: (e: any) => void) => {
      let set = listeners.get(type);
      if (!set) { set = new Set(); listeners.set(type, set); }
      set.add(fn);
    },
    removeEventListener: (type: string, fn: (e: any) => void) => {
      listeners.get(type)?.delete(fn);
    },
    dispatch: (type: string, e: any) => {
      const set = listeners.get(type);
      if (set) for (const fn of [...set]) fn(e);
    },
    hasListener: (type: string) => (listeners.get(type)?.size ?? 0) > 0,
    append: (...kids: any[]) => { children.push(...kids); },
    replaceChildren: (...kids: any[]) => { children.length = 0; children.push(...kids); },
    remove: () => {},
    getBoundingClientRect: () => ({ width: 1000, height: 600, left: 0, top: 0, right: 1000, bottom: 600 }),
    setPointerCapture: () => {},
    releasePointerCapture: () => {},
    contains: (_n: any) => false,
    focus: () => {},
    blur: () => {},
  };
  return el;
}

/** Installs global document/window stubs that let buildDivider and reconcile()
 *  run under node. Returns a recorder for host.replaceChildren calls. Restored
 *  on test completion. */
function stubDom(t: import("node:test").TestContext): {
  body: any;
  replaceChildrenCalls: number;
  resetReplaceChildren: () => void;
} {
  const body = makeEl("body");
  let replaceChildrenCalls = 0;

  const doc: any = {
    body,
    createElement: (_tag: string) => makeEl(_tag),
    createElementNS: (_ns: string, _tag: string) => makeEl(_tag),
    activeElement: null,
    addEventListener: () => {},
    removeEventListener: () => {},
  };

  const win: any = {
    matchMedia: () => ({ matches: false }),
    setTimeout: (fn: () => void) => setTimeout(fn, 0),
    clearTimeout: (h: ReturnType<typeof setTimeout>) => clearTimeout(h),
    addEventListener: () => {},
    removeEventListener: () => {},
    innerHeight: 600,
  };

  const prevDoc = Object.getOwnPropertyDescriptor(globalThis, "document");
  const prevWin = Object.getOwnPropertyDescriptor(globalThis, "window");
  Object.defineProperty(globalThis, "document", { configurable: true, value: doc });
  Object.defineProperty(globalThis, "window", { configurable: true, value: win });
  t.after(() => {
    if (prevDoc) Object.defineProperty(globalThis, "document", prevDoc);
    else Reflect.deleteProperty(globalThis, "document");
    if (prevWin) Object.defineProperty(globalThis, "window", prevWin);
    else Reflect.deleteProperty(globalThis, "window");
  });

  return {
    body,
    get replaceChildrenCalls() { return replaceChildrenCalls; },
    resetReplaceChildren: () => { replaceChildrenCalls = 0; },
  };
}

// --- SplitView internals access ------------------------------------------------

type SplitViewInternals = {
  tree: LayoutNode | null;
  builtTree: LayoutNode | null;
  displayTree: LayoutNode | null;
  builtRootEl: HTMLElement | null;
  sessionId: string | null;
  token: string | null;
  tabIds: string[];
  tabKinds: number[];
  tabRealIds: string[];
  tabTargets: (string | undefined)[];
  tabNames: string[];
  tabWebProxied: (boolean | undefined)[];
  focusedId: string | null;
  termHoldsFocus: boolean;
  panes: Map<string, any>;
  narrowEl: HTMLElement | null;
  host: HTMLElement;
  buildDivider: (node: SplitNode, aEl: HTMLElement, bEl: HTMLElement) => HTMLElement;
  buildNode: (node: LayoutNode) => HTMLElement;
  reconcile: () => void;
  retain: (sessionId: string, tree: LayoutNode, ids: string[]) => void;
  proposalRatios: Map<string, number>;
};

function noopCallbacks(): SplitCallbacks {
  return { onStatus: () => {}, onFocusChange: () => {}, onLayout: () => {} };
}

/** A pane stub with the surface reconcile() touches: container (classList,
 *  setAttribute, contains), host, label (textContent), glyph (replaceChildren),
 *  a non-null term so the terminal-rebuild branch does not fire, and the
 *  identity/tab matching the current roster so staleAddress is false. */
function makePane(leafId: string, tab: number, identity: string): any {
  return {
    leafId,
    container: makeEl("div"),
    host: makeEl("div"),
    label: makeEl("span"),
    glyph: makeEl("span"),
    overlay: makeEl("div"),
    term: { dispose: () => {} },
    tab,
    identity,
    status: "open",
    webUrl: null,
    webDispose: null,
    webArchived: false,
    iframeProxied: undefined,
  };
}

/** Stages a 2-pane split(row, A, B) with the post-reconcile state
 *  (this.tree === this.builtTree) and panes pre-populated so reconcile() can
 *  reach the sameLayout gate without creating new terminals. Returns the view,
 *  internals, the split node, the divider, and the a/b pane containers. */
function stageSplit(t: import("node:test").TestContext): {
  internals: SplitViewInternals;
  splitNode: SplitNode;
  divider: HTMLElement;
  aEl: HTMLElement;
  bEl: HTMLElement;
  body: any;
  replaceChildrenCalls: () => number;
  resetReplaceChildren: () => void;
} {
  const dom = stubDom(t);

  resetIds();
  const one = singleLeaf(0);
  const two = splitLeaf(one, one.id, "right", 1);
  const [A, B] = leaves(two).map((l) => l.id);
  const splitNode = two.kind === "split" ? two : (null as unknown as SplitNode);
  assert.ok(splitNode, "staged tree must be a split");

  const host = makeEl("div");
  (host as any).clientWidth = 0;
  (host as any).clientHeight = 0;
  const replaceChildrenRecorder = (host as any).replaceChildren;
  let rcCalls = 0;
  (host as any).replaceChildren = (...kids: any[]) => {
    rcCalls++;
    return replaceChildrenRecorder.call(host, ...kids);
  };

  const view = new SplitView(host as unknown as HTMLElement, noopCallbacks());
  const internals = view as unknown as SplitViewInternals;

  // Stage the post-reconcile state: tree === builtTree, panes populated.
  internals.tree = two;
  internals.builtTree = two;
  internals.displayTree = null;
  internals.builtRootEl = null;
  internals.sessionId = "s1";
  internals.token = "t1";
  internals.tabIds = ["a", "b"];
  internals.tabKinds = [0, 0];
  internals.tabRealIds = ["", ""];
  internals.tabTargets = [];
  internals.tabNames = [];
  internals.tabWebProxied = [];
  internals.focusedId = A;
  internals.termHoldsFocus = false;
  internals.narrowEl = null;
  internals.panes.set(A, makePane(A, 0, "a"));
  internals.panes.set(B, makePane(B, 1, "b"));

  // Build a real divider from the real buildDivider, with real a/b containers.
  const aEl = internals.panes.get(A).container as HTMLElement;
  const bEl = internals.panes.get(B).container as HTMLElement;
  const divider = internals.buildDivider(splitNode, aEl, bEl);

  // Wire the divider into a parent so pointerdown's parentElement check passes.
  const parent = makeEl("div");
  (divider as any).parentElement = parent;

  return {
    internals,
    splitNode,
    divider,
    aEl,
    bEl,
    body: dom.body,
    replaceChildrenCalls: () => rcCalls,
    resetReplaceChildren: () => { rcCalls = 0; },
  };
}

/** Drives a complete drag gesture: pointerdown at (startX, startY), pointermove
 *  to (moveX, moveY), then pointerup at (upX, upY). Returns the ratio the onMove
 *  wrote (read from the divider's aEl flex value). */
function drag(
  divider: HTMLElement,
  startX: number,
  startY: number,
  moveX: number,
  moveY: number,
  upX: number,
  upY: number,
): void {
  const d = divider as any;
  d.dispatch("pointerdown", { pointerId: 1, clientX: startX, clientY: startY, preventDefault: () => {} });
  d.dispatch("pointermove", { pointerId: 1, clientX: moveX, clientY: moveY, preventDefault: () => {} });
  d.dispatch("pointerup", { pointerId: 1, clientX: upX, clientY: upY, preventDefault: () => {} });
}

// --- the bug: a second drag must not diverge builtTree from tree ---------------

test("divider drag: after a persisted drag, a second drag's pointermove keeps sameLayout(tree, builtTree) true", (t) => {
  const { internals, divider, body } = stageSplit(t);
  const original = internals.tree!;

  // Drag #1: move the divider to ratio ~0.50 (clientX=500, span=996, want=0.502).
  // The parent rect is 1000px wide; the pane-min clamp bounds the ratio to
  // [0.32, 0.68] (320px / 996px), so 0.50 is well inside.
  drag(divider, 500, 0, 500, 0, 500, 0);

  // setRatio rebuilt every split node, so this.tree is a fresh object.
  assert.notEqual(internals.tree, original, "drag #1's setRatio must rebuild this.tree (fresh split nodes)");
  // The gate stays closed: the DOM already shows the dragged ratio.
  assert.equal(sameLayout(internals.tree, internals.builtTree), true, "sameLayout(tree, builtTree) stays true after drag #1");

  // Drag #2: move the divider to ratio ~0.60 (clientX=600, want=0.602).
  // This is a DIFFERENT ratio than drag #1, so without the fix the stale captured
  // node's mutation would diverge builtTree from tree.
  const d = divider as any;
  d.dispatch("pointerdown", { pointerId: 1, clientX: 500, clientY: 0, preventDefault: () => {} });
  d.dispatch("pointermove", { pointerId: 1, clientX: 600, clientY: 0, preventDefault: () => {} });

  // THE BUG: without the fix, onMove mutates the stale captured node (in the old
  // graph builtTree references), diverging its ratio from this.tree. sameLayout
  // would flip to false, and a reconcile() at that instant would take the
  // replaceChildren branch — rebuilding the split DOM mid-gesture.
  assert.equal(
    sameLayout(internals.tree, internals.builtTree),
    true,
    "sameLayout(tree, builtTree) must stay true during drag #2's pointermove — the gate must not flip mid-gesture",
  );

  // The af-resizing body class was added by pointerdown and must still be present
  // (the drag is in flight).
  assert.ok(body.classList.contains("af-resizing-col"), "the af-resizing-col body class is present during the drag");

  // Finish drag #2.
  d.dispatch("pointerup", { pointerId: 1, clientX: 600, clientY: 0, preventDefault: () => {} });

  // The body classes must be cleaned up by pointerup.
  assert.ok(!body.classList.contains("af-resizing-col"), "af-resizing-col is removed after pointerup");
  assert.ok(!body.classList.contains("af-resizing-row"), "af-resizing-row is removed after pointerup");
});

// --- the gate: a reconcile() mid-gesture must not call replaceChildren ----------

test("divider drag: a reconcile() during drag #2 does NOT call host.replaceChildren (gate stays closed)", (t) => {
  const { internals, divider, replaceChildrenCalls, resetReplaceChildren } = stageSplit(t);

  // Drag #1: persist a ratio (~0.50).
  drag(divider, 500, 0, 500, 0, 500, 0);
  assert.equal(sameLayout(internals.tree, internals.builtTree), true, "gate closed after drag #1");

  // Drag #2: pointerdown + pointermove only (mid-gesture, ratio → ~0.60).
  const d = divider as any;
  d.dispatch("pointerdown", { pointerId: 1, clientX: 500, clientY: 0, preventDefault: () => {} });
  d.dispatch("pointermove", { pointerId: 1, clientX: 600, clientY: 0, preventDefault: () => {} });

  // A reconcile() fires mid-gesture (ResizeObserver → rAF, or setSession roster
  // update). With the fix, sameLayout(tree, builtTree) is true, so the
  // replaceChildren rebuild branch is NOT taken.
  resetReplaceChildren();
  assert.doesNotThrow(() => internals.reconcile(), "reconcile() must not throw when called mid-gesture");
  assert.equal(replaceChildrenCalls(), 0, "reconcile() must NOT call host.replaceChildren during a drag (the gate stays closed)");

  // Finish the drag so the body classes are cleaned up.
  d.dispatch("pointerup", { pointerId: 1, clientX: 600, clientY: 0, preventDefault: () => {} });
});

// --- the ratio persists on pointerup after a second drag ------------------------

test("divider drag: the dragged ratio is persisted into this.tree on pointerup after a second drag", (t) => {
  const { internals, divider } = stageSplit(t);

  // Drag #1: ratio → ~0.50 (clientX=500, span=996, want=0.502).
  drag(divider, 500, 0, 500, 0, 500, 0);
  const after1 = internals.tree!;
  assert.ok(after1.kind === "split", "tree is a split after drag #1");
  const ratio1 = (after1 as SplitNode).ratio;
  assert.ok(ratio1 > 0.45 && ratio1 < 0.55, `drag #1 persisted ratio ~0.50, got ${ratio1}`);

  // Drag #2: ratio → ~0.60 (clientX=600, want=0.602).
  drag(divider, 500, 0, 600, 0, 600, 0);
  const after2 = internals.tree!;
  assert.ok(after2.kind === "split", "tree is a split after drag #2");
  const ratio2 = (after2 as SplitNode).ratio;
  assert.ok(ratio2 > 0.55 && ratio2 < 0.65, `drag #2 persisted ratio ~0.60, got ${ratio2}`);
  assert.notEqual(ratio1, ratio2, "drag #2 persisted a different ratio than drag #1");
});

// --- pointercancel: the body classes and listeners must not leak ----------------

test("divider drag: a pointercancel cleans up the af-resizing body classes and removes listeners", (t) => {
  const { divider, body } = stageSplit(t);
  const d = divider as any;

  // Start a drag.
  d.dispatch("pointerdown", { pointerId: 1, clientX: 500, clientY: 0, preventDefault: () => {} });
  assert.ok(body.classList.contains("af-resizing-col"), "af-resizing-col is added on pointerdown");

  // The browser cancels the gesture (system gesture takeover, accessibility cancel).
  d.dispatch("pointercancel", { pointerId: 1, clientX: 500, clientY: 0, preventDefault: () => {} });

  // The body classes must be cleaned up — without the pointercancel handler they
  // would stay stuck, pinning cursor: col/row-resize !important and user-select:
  // none over the entire app until another drag completed.
  assert.ok(!body.classList.contains("af-resizing-col"), "af-resizing-col is removed on pointercancel");
  assert.ok(!body.classList.contains("af-resizing-row"), "af-resizing-row is removed on pointercancel");

  // The move/up listeners must be removed so a stale pointermove does not fire.
  assert.ok(!d.hasListener("pointermove"), "pointermove listener is removed on pointercancel");
  assert.ok(!d.hasListener("pointerup"), "pointerup listener is removed on pointercancel");
  assert.ok(!d.hasListener("pointercancel"), "pointercancel listener is removed on pointercancel");
});

// --- findSplitById: the helper itself ------------------------------------------

test("findSplitById: returns the split node with the given id, or null", () => {
  resetIds();
  const one = singleLeaf(0);
  const two = splitLeaf(one, one.id, "right", 1);
  assert.ok(two.kind === "split", "staged tree is a split");
  const splitId = (two as SplitNode).id;
  assert.ok(findSplitById(two, splitId) === two, "findSplitById returns the root split itself");
  assert.equal(findSplitById(two, "nonexistent"), null, "findSplitById returns null for a missing id");
  assert.equal(findSplitById(one, "any"), null, "findSplitById returns null for a leaf root");
});

// --- regression: a single drag still works (no false gate flip) ----------------

test("divider drag: a single drag persists the ratio and cleans up the body classes", (t) => {
  const { internals, divider, body } = stageSplit(t);
  const before = (internals.tree as SplitNode).ratio;

  // Drag to ratio ~0.40 (clientX=400, span=996, want=0.402, within clamp [0.32, 0.68]).
  drag(divider, 500, 0, 400, 0, 400, 0);

  const after = internals.tree as SplitNode;
  assert.ok(after.ratio !== before, "the ratio changed after a drag");
  assert.ok(!body.classList.contains("af-resizing-col"), "body classes are cleaned up after pointerup");
  assert.ok(!body.classList.contains("af-resizing-row"), "body classes are cleaned up after pointerup");
});
