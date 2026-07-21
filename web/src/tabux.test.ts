// Unit coverage for the pure decisions behind the tab UX work (#1813): what a tab
// is NAMED, whether its name may be edited, and where a drag within the tab bar
// drops it.
//
// These are the three places the feature can be wrong without anything throwing —
// a pane header that disagrees with the tab bar, a rename offered where the daemon
// refuses it, an off-by-one that moves a tab two slots — so they are pulled out as
// pure functions and pinned here, the way layout.ts's resolveDragTab is.

import { test } from "node:test";
import assert from "node:assert/strict";

import { cacheBustedWebSrc, nextReloadNonce } from "./tabaddr.js";
import { isRenameableTab, tabDisplayLabel, tabIcon, tabLabel } from "./tablabel.js";
import { insertionIndexAt, PINNED_TABS, reorderTargetIndex } from "./tabreorder.js";
import { TabKind } from "./types.js";

// --- the kind → icon map ----------------------------------------------------

test("tabIcon: each kind gets its semantic Lucide icon", () => {
  assert.equal(tabIcon(TabKind.Agent), "bot");
  assert.equal(tabIcon(TabKind.Shell), "terminal");
  assert.equal(tabIcon(TabKind.Process), "terminal");
  assert.equal(tabIcon(TabKind.Web), "panels");
});

test("tabIcon: a VS Code tab shares the web icon, not the process fallback (#1817)", () => {
  // The rule that has shell and process share an icon: the icon names what a tab IS, and
  // a VS Code tab is an embedded browser surface with no PTY. Pinned explicitly
  // because the failure mode is SILENT — VSCode landing on the default arm would
  // render a terminal icon and call the editor a terminal, which it is not.
  assert.equal(tabIcon(TabKind.VSCode), "panels");
  assert.equal(tabIcon(TabKind.VSCode), tabIcon(TabKind.Web));
  assert.notEqual(tabIcon(TabKind.VSCode), tabIcon(TabKind.Shell));
});

test("tabIcon: an unknown kind falls back to the process icon, as labelForTab does", () => {
  // ui/tree/labels.go's default branch names an unknown kind like a process; the
  // icon follows the same branch rather than rendering a blank.
  assert.equal(tabIcon(99), "terminal");
});

// --- the kind + name → label map (mirrors ui/tree/labels.go labelForTab) ----

test("tabLabel: the agent tab is always Agent, ignoring its name", () => {
  assert.equal(tabLabel({ name: "agent", kind: TabKind.Agent }), "Agent");
  // The name is ignored, which is exactly why the daemon refuses to rename it.
  assert.equal(tabLabel({ name: "renamed-somehow", kind: TabKind.Agent }), "Agent");
});

test("tabLabel: a shell tab is always Terminal, ignoring its name", () => {
  assert.equal(tabLabel({ name: "shell", kind: TabKind.Shell }), "Terminal");
  assert.equal(tabLabel({ name: "my-shell", kind: TabKind.Shell }), "Terminal");
});

test("tabLabel: a web tab shows its name, or Web when it has none", () => {
  assert.equal(tabLabel({ name: "preview", kind: TabKind.Web }), "preview");
  assert.equal(tabLabel({ name: "", kind: TabKind.Web }), "Web");
});

test("tabLabel: a process tab shows its name, or Tab when it has none", () => {
  assert.equal(tabLabel({ name: "logs", kind: TabKind.Process }), "logs");
  assert.equal(tabLabel({ name: "", kind: TabKind.Process }), "Tab");
});

test("tabLabel: a VS Code tab shows its name, or VS Code when it has none (#1817)", () => {
  // Mirrors ui/tree/labels.go textForTab's VSCode arm. This is also WHY a vscode tab
  // is renameable — it reads its Name — so the two assertions belong together.
  assert.equal(tabLabel({ name: "editor", kind: TabKind.VSCode }), "editor");
  assert.equal(tabLabel({ name: "", kind: TabKind.VSCode }), "VS Code");
});

test("a VS Code tab's text label stays independent of its decorative icon (#1817)", () => {
  assert.equal(tabDisplayLabel({ name: "", kind: TabKind.VSCode }), "VS Code");
  assert.equal(tabDisplayLabel({ name: "editor", kind: TabKind.VSCode }), "editor");
});

test("tabDisplayLabel: accessible titles contain text, never icon glyphs", () => {
  assert.equal(tabDisplayLabel({ name: "preview", kind: TabKind.Web }), "preview");
  assert.equal(tabDisplayLabel({ name: "agent", kind: TabKind.Agent }), "Agent");
  assert.equal(tabDisplayLabel({ name: "x", kind: TabKind.Shell }), "Terminal");
});

// --- pane label derivation --------------------------------------------------

