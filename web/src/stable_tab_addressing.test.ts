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

import { tabBarSig, tabIdentity, tabRealId } from "./ui.js";
import type { AppState } from "./ui.js";
import type { SessionData } from "./types.js";
import { leaves, remapByIdentity, resolveDragTab, tabsRebound } from "./layout.js";

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

// --- the drag-id cache must not hide behind the tab-bar render gate ----------

test("an id backfill does NOT change tabBarSig — so the drag-id cache cannot be refreshed by the bar render", () => {
  // The constraint that forces syncTabIdentityCaches to run outside the render gate
  // (#1779). tabBarSig covers only what the bar DRAWS (kind/name/active/shown), so a
  // snapshot that backfills a just-created tab's stable id is signature-identical and
  // the bar is NOT rebuilt — correctly, since rebuilding would destroy a button held
  // mid-drag (#1737). A drag-id cache refreshed only by renderTabBar would therefore
  // stay stale on exactly the tab that just earned an id.
  //
  // If this assertion ever flips, the caches could move back into renderTabBar — but
  // the #1737 drag regression would be back too, so it should not.
  const sess = (tabs: SessionData["tabs"]): SessionData => ({ id: "a", title: "s", branch: "b", tabs });
  const st = (tabs: SessionData["tabs"]): AppState =>
    ({ selectedId: "a", sessions: [sess(tabs)], activeTab: 0, shownTabs: [0] }) as AppState;

  const beforeAdopt = st([
    { name: "agent", kind: 0, id: "id-agent" },
    { name: "shell", kind: 1 }, // just created; no id yet
  ]);
  const afterAdopt = st([
    { name: "agent", kind: 0, id: "id-agent" },
    { name: "shell", kind: 1, id: "id-shell" }, // the snapshot backfilled it
  ]);

  assert.equal(tabBarSig(beforeAdopt), tabBarSig(afterAdopt), "an id backfill draws no pixel, so the sig must not change");

  // ...and the real id the drag needs DID change across those two snapshots, which is
  // exactly what the sig cannot see.
  assert.equal(tabRealId({ name: "shell", kind: 1 } as { id?: string }), "");
  assert.equal(tabRealId({ id: "id-shell" }), "id-shell");
});

// --- remapByIdentity: a pane follows its OWN tab across a shift --------------

test("a pane follows its tab when a lower tab is closed and replaced (the codex #1805 repro)", () => {
  // A leaf holds an ORDINAL but the user put a TAB in that pane. Pane shows B at
  // index 2; another client closes A and creates C, so the list goes
  // [agent,A,B] → [agent,B,C] and B is now at index 1. Reconciling from the leaf's
  // stale 2 would rebind the pane to C. The leaf must move to 1 instead.
  const prevIds = ["id-agent", "id-a", "id-b"];
  const ids = ["id-agent", "id-b", "id-c"];
  const tree = { kind: "leaf", id: "L1", tab: 2 } as const;

  const out = remapByIdentity(tree, prevIds, ids);
  assert.equal(leaves(out).length, 1);
  assert.equal(leaves(out)[0].tab, 1, "the pane must follow tab B to its new ordinal, not stay on 2 (now tab C)");
});

test("a no-op resync preserves the tree REFERENCE — no churn, no rebuild", () => {
  // Reference stability is load-bearing: setSession re-validates on every store
  // update, and a fresh tree each time would reconcile (and rebuild terminals) on
  // every status tick.
  const ids = ["id-agent", "id-b"];
  const tree = { kind: "leaf", id: "L1", tab: 1 } as const;
  assert.equal(remapByIdentity(tree, ids, ids), tree);
});

test("a leaf whose tab was really closed keeps its ordinal and rebuilds against what is there", () => {
  // No survivor claims index 1, so the pane degrades to showing the tab that took the
  // slot — the same behavior as a plain tab-count shrink.
  const prevIds = ["id-agent", "id-a"];
  const ids = ["id-agent", "id-c"];
  const tree = { kind: "leaf", id: "L1", tab: 1 } as const;
  assert.equal(leaves(remapByIdentity(tree, prevIds, ids))[0].tab, 1);
});

test("when a dead leaf collides with a survivor's claim, the SURVIVOR keeps the tab", () => {
  // Two panes: L1 shows A (index 1), L2 shows B (index 2). A is closed, C created:
  // [agent,A,B] → [agent,B,C]. B survives and moves 2→1, which is where L1 (dead)
  // sits. L2 actually holds B, so it must win; L1's tab is gone, so it closes.
  // Note validate() would resolve this collision the OTHER way — it keeps the FIRST
  // leaf — which is why remapByIdentity settles it first.
  const prevIds = ["id-agent", "id-a", "id-b"];
  const ids = ["id-agent", "id-b", "id-c"];
  const tree = {
    kind: "split",
    id: "S1",
    dir: "row",
    ratio: 0.5,
    a: { kind: "leaf", id: "L1", tab: 1 },
    b: { kind: "leaf", id: "L2", tab: 2 },
  } as const;

  const out = remapByIdentity(tree, prevIds, ids);
  const ls = leaves(out);
  assert.equal(ls.length, 1, "the pane whose tab was closed must go, not the one that still holds B");
  assert.equal(ls[0].id, "L2", "L2 is the pane that actually holds tab B");
  assert.equal(ls[0].tab, 1, "and it follows B to its new ordinal");
});

test("remapByIdentity is a no-op before anything is bound", () => {
  const tree = { kind: "leaf", id: "L1", tab: 0 } as const;
  assert.equal(remapByIdentity(tree, [], ["id-agent"]), tree);
});

test("a legacy leaf with a synthesized identity still follows a shift", () => {
  // Identity is all a legacy tab has; `1:shell` moving 2→1 is followed on that basis.
  // This inherits the known kind:name collision hole, exactly like the DnD legacy
  // branch — it is not made worse here.
  const prevIds = ["0:agent", "1:other", "1:shell"];
  const ids = ["0:agent", "1:shell"];
  const tree = { kind: "leaf", id: "L1", tab: 2 } as const;
  assert.equal(leaves(remapByIdentity(tree, prevIds, ids))[0].tab, 1);
});
