// Unit coverage for the web half of the stable-tab-id addressing contract
// (#1738, completed in #1779).
//
// #1738 introduced the stable id but left the web client leaking two ORDINAL /
// FALLBACK identities back into paths that must key on a real id:
//
//  1. tabIdentity synthesizes a `kind:name` for an id-less tab. That synthesized
//     value is fine as a LOCAL bookkeeping key, but it is not a daemon id — sending
//     it as ?tab_id= is an unknown id the daemon 404s, and trusting it as
//     collision-proof re-opens the close+recreate hole the guard exists to catch.
//  2. Reconcile only ran when the layout TREE changed, so a tab identity swapping
//     at an unchanged index left the pane attached to a dead tab.
//
// These pin the three pure decisions that close both.

import { test } from "node:test";
import assert from "node:assert/strict";

import { tabIdentity, tabRealId } from "./ui.js";
import { resolveDragTab, tabsRebound } from "./layout.js";

// --- tabRealId: the real id, never a synthesized one -----------------------

test("tabRealId is the daemon id when the tab has one", () => {
  assert.equal(tabRealId({ id: "deadbeefcafef00d" }), "deadbeefcafef00d");
});

test("tabRealId is EMPTY for an id-less tab, where tabIdentity synthesizes", () => {
  // The whole point of the split: for the same tab, the identity is a usable local
  // key while the real id is honestly absent. Passing the synthesized value as a
  // ?tab_id= would 404 a legacy tab that attaches fine by ordinal (#1779).
  const legacy: { id?: string; name: string; kind: number } = { name: "shell", kind: 1 };
  assert.equal(tabIdentity(legacy), "1:shell");
  assert.equal(tabRealId(legacy), "");
  assert.equal(tabRealId({ id: "" }), "");
});

// --- resolveDragTab: only a REAL id may skip the legacy guard ---------------

test("a real stable id resolves to the tab's CURRENT ordinal after a mid-drag shift", () => {
  // The misroute fix: the tab dragged from index 2 now sits at index 1 because a
  // lower tab closed mid-drag. It binds where the SAME tab is now, not index 2.
  const drag = { id: "id-b", index: 2, tabs: ["id-agent", "id-a", "id-b"] };
  const realIds = ["id-agent", "id-b"];
  assert.equal(resolveDragTab(drag, realIds, realIds, realIds.length), 1);
});

test("a real stable id whose tab was closed mid-drag cancels the drop", () => {
  const drag = { id: "id-a", index: 1, tabs: ["id-agent", "id-a"] };
  const realIds = ["id-agent"];
  assert.equal(resolveDragTab(drag, realIds, realIds, realIds.length), null);
});

test("a FALLBACK identity is never treated as a drag id — the legacy guard still runs", () => {
  // The #1779 hole: an id-less "shell" tab is closed and a NEW "shell" created
  // mid-drag. Both share the `1:shell` identity, so an id-keyed lookup would happily
  // resolve the drag onto the REPLACEMENT tab. dragstart stamps "" for such a tab,
  // which routes it to the guarded branch, where the changed tab-set snapshot
  // cancels the drop.
  const drag = { id: "", index: 1, tabs: ["0:agent", "1:shell"] };
  const identitiesNow = ["0:agent", "1:shell"]; // same STRINGS, but a different tab
  const realIdsNow = ["", ""]; // still no daemon ids
  // The tab set "matches" by identity, so the legacy branch permits the drop — that
  // is the pre-existing legacy behavior and is not what this test pins. What it pins
  // is that an id-less tab takes the GUARDED branch at all:
  assert.equal(resolveDragTab(drag, realIdsNow, identitiesNow, 2), 1);

  // ...and when the set HAS visibly changed, the guard cancels — proving the drop
  // was never id-resolved. Had `1:shell` been accepted as a stable id, indexOf would
  // have found it at index 1 and bound the replacement regardless of this guard.
  const changed = { id: "", index: 1, tabs: ["0:agent", "1:shell", "1:extra"] };
  assert.equal(resolveDragTab(changed, realIdsNow, identitiesNow, 2), null);
});

test("a synthesized identity in drag.id resolves against REAL ids and cancels, never the identity list", () => {
  // The precise #1779 hole, and the reason the lookup list matters. A legacy tab has
  // identity `1:shell` but NO daemon id. If that synthesized identity reaches drag.id
  // — which is exactly what stamping the identity list at dragstart did — then
  // resolving it against the IDENTITY list finds it at index 1 and binds the pane,
  // skipping the sameTabs guard that is a legacy tab's only protection against a
  // mid-drag close+recreate. Resolving against the REAL ids finds nothing (they hold
  // "" for such a tab) and cancels.
  //
  // The two lists MUST differ here or the assertion proves nothing.
  const identities = ["0:agent", "1:shell"];
  const realIds = ["", ""];
  const drag = { id: "1:shell", index: 1, tabs: ["0:agent", "1:shell"] };
  assert.equal(resolveDragTab(drag, realIds, identities, 2), null);
});

test("a legacy drag with an out-of-range index cancels", () => {
  const drag = { id: "", index: 5, tabs: ["0:agent"] };
  assert.equal(resolveDragTab(drag, [""], ["0:agent"], 1), null);
});

// --- tabsRebound: reconcile on IDENTITY change, not just count -------------

test("tabsRebound is false for an identical snapshot (an unrelated resync is a no-op)", () => {
  assert.equal(tabsRebound(["a", "b"], [0, 1], ["", ""], ["a", "b"], [0, 1], ["", ""]), false);
});

test("tabsRebound is TRUE when a tab identity swaps at an unchanged index", () => {
  // The #1779 stale-pane bug: another client closed tab "b" and created "c" before
  // this browser fetched. The COUNT is unchanged, so the layout tree validates
  // identically and a tree-only check would find nothing to do — leaving the pane
  // attached to the dead tab_id "b".
  assert.equal(tabsRebound(["a", "b"], [0, 1], ["", ""], ["a", "c"], [0, 1], ["", ""]), true);
});

test("tabsRebound is TRUE when a tab's kind or web target changes at the same index", () => {
  // Same story for an iframe pane: the identity can hold while the target moves.
  assert.equal(tabsRebound(["a", "b"], [0, 1], ["", ""], ["a", "b"], [0, 3], ["", ""]), true);
  assert.equal(
    tabsRebound(["a", "b"], [0, 3], ["", "http://localhost:3000"], ["a", "b"], [0, 3], ["", "http://localhost:4000"]),
    true,
  );
});

test("tabsRebound treats an undefined target and an empty one as the same", () => {
  // A terminal tab carries no target; undefined vs "" must not churn a rebuild.
  assert.equal(tabsRebound(["a"], [0], [undefined], ["a"], [0], [""]), false);
});

test("tabsRebound is TRUE when the tab count changes", () => {
  assert.equal(tabsRebound(["a"], [0], [""], ["a", "b"], [0, 1], ["", ""]), true);
});
