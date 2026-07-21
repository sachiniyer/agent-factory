// Unit coverage for tabBarSig (#1737 follow-up). The signature decides WHEN the tab
// bar is rebuilt: it must change when anything the bar DRAWS changes (tab set, active
// index, shown-in-a-pane set, manageability) and must NOT change on an unrelated
// session-status snapshot. That second property is the fix — a status-only rebuild
// would replaceChildren() the bar and destroy the button a user just grabbed to drag a
// freshly-created tab, aborting the native HTML5 drag mid-gesture.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  canManageTabs,
  documentTitle,
  isActionableSession,
  isKillableSession,
  supportsTabManagement,
  tabBarSig,
  tabCreationUnavailableReason,
} from "./ui.js";
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

test("rail actionability is granted only by the daemon projection (#2234)", () => {
  assert.equal(
    isActionableSession(sess({ lifecycle_action: "archive" })),
    true,
    "a stable row with the projected verb is actionable",
  );
  assert.equal(
    isActionableSession(sess()),
    false,
    "the browser must not infer an action from a settled-looking row",
  );
  assert.equal(
    isActionableSession(sess({ id: undefined, lifecycle_action: "archive" })),
    false,
    "a malformed id-less capability fails closed",
  );
});

test("kill addressability is independent and fails closed", () => {
  assert.equal(
    isKillableSession(sess({ can_kill: true })),
    true,
    "a stable row with the projected teardown capability is killable",
  );
  assert.equal(
    isKillableSession(sess({ lifecycle_action: "archive" })),
    false,
    "the lifecycle verb must not implicitly grant teardown",
  );
  assert.equal(
    isKillableSession(sess({ id: undefined, can_kill: true })),
    false,
    "an id-less teardown capability fails closed",
  );
});

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

test("canManageTabs: an archived session is inert — its + / × are withdrawn (#1809)", () => {
  // Archive PRESERVES web tabs so a restore can render them again, which made an
  // archived session the first one to carry a closable (non-agent) tab. The daemon
  // refuses CreateTab/CloseTab on one, and the × specifically would strip the very
  // URL the archive kept — so the affordances must not be offered at all.
  assert.equal(canManageTabs(sess({ backend_type: "local" })), true, "a live local session manages tabs");
  assert.equal(
    canManageTabs(sess({ backend_type: "local", liveness: Liveness.Archived })),
    false,
    "an archived session is inert",
  );
  assert.equal(canManageTabs(sess({ backend_type: "remote" })), false, "remote tabs stay config-fixed");
});

test("supportsTabManagement: every off-box runtime is withdrawn, not just the hook one (#1874)", () => {
  // This predicate used to read `backend_type !== "remote"`, which named the hook
  // runtime only — so docker/ssh sessions were offered a + and an "Open in VS
  // Code" item that could not work: every Add*Tab path needs a daemon-side git
  // worktree an off-box workspace does not have. The daemon rejects the call, so
  // the affordance could only ever produce an error toast.
  for (const backend_type of ["docker", "ssh", "remote"]) {
    assert.equal(
      supportsTabManagement(sess({ backend_type })),
      false,
      `${backend_type} runs off-box and cannot spawn a tab`,
    );
  }
});

test("supportsTabManagement: local and legacy records keep tab management", () => {
  assert.equal(supportsTabManagement(sess({ backend_type: "local" })), true, "a local session manages tabs");
  // backend_type is omitempty, so a pre-#1592 record carries none. It is a local
  // session; defaulting it to off-box would strip the + from every legacy row.
  assert.equal(supportsTabManagement(sess({})), true, "a record with no backend_type is local");
});

test("tab creation always explains archived, off-box, and full states (#2077)", () => {
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "local" }), 1), null);
  assert.equal(
    tabCreationUnavailableReason(sess({ backend_type: "local", liveness: Liveness.Archived }), 1),
    "Restore this session to create tabs",
  );
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "docker" }), 1), "Docker sessions have a fixed tab list");
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "ssh" }), 1), "SSH sessions have a fixed tab list");
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "remote" }), 1), "Remote sessions have a fixed tab list");
  assert.equal(
    tabCreationUnavailableReason(sess({ backend_type: "remote", liveness: Liveness.Archived }), 1),
    "Archived · Remote sessions have a fixed tab list",
    "restoring an off-box session must not falsely promise that tab creation will become available",
  );
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "local" }), 9), "Nine-tab limit reached");
});

test("archiving the selected session changes the sig — the bar must rebuild to drop the × (#1809)", () => {
  // The sig gates the rebuild, so if archiving didn't change it the bar would keep
  // rendering a live × over an archived session's preserved web tab.
  const tabs = {
    tabs: [
      { name: "agent", kind: 0 },
      { name: "webpreview", kind: 3, url: "http://localhost:3000" },
    ],
  };
  assert.notEqual(
    tabBarSig(state({ sessions: [sess({ ...tabs, backend_type: "local" })] })),
    tabBarSig(state({ sessions: [sess({ ...tabs, backend_type: "local", liveness: Liveness.Archived })] })),
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
