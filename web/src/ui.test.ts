// Unit coverage for tabBarSig (#1737 follow-up). The signature decides WHEN the tab
// bar is rebuilt: it must change when anything the bar DRAWS changes (tab set, active
// index, shown-in-a-pane set, manageability) and must NOT change on an unrelated
// session-status snapshot. That second property is the fix — a status-only rebuild
// would replaceChildren() the bar and destroy the button a user just grabbed to drag a
// freshly-created tab, aborting the native HTML5 drag mid-gesture.

import { test } from "node:test";
import assert from "node:assert/strict";

import { documentTitle, tabBarSig } from "./ui.js";
import type { AppState } from "./ui.js";
import { Liveness, type SessionData } from "./types.js";

function sess(over: Partial<SessionData> = {}): SessionData {
  return { id: "a", title: "s", branch: "b", ...over };
}

/** A minimal AppState carrying only the fields tabBarSig reads. */
function state(over: Partial<AppState> = {}): AppState {
  return {
    selectedId: "a",
    sessions: [sess({ tabs: [{ name: "agent", kind: 0 }] })],
    activeTab: 0,
    shownTabs: [0],
    ...over,
  } as AppState;
}

test("an unrelated status/title snapshot on the selected session keeps the SAME sig", () => {
  const base = state();
  // Same tabs, active, shown — only the liveness + title changed (a rail event). The
  // bar draws none of that, so it must NOT be rebuilt (no drag-breaking churn).
  const churned = state({
    sessions: [sess({ tabs: [{ name: "agent", kind: 0 }], liveness: Liveness.Running, title: "s (working)" })],
  });
  assert.equal(tabBarSig(base), tabBarSig(churned));
});

test("an unrelated OTHER session appearing/updating keeps the SAME sig", () => {
  const base = state();
  const withNeighbor = state({
    sessions: [
      sess({ tabs: [{ name: "agent", kind: 0 }] }),
      sess({ id: "z", title: "other", tabs: [{ name: "agent", kind: 0 }] }),
    ],
  });
  assert.equal(tabBarSig(base), tabBarSig(withNeighbor));
});

test("creating a tab on the selected session CHANGES the sig (a real rebuild)", () => {
  const one = state();
  const two = state({
    sessions: [sess({ tabs: [{ name: "agent", kind: 0 }, { name: "shell", kind: 1 }] })],
  });
  assert.notEqual(tabBarSig(one), tabBarSig(two));
});

test("moving the active tab or the shown-set CHANGES the sig", () => {
  const twoTabs = { tabs: [{ name: "agent", kind: 0 }, { name: "shell", kind: 1 }] };
  const base = state({ sessions: [sess(twoTabs)] });
  assert.notEqual(tabBarSig(base), tabBarSig(state({ sessions: [sess(twoTabs)], activeTab: 1 })));
  assert.notEqual(tabBarSig(base), tabBarSig(state({ sessions: [sess(twoTabs)], shownTabs: [0, 1] })));
});

test("the shown-set sig is order-independent (a set, not a list)", () => {
  const twoTabs = { tabs: [{ name: "agent", kind: 0 }, { name: "shell", kind: 1 }] };
  assert.equal(
    tabBarSig(state({ sessions: [sess(twoTabs)], shownTabs: [0, 1] })),
    tabBarSig(state({ sessions: [sess(twoTabs)], shownTabs: [1, 0] })),
  );
});

test("manageability (local vs remote) is part of the sig — the + / × affordances differ", () => {
  const tabs = { tabs: [{ name: "agent", kind: 0 }] };
  assert.notEqual(
    tabBarSig(state({ sessions: [sess({ ...tabs, backend_type: "local" })] })),
    tabBarSig(state({ sessions: [sess({ ...tabs, backend_type: "remote" })] })),
  );
});

test("no selection collapses to the empty sig", () => {
  assert.equal(tabBarSig(state({ selectedId: null })), "");
});

test("the signature is delimiter-safe: a tab name containing separators can't hide a change", () => {
  // A naive `${kind}:${name}` joined by "|" would collide these two DIFFERENT tab sets
  // into the same string ("1:a|1:b") — suppressing a required rebuild and leaving a
  // stale tab bar. A structured signature must tell them apart.
  const oneTab = state({ sessions: [sess({ tabs: [{ name: "a|1:b", kind: 1 }] })] });
  const twoTabs = state({ sessions: [sess({ tabs: [{ name: "a", kind: 1 }, { name: "b", kind: 1 }] })] });
  assert.notEqual(tabBarSig(oneTab), tabBarSig(twoTabs));
});

test("the signature is delimiter-safe: a name mimicking the field separators still changes the sig", () => {
  const plain = state({ sessions: [sess({ tabs: [{ name: "t", kind: 1 }] })] });
  // A name crafted to look like the trailing sig fields must not collide with any real
  // active/shown/manageability combination.
  const tricky = state({ sessions: [sess({ tabs: [{ name: 't"::0::[0]::true', kind: 1 }] })] });
  assert.notEqual(tabBarSig(plain), tabBarSig(tricky));
});

// Unit coverage for documentTitle (#1826 item 2). The browser tab was a static
// "Agent Factory" on every screen, so a pinned or backgrounded tab said nothing about
// what it held. The title names the selected session and its project, and degrades
// cleanly when there is no selection.

/** A session rooted in a repo, the shape documentTitle reads. */
function inRepo(title: string, root: string): SessionData {
  return { id: title, title, branch: "b", worktree: { repo_path: root } };
}

test("documentTitle: a selected session names itself and its project", () => {
  const s = state({
    selectedId: "api",
    sessions: [inRepo("api", "/home/u/code/agent-factory")],
    selectedProject: "/home/u/code/agent-factory",
  });
  assert.equal(documentTitle(s), "api — agent-factory · Agent Factory");
});

test("documentTitle: with no selection the scoped project still qualifies the tab", () => {
  const s = state({
    selectedId: null,
    sessions: [inRepo("api", "/home/u/code/agent-factory")],
    selectedProject: "/home/u/code/agent-factory",
  });
  assert.equal(documentTitle(s), "agent-factory · Agent Factory");
});

test("documentTitle: with neither a selection nor a project it is the bare app name", () => {
  const s = state({ selectedId: null, sessions: [], selectedProject: null });
  assert.equal(documentTitle(s), "Agent Factory");
});

// The title must name the project the session actually LIVES in. The two only differ
// transiently (a selection surviving a project switch), but naming the scope there
// would caption the session with a repo it isn't in.
test("documentTitle: the session's own repo wins over the scoped project", () => {
  const s = state({
    selectedId: "api",
    sessions: [inRepo("api", "/home/u/code/agent-factory")],
    selectedProject: "/home/u/code/other-repo",
  });
  assert.equal(documentTitle(s), "api — agent-factory · Agent Factory");
});