test("pane label: a pane names its TAB, not its position (#1813)", () => {
  // The bug: every pane header read "Tab ${leaf.tab + 1}". This is the assertion
  // that the header is derived from the tab's own kind+name, so two panes showing
  // different tabs can never read the same thing.
  const tabs = [
    { name: "agent", kind: TabKind.Agent },
    { name: "preview", kind: TabKind.Web },
    { name: "logs", kind: TabKind.Process },
  ];
  assert.deepEqual(tabs.map(tabDisplayLabel), ["Agent", "preview", "logs"]);
});

test("pane label: a rename changes the label the pane renders", () => {
  // What the live-repaint path exists to deliver: same tab, same kind, new name.
  assert.equal(tabLabel({ name: "preview", kind: TabKind.Web }), "preview");
  assert.equal(tabLabel({ name: "storefront", kind: TabKind.Web }), "storefront");
});

// --- which tabs may be renamed ---------------------------------------------

test("isRenameableTab: exactly the kinds whose label reads the name", () => {
  assert.equal(isRenameableTab(TabKind.Web), true);
  assert.equal(isRenameableTab(TabKind.Process), true);
  // VS Code (#1817) renders `name || "VS Code"`, so the same rule admits it — no new
  // policy, just the existing one applied. Mirrors session.TabKindRenameable.
  assert.equal(isRenameableTab(TabKind.VSCode), true);
  // Agent/shell labels ignore the name (see tabLabel above), so renaming one would
  // change nothing visible — the daemon refuses, and the affordance is not offered.
  assert.equal(isRenameableTab(TabKind.Agent), false);
  assert.equal(isRenameableTab(TabKind.Shell), false);
});

test("isRenameableTab tracks tabLabel exactly — the two are one rule stated twice", () => {
  // The invariant behind the predicate, checked mechanically rather than trusted: a
  // kind is renameable IF AND ONLY IF changing its name changes what tabLabel renders.
  // If someone adds a kind to one and not the other, this fails — which is the drift
  // the Go side guards with the same pairing beside its own mapping.
  for (const kind of Object.values(TabKind)) {
    const readsName = tabLabel({ name: "zzz", kind }) !== tabLabel({ name: "qqq", kind });
    assert.equal(isRenameableTab(kind), readsName, `kind ${kind}: renameable must match "label reads name"`);
  }
});

// --- the insertion point a drop over the bar resolves to --------------------

// Four tabs, 100px wide, laid out from x=0: centres at 50, 150, 250, 350.
const CENTERS = [50, 150, 250, 350];

test("insertionIndexAt: a pointer past a tab's centre drops AFTER it", () => {
  assert.equal(insertionIndexAt(CENTERS, 160), 2); // just right of tab 1's centre
  assert.equal(insertionIndexAt(CENTERS, 260), 3); // just right of tab 2's centre
});

test("insertionIndexAt: a pointer before a tab's centre drops BEFORE it", () => {
  assert.equal(insertionIndexAt(CENTERS, 140), 1); // just left of tab 1's centre
  assert.equal(insertionIndexAt(CENTERS, 240), 2); // just left of tab 2's centre
});

test("insertionIndexAt: past the last tab drops at the end", () => {
  assert.equal(insertionIndexAt(CENTERS, 9999), CENTERS.length);
});

test("insertionIndexAt: nothing can drop in front of the pinned agent tab", () => {
  // The hard constraint: Go's Tabs[0] is load-bearing, so aiming anywhere left of
  // the agent tab resolves to the gap just AFTER it rather than to 0.
  assert.equal(insertionIndexAt(CENTERS, 0), PINNED_TABS);
  assert.equal(insertionIndexAt(CENTERS, -500), PINNED_TABS);
  assert.equal(insertionIndexAt(CENTERS, 49), PINNED_TABS);
});

test("insertionIndexAt: an empty bar still clamps rather than returning a negative", () => {
  assert.equal(insertionIndexAt([], 100), 0);
});

// --- the target index a reorder request carries -----------------------------

test("reorderTargetIndex: moving a tab RIGHT accounts for its own removal", () => {
  // The off-by-one this function exists for: tab 1 dropped in the gap before old
  // index 3 lands at 2 once it is lifted out, not at 3.
  assert.equal(reorderTargetIndex(1, 3), 2);
  assert.equal(reorderTargetIndex(1, 4), 3);
});

test("reorderTargetIndex: moving a tab LEFT uses the insertion point as-is", () => {
  // Gaps below the tab are unaffected by lifting it out.
  assert.equal(reorderTargetIndex(3, 1), 1);
  assert.equal(reorderTargetIndex(3, 2), 2);
});

test("reorderTargetIndex: a drop on either side of the tab's own slot is a no-op", () => {
  assert.equal(reorderTargetIndex(2, 2), null); // the gap before it
  assert.equal(reorderTargetIndex(2, 3), null); // the gap after it
});

