// Pins the #5061 fix: an awaited tab mutation's post-await apply must rebind the
// focused pane WITHOUT moving layoutGeneration. That counter answers "did the user
// move the layout during my await?" — and a gesture's own landing write is not the
// user moving anything. Counting it made every awaited mutation veto the NEXT one:
// a create pinned while an earlier close was still in flight read the close's
// landing write as newer intent and refused, leaving the pane bound to the old tab
// ("Tab created · the layout changed meanwhile" — the flake's trace, #5061).
//
// Overlapping awaited gestures then order among themselves by issue sequence —
// rebindTargetAfterAwait's rebindSeq/newestAppliedSeq — so the complementary check
// is pinned too: an apply that lands AFTER a newer gesture's cannot clobber it.
//
// SplitView is staged as in split_tabdrop.test.ts: no sessionId means the commit's
// reconcile is a no-op, so the generation counter and the leaf's tab are the whole
// observation. The end-to-end ordering is pinned by the Playwright selftest
// (web-driver.spec.ts #5061), which parks each gesture's post-await step on the
// __afTabRebindHold seam and releases them in the losing order.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import { leaves, type LayoutNode, resetIds, singleLeaf } from "./layout.js";
import { rebindTargetAfterAwait } from "./sessions.js";
import type { SplitCallbacks, SplitView as SplitViewType } from "./split.js";

// split.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of which plain
// node can load (esbuild resolves them at bundle time). Stub them out, then import
// the module dynamically so the hook is registered first.
register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

/** The private state the apply paths read; the cast is confined here. */
type SplitViewInternals = {
  tree: LayoutNode | null;
  focusedId: string | null;
};

function noopCallbacks(): SplitCallbacks {
  return {
    onStatus: () => {},
    onFocusChange: () => {},
    onLayout: () => {},
  };
}

/** A view showing a single leaf bound to tab 0. */
function stage(): { view: SplitViewType; internals: SplitViewInternals } {
  resetIds();
  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  const internals = view as unknown as SplitViewInternals;
  internals.tree = singleLeaf(0);
  internals.focusedId = leaves(internals.tree)[0].id;
  return { view, internals };
}

test("#5061 an awaited apply still rebinds the focused pane — it is not skipped work", () => {
  const { view, internals } = stage();
  view.setFocusedTabAwaited(1);
  assert.equal(leaves(internals.tree!)[0].tab, 1, "the focused pane now shows the awaited tab");
});

test("#5061 the awaited apply does not move the layout generation", () => {
  const { view } = stage();
  const genBefore = view.layoutGeneration();
  view.setFocusedTabAwaited(1);
  assert.equal(
    view.layoutGeneration(),
    genBefore,
    "a gesture's own landing write is not newer intent — counting it vetoes the NEXT gesture's rebind",
  );
});

test("#5061 a user re-point of the focused pane still counts as intent", () => {
  const { view } = stage();
  const genBefore = view.layoutGeneration();
  view.setFocusedTab(1);
  assert.equal(view.layoutGeneration(), genBefore + 1);
});

test("#5061 the losing ordering, replayed: the earlier gesture's landing does not veto the newer", () => {
  const { view } = stage();
  // Exactly the CI flake's ordering: the close pinned first, the create pinned while
  // the close was in flight, then the close's apply landed inside the create's
  // window. The create's guard must still pass — the write it saw was the close's
  // own landing, not newer intent.
  const pinnedGen = view.layoutGeneration();
  view.setFocusedTabAwaited(0); // the earlier gesture's landing write
  const outcome = rebindTargetAfterAwait({
    pinnedGen,
    pinnedSelId: "sess-a",
    currentGen: view.layoutGeneration(),
    currentSelId: "sess-a",
    pinnedSessionAlive: true,
    targetIdx: 1,
    rebindSeq: 2,
    newestAppliedSeq: 1,
    newestAppliedSelId: "sess-a",
  });
  assert.deepEqual(outcome, { kind: "rebind", idx: 1 });
});

test("#5061 a stale completion cannot clobber a NEWER gesture that already applied", () => {
  // The other direction of the same ordering: the create's RPC beat the close's, so
  // the create applied first — the close's late apply must refuse rather than yank
  // the pane back to the older intent's target.
  const outcome = rebindTargetAfterAwait({
    pinnedGen: 5,
    pinnedSelId: "sess-a",
    currentGen: 5,
    currentSelId: "sess-a",
    pinnedSessionAlive: true,
    targetIdx: 0,
    rebindSeq: 1,
    newestAppliedSeq: 2,
    newestAppliedSelId: "sess-a",
  });
  assert.deepEqual(outcome, { kind: "refused", reason: "layout-moved" });
});
