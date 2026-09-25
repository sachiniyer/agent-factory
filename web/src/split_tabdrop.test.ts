// Pins applyTabDrop's "changed" answer on the no-op drops (#4434): the release
// still LANDS — the gesture is consumed — but a drop that resolves and mutates
// nothing must report false so AppShell leaves user-opened disclosures alone.
// The no-ops are three: a stale payload, the sole tab on its own pane's edge,
// and (#4434 review) a tab dropped on the center of the pane already showing
// it — replaceTab hands back the same tree, so committing it would report a
// change that never happened.
//
// SplitView is constructed directly and its private state staged, exactly as
// split_selfsplit.test.ts does: the real path needs a DOM, an xterm and a live
// WS, none of which npm test has — and a staged view with no sessionId makes
// commit()'s reconcile a no-op, so the boolean is the whole observation.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import { type DragPayload, type LayoutNode, leaves, resetIds, singleLeaf } from "./layout.js";
import type { SplitCallbacks, SplitView as SplitViewType } from "./split.js";

register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

/** The private state applyTabDrop reads and writes, plus the method itself. */
type SplitViewInternals = {
  tree: LayoutNode | null;
  focusedId: string | null;
  tabRealIds: string[];
  applyTabDrop: (pane: unknown, drag: DragPayload, x: number, y: number) => boolean;
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

/** A pane stand-in whose zero-size rect zones "center" for any point —
 *  zoneAt's degenerate-rect guard answers center before the bands run. */
function centerPane(leafId: string): unknown {
  return {
    leafId,
    container: {
      getBoundingClientRect: () => ({ width: 0, height: 0, left: 0, top: 0, right: 0, bottom: 0 }),
    },
  };
}

test("drop: a tab on the center of the pane already showing it is a no-op (#4434 review)", () => {
  const v = stage(["a", "b"]);
  const leaf = singleLeaf(0);
  v.tree = leaf;

  const changed = v.applyTabDrop(centerPane(leaf.id), { id: "a", index: 0, tabs: ["a", "b"] }, 0, 0);
  assert.equal(changed, false,
    "replaceTab returns the same tree — reporting a change would dismiss a disclosure the drop never touched");
  assert.equal(leaves(v.tree!)[0].tab, 0, "the tree is untouched");
});

test("drop: a different tab on a pane's center commits and reports changed", () => {
  const v = stage(["a", "b"]);
  const leaf = singleLeaf(0);
  v.tree = leaf;

  const changed = v.applyTabDrop(centerPane(leaf.id), { id: "b", index: 1, tabs: ["a", "b"] }, 0, 0);
  assert.equal(changed, true, "the pane now shows the dropped tab — a real mutation");
  assert.equal(leaves(v.tree!)[0].tab, 1);
});

test("drop: a payload that resolves to nothing is still rejected, not a no-op commit", () => {
  const v = stage(["a", "b"]);
  const leaf = singleLeaf(0);
  v.tree = leaf;

  // "closed" is not in tabRealIds — the tab was closed mid-drag.
  const changed = v.applyTabDrop(centerPane(leaf.id), { id: "closed", index: 0, tabs: ["a", "b"] }, 0, 0);
  assert.equal(changed, false);
  assert.equal(leaves(v.tree!)[0].tab, 0);
});