test("reorderTargetIndex: the pinned agent tab can never be moved", () => {
  // It stays DRAGGABLE (dragging it onto a pane still splits), but no drop on the
  // bar can relocate it. The daemon refuses this too; the bar just never asks.
  assert.equal(reorderTargetIndex(0, 1), null);
  assert.equal(reorderTargetIndex(0, 3), null);
});

test("reorder: a full drag of tab 1 to the end resolves to the last index", () => {
  // End to end through both halves, the way the drop handler composes them.
  const from = 1;
  const to = reorderTargetIndex(from, insertionIndexAt(CENTERS, 9999));
  assert.equal(to, CENTERS.length - 1);
});

test("reorder: a full drag of the last tab to just after the agent tab resolves to 1", () => {
  const from = 3;
  const to = reorderTargetIndex(from, insertionIndexAt(CENTERS, 60)); // right of agent's centre
  assert.equal(to, 1);
});

test("reorder: a full drag of any tab onto the agent tab's left half still lands after it", () => {
  const from = 2;
  const to = reorderTargetIndex(from, insertionIndexAt(CENTERS, 10)); // far left
  assert.equal(to, PINNED_TABS); // 1 — never 0
});

// --- the cache-busted reload src (#1900) -----------------------------------

test("cacheBustedWebSrc: adds the param with ? when the src carries no query", () => {
  assert.equal(cacheBustedWebSrc("/v1/webtab/s1/t1/", 1), "/v1/webtab/s1/t1/?_afreload=1");
});

test("cacheBustedWebSrc: adds the param with & when the src already has a query", () => {
  // The common proxied shape: a network peer's src carries ?access_token=.
  assert.equal(
    cacheBustedWebSrc("/v1/webtab/s1/t1/?access_token=abc", 2),
    "/v1/webtab/s1/t1/?access_token=abc&_afreload=2",
  );
});

test("cacheBustedWebSrc: leaves the rest of the query byte-for-byte alone", () => {
  // The token is percent-encoded by webProxyPath. Round-tripping the URL through a
  // parser would re-encode it (space → + vs %20, etc.); a plain append cannot, which
  // is why this is string concatenation and not URL/URLSearchParams.
  const src = "/v1/webtab/s1/t1/app/x.html?access_token=a%2Bb%2Fc%3D";
  assert.equal(cacheBustedWebSrc(src, 3), `${src}&_afreload=3`);
});

test("cacheBustedWebSrc: repeated reloads REPLACE the param, never accumulate it", () => {
  // The contract is in the CALLER: it always passes the pristine src, so each attempt
  // rebuilds from a base that has no _afreload to stack onto. This pins that the
  // helper composes that way — a URL that grew a second _afreload per press would
  // still "work" while quietly unbounded, which is exactly the bug to prevent.
  const pristine = "/v1/webtab/s1/t1/?access_token=abc";
  const first = cacheBustedWebSrc(pristine, 1);
  const second = cacheBustedWebSrc(pristine, 2);
  assert.equal(second, "/v1/webtab/s1/t1/?access_token=abc&_afreload=2");
  assert.notEqual(first, second); // a distinct URL per attempt is the whole mechanism
  assert.equal(second.match(/_afreload=/g)?.length, 1);
});

// A LOCK on the nonce's own contract, not the repro. The bug it guards (a per-mount
// counter re-issuing a cached URL) lives in split.ts's MOUNT, which this cannot reach —
// a helper called directly skips the very gate that kept the bug live, so the repro is
// the selftest that presses ↻ across a real remount. These pin the two properties that
// selftest depends on and could not localize if they broke.

test("nextReloadNonce: never returns the same value twice", () => {
  // The one property a cache-buster has. Anything re-issued is a URL the HTTP cache may
  // still hold an entry for.
  const seen = new Set<number>();
  for (let i = 0; i < 1000; i++) {
    seen.add(nextReloadNonce());
  }
  assert.equal(seen.size, 1000);
});

test("nextReloadNonce: is seeded past a fresh counter, so a page reload cannot walk back over issued URLs", () => {
  // The module-scope fix alone would only move the collision up one scope: a page
  // reload resets every module, and a counter starting at 0 re-issues _afreload=1 to a
  // cache that outlived the page. Seeding from the clock is what makes the value not
  // reset when its scope does — so this asserts the seed is a real one, not 0.
  // Compared against a wall-clock lower bound rather than a literal: the seed IS the
  // clock, so pinning a constant would rot.
  assert.ok(nextReloadNonce() > 1_600_000_000_000, "the nonce must be clock-seeded, not a fresh counter");
});

test("nextReloadNonce: is monotonic, so the sequence is assertable rather than merely probable", () => {
  const a = nextReloadNonce();
  const b = nextReloadNonce();
  assert.ok(b > a);
});
