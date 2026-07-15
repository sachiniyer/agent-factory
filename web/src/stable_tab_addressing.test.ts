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
import { type SessionData, TabKind } from "./types.js";
import { leaves, remapByIdentity, resolveDragTab, tabsRebound } from "./layout.js";
import { iframeIdentity, iframeIsProxied, paneAddressUsesOrdinal, webProxyPath } from "./tabaddr.js";

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

// --- webProxyPath: id-keyed, and MIRRORS the target's path (#1810, #1811) ----

test("the proxy src is keyed by the tab's STABLE id, never its ordinal", () => {
  // The client half of #1810: an ordinal in this URL is what let a closed lower tab
  // silently repoint an open pane at a different dev server.
  const src = webProxyPath("sess-1", "id-web-b", "http://localhost:3000/", null);
  assert.equal(src, "/v1/webtab/sess-1/id-web-b/");
});

test("a root target's proxy path is the bare prefix, with its trailing slash", () => {
  // The trailing slash is load-bearing: the route requires it, and it keeps the
  // app's relative URLs resolving UNDER the prefix rather than beside it.
  assert.equal(webProxyPath("s", "t", "http://localhost:3000", null), "/v1/webtab/s/t/");
  assert.equal(webProxyPath("s", "t", "http://127.0.0.1:8080/", null), "/v1/webtab/s/t/");
});

test("the target's own path is MIRRORED into the proxy URL", () => {
  // The mirror model: the browser-visible depth matches the dev server's, so the
  // browser resolves the app's relative URLs exactly where the app expects them.
  assert.equal(
    webProxyPath("s", "t", "http://localhost:3000/app/viewer.html", null),
    "/v1/webtab/s/t/app/viewer.html",
  );
  assert.equal(
    webProxyPath("s", "t", "http://localhost:8899/viewer.html", null),
    "/v1/webtab/s/t/viewer.html",
  );
  // A directory-style target keeps its trailing slash.
  assert.equal(webProxyPath("s", "t", "http://localhost:3000/app/", null), "/v1/webtab/s/t/app/");
});

test("the mirrored depth is what makes ../ resolve INSIDE the prefix", () => {
  // The property the whole model exists for, asserted the way a browser computes it:
  // a parent-relative link on the proxied page must land back inside the tab prefix
  // (and mirror upstream /shared.css), never escape to the origin root.
  const src = webProxyPath("s", "t", "http://localhost:3000/app/viewer.html", null);
  const resolved = new URL("../shared.css", `http://daemon${src}`);
  assert.equal(resolved.pathname, "/v1/webtab/s/t/shared.css");
  // A sibling link stays beside the document, mirroring upstream /app/x.css.
  assert.equal(new URL("x.css", `http://daemon${src}`).pathname, "/v1/webtab/s/t/app/x.css");
});

test("the token rides ?access_token= (an iframe src cannot set a header)", () => {
  assert.equal(
    webProxyPath("s", "t", "http://localhost:3000/app/viewer.html", "tok en"),
    "/v1/webtab/s/t/app/viewer.html?access_token=tok%20en",
  );
  // A tokenless (loopback/exempt) client sends none.
  assert.equal(webProxyPath("s", "t", "http://localhost:3000/", null), "/v1/webtab/s/t/");
});

test("session and tab ids are escaped into their path segments", () => {
  const src = webProxyPath("a/b", "c d", "http://localhost:3000/", null);
  assert.equal(src, "/v1/webtab/a%2Fb/c%20d/");
});

test("an unparseable target contributes no path", () => {
  assert.equal(webProxyPath("s", "t", "not a url", null), "/v1/webtab/s/t/");
});

// --- paneAddressUsesOrdinal: rebuild iff the pane's ADDRESS would change -----

test("an id-addressed terminal's ordinal is inert — a move needs no rebuild", () => {
  // terminal.ts sends ?tab_id= OR ?tab=, never both, so a real id makes the captured
  // ordinal dead weight: the daemon resolves the id to wherever the tab now sits.
  assert.equal(paneAddressUsesOrdinal(null, "id-shell"), false);
});

test("a LEGACY terminal's ordinal IS its address — a move must rebuild", () => {
  assert.equal(paneAddressUsesOrdinal(null, ""), true);
});

test("a PROXIED web tab is id-keyed since #1810 — a move is followed, not remounted", () => {
  // This INVERTS the pre-#1810 rule, because the reason for it is gone. A loopback
  // preview used to be fetched through /v1/webtab/{session}/{ORDINAL}/, so a moved
  // tab had to be torn down or the iframe would proxy whatever took its old index.
  // The route is keyed by the stable tab id now, so the src survives a shift and the
  // frame — with the dev server's in-page state — is kept.
  assert.equal(paneAddressUsesOrdinal("http://localhost:3000", "id-web"), false);
  assert.equal(paneAddressUsesOrdinal("http://127.0.0.1:8080/app", "id-web"), false);
  assert.equal(paneAddressUsesOrdinal("http://[::1]:5173", "id-web"), false);
});

