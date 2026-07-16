// Pins the focus history a SELF-SPLIT reads (#1901) — dragging the tab a pane already
// shows onto its own edge, which must open a DIFFERENT tab in the new half rather than
// collapse back to a no-op. companionTab picks that tab (layout.test.ts covers the
// choice); what these pin is the PREFERENCE fed to it: the tab the user was last on.
//
// The history is recorded by stable daemon id, never by ordinal, because the roster
// shifts underneath it (#1779) — a remembered ordinal names a different tab the moment
// a lower tab closes, which is the misroute the stable id exists to prevent. That is
// the property worth a test: it cannot be seen from the tree, and the Playwright
// selftest cannot force a reorder mid-history.
//
// These construct SplitView directly and stage its private state rather than driving
// setSession(), for the same reason split_focus.test.ts does: the real path needs a
// DOM, an xterm and a live WS, none of which npm test has, and none of which this
// contract depends on.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import { type LayoutNode, resetIds, singleLeaf } from "./layout.js";
import type { SplitCallbacks, SplitView as SplitViewType } from "./split.js";

// split.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of which plain node
// can load. Stub them out before importing the module. See split_focus.test.ts.
register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

/** The private state report()/preferredTabs() read and write. The cast is confined
 *  here so the tests below read as ordinary calls. */
type SplitViewInternals = {
  tree: LayoutNode | null;
  focusedId: string | null;
  tabRealIds: string[];
  tabMru: string[];
  lastFocusedTabId: string;
  report: () => void;
  preferredTabs: () => number[];
  forgetFocusHistory: () => void;
};

function noopCallbacks(): SplitCallbacks {
  return {
    onStatus: () => {},
    onFocusChange: () => {},
    onLayout: () => {},
  };
}

function stage(tabRealIds: string[]): SplitViewInternals {
  resetIds();
  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  const internals = view as unknown as SplitViewInternals;
  internals.tabRealIds = tabRealIds;
  return internals;
}

/** Focuses `tab` in the (single) pane and lets report() record the move — the path
 *  every real focus change funnels through. */
function focusTab(v: SplitViewInternals, tab: number): void {
  const leaf = singleLeaf(tab);
  v.tree = leaf;
  v.focusedId = leaf.id;
  v.report();
}

test("focus history: the tab you were last on is the preferred companion", () => {
  const v = stage(["id-agent", "id-term"]);
  focusTab(v, 0); // land on Agent
  focusTab(v, 1); // switch to Terminal — Agent becomes "previously focused"
  assert.deepEqual(v.preferredTabs(), [0], "a self-split on Terminal reopens Agent");
});

test("focus history: most-recent first, and a tab is never listed twice", () => {
  const v = stage(["a", "b", "c"]);
  focusTab(v, 0);
  focusTab(v, 1);
  focusTab(v, 2);
  assert.deepEqual(v.preferredTabs(), [1, 0], "most recently left tab comes first");
  focusTab(v, 0); // revisiting 0 re-dates it rather than duplicating it
  assert.deepEqual(v.preferredTabs(), [2, 1], "0 is current, so it leaves the history");
});

test("focus history: a REORDER moves the preference with the tab, not the slot (#1779)", () => {
  const v = stage(["a", "b", "c"]);
  focusTab(v, 0); // "a"
  focusTab(v, 2); // "c" — the history now holds "a", which sits at ordinal 0
  assert.deepEqual(v.preferredTabs(), [0]);
  // Another client creates a tab ahead of "a": every later tab shifts up, so "a" is at
  // ordinal 1 now. Had the history stored the ORDINAL, it would still say 0 — and offer
  // the stranger "x" as the companion.
  v.tabRealIds = ["x", "a", "c"];
  assert.deepEqual(v.preferredTabs(), [1], "the preference follows the tab to its new ordinal");
});

test("focus history: a tab closed since drops out instead of binding a stranger", () => {
  const v = stage(["a", "b"]);
  focusTab(v, 0); // "a"
  focusTab(v, 1); // history: "a"
  v.tabRealIds = ["b"]; // "a" was closed
  assert.deepEqual(v.preferredTabs(), [], "no ordinal is offered for a tab that is gone");
});

test("focus history: a tab with no daemon id never enters the history", () => {
  // A legacy/pre-#1738 tab has no collision-proof handle, so it is not remembered; the
  // next-in-order walk in companionTab still covers it.
  const v = stage(["", "id-term"]);
  focusTab(v, 0); // the id-less tab
  focusTab(v, 1);
  assert.deepEqual(v.preferredTabs(), []);
});

test("focus history: it is scoped to the session — a switch forgets it", () => {
  const v = stage(["a", "b"]);
  focusTab(v, 0);
  focusTab(v, 1);
  assert.deepEqual(v.preferredTabs(), [0]);
  v.forgetFocusHistory(); // what setSession does when the shown session changes
  assert.deepEqual(v.preferredTabs(), [], "another session's tabs are not preferences here");
  assert.equal(v.lastFocusedTabId, "");
});