test("an EXTERNAL web tab's src encodes no ordinal — a move is followed, not remounted", () => {
  // This is the case the P3 is really about: the iframe src is the target URL itself,
  // so a shifted ordinal cannot invalidate it and reloading would drop in-page state
  // for nothing.
  assert.equal(paneAddressUsesOrdinal("https://example.com/docs", "id-web"), false);
  assert.equal(paneAddressUsesOrdinal("https://example.com/docs", ""), false);
});

test("no web target shape embeds an ordinal — none of them force a remount", () => {
  // Since #1810 this holds for EVERY web pane, whatever the target: a targetless tab
  // (which renders a fallback, not a frame) and an unparseable one (never proxied)
  // included. Pinned so a future change that reintroduces an ordinal into any web
  // address has to come through this assertion.
  assert.equal(paneAddressUsesOrdinal("", "id-web"), false);
  assert.equal(paneAddressUsesOrdinal("not a url", "id-web"), false);
  assert.equal(paneAddressUsesOrdinal("", ""), false);
});

// --- the end-to-end property: a shifted tab is still addressed by ITS id -----

test("after a cross-client close+create, the pane's stream id is still the tab it shows", () => {
  // The user-visible contract behind remapByIdentity, asserted on the value that
  // actually reaches the wire: pane shows B; another client closes lower tab A and
  // creates C; the pane must end up addressing B's stable id — NOT C's, which is what
  // the pre-fix code bound because it resolved by the leaf's stale ordinal.
  const prevIds = ["id-agent", "id-a", "id-b"];
  const ids = ["id-agent", "id-b", "id-c"];
  const realIdsNow = ["id-agent", "id-b", "id-c"];

  const settled = remapByIdentity({ kind: "leaf", id: "L1", tab: 2 }, prevIds, ids);
  const leaf = leaves(settled)[0];

  // What reconcile would hand AttachTerminal as ?tab_id= for this pane:
  assert.equal(realIdsNow[leaf.tab], "id-b", "input must be routed to tab B — the tab the pane shows");
  assert.notEqual(realIdsNow[leaf.tab], "id-c", "binding C's id here is the misroute this PR ends");

  // And because the identity at the settled ordinal still matches what the pane was
  // built against, the terminal is FOLLOWED rather than torn down and re-dialled.
  assert.equal(ids[leaf.tab], "id-b");
  assert.equal(paneAddressUsesOrdinal(null, realIdsNow[leaf.tab]), false, "an id-addressed pane need not rebuild to follow");
});

// --- the vscode pane's addressing (feat: VS Code tabs) ----------------------

test("a VSCODE pane is always proxied — it has no target to classify", () => {
  // The empty-target test that correctly answers false for a URL-less WEB tab would
  // answer the wrong question here: a vscode tab carries no target BY DESIGN (its
  // code-server is a daemon-managed per-session process on an ephemeral port), and
  // the proxy path is the only address that exists for it.
  assert.equal(iframeIsProxied({ kind: TabKind.VSCode, target: "" }), true);
  // A web tab still classifies by its target.
  assert.equal(iframeIsProxied({ kind: TabKind.Web, target: "http://localhost:3000" }), true);
  assert.equal(iframeIsProxied({ kind: TabKind.Web, target: "https://example.com" }), false);
  assert.equal(iframeIsProxied({ kind: TabKind.Web, target: "" }), false);
});

test("a VSCODE pane's identity is constant, so a reconcile never reloads the editor", () => {
  // The identity feeds the rebuild guard, and a rebuild reloads the iframe — which
  // for VS Code means dropping unsaved buffers. A vscode tab has no target to vary,
  // so its identity must not vary either.
  const a = iframeIdentity({ kind: TabKind.VSCode, target: "" });
  const b = iframeIdentity({ kind: TabKind.VSCode, target: "" });
  assert.equal(a, b);
  // ...and it can never collide with a real URL identity.
  assert.notEqual(a, iframeIdentity({ kind: TabKind.Web, target: "" }));
});

test("a VSCODE pane addresses by tab id, not ordinal — a move never repoints it", () => {
  // Since #1810 the proxy is id-keyed, so no iframe pane rebuilds on a move. For a
  // vscode pane that is what stops a moved tab from framing ANOTHER session's editor.
  assert.equal(paneAddressUsesOrdinal("", "id-vscode"), false);
});

test("a VSCODE pane's proxy path is the bare tab prefix — there is no target to mirror", () => {
  // The mirror-path model (#1808) splices the TARGET's path into the URL. A vscode
  // tab has none, so its src is just the tab prefix — and the trailing slash still
  // matters: code-server derives its relative asset URLs from the path depth.
  assert.equal(webProxyPath("sess-1", "id-vscode", "", null), "/v1/webtab/sess-1/id-vscode/");
  assert.equal(
    webProxyPath("sess-1", "id-vscode", "", "tok"),
    "/v1/webtab/sess-1/id-vscode/?access_token=tok",
  );
});
