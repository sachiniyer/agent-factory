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
  usesCondensedSessionChrome,
  canCreateTabKind,
  canCloseTabs,
  canMutateTabRoster,
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
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "local" })), null);
  assert.equal(
    tabCreationUnavailableReason(sess({ backend_type: "local", liveness: Liveness.Archived })),
    "Restore this session to create tabs",
  );
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "docker" })), "Docker sessions have a fixed tab list");
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "ssh" })), "SSH sessions have a fixed tab list");
  assert.equal(tabCreationUnavailableReason(sess({ backend_type: "remote" })), "Remote sessions have a fixed tab list");
  assert.equal(
    tabCreationUnavailableReason(sess({ backend_type: "remote", liveness: Liveness.Archived })),
    "Archived · Remote sessions have a fixed tab list",
    "restoring an off-box session must not falsely promise that tab creation will become available",
  );
  assert.equal(
    tabCreationUnavailableReason(sess({ backend_type: "local" })),
    null,
    "a local session offers tab creation at any count — the nine-tab cap is gone (#3023)",
  );
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

test("mobile session chrome condenses only for a real selection on the sessions surface (#2354)", () => {
  const selected = state({ view: "sessions" });
  assert.equal(usesCondensedSessionChrome(selected), true, "a selected session gives its row to hamburger + tabs");
  assert.equal(
    usesCondensedSessionChrome(state({ view: "tasks" })),
    false,
    "another top-level view keeps app navigation in flow",
  );
  assert.equal(
    usesCondensedSessionChrome(state({ view: "sessions", selectedId: null })),
    false,
    "no selection keeps navigation",
  );
  assert.equal(
    usesCondensedSessionChrome(state({ view: "sessions", selectedId: "gone" })),
    false,
    "a stale id must not hide navigation over the empty pane",
  );
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

test("tab kinds come from the daemon's projection, not from backend_type (#3060)", () => {
  // The whole point: the client reads the daemon's per-kind verdict. An off-box
  // session whose daemon ALLOWS a web tab must be offered one, even though the old
  // backend_type rule would have refused every kind.
  const offBoxButWebAllowed = sess({
    backend_type: "ssh",
    tab_kinds: [
      { kind: "shell", allowed: false, reason: "needs a local worktree" },
      { kind: "web", allowed: true },
    ],
  });
  assert.equal(canCreateTabKind(offBoxButWebAllowed, "web"), true, "the daemon said yes; the UI must offer it");
  assert.equal(canCreateTabKind(offBoxButWebAllowed, "shell"), false);
  // supportsTabManagement is scoped to what the web can OFFER: the menu has no
  // entry for `web`, so allowing only that kind must not light up the control or
  // the `t` shortcut — both would lead to a create the per-kind check rejects.
  assert.equal(
    supportsTabManagement(offBoxButWebAllowed),
    false,
    "the daemon allows a kind this UI cannot offer, so the new-tab control stays withdrawn",
  );

  // And a LOCAL session whose daemon refuses everything is refused, which the old
  // rule could not express at all.
  const localButRefused = sess({
    backend_type: "local",
    tab_kinds: [
      { kind: "shell", allowed: false, reason: "some future reason" },
      { kind: "web", allowed: false, reason: "some future reason" },
    ],
  });
  assert.equal(supportsTabManagement(localButRefused), false, "backend_type says local; the daemon says no");
});

test("a pre-#3060 daemon falls back to the backend_type rule it actually enforces", () => {
  // No tab_kinds at all. Falling back is the SAFE direction: that daemon enforces
  // exactly the old rule, so the affordances still match its answer.
  assert.equal(supportsTabManagement(sess({ backend_type: "local" })), true);
  assert.equal(supportsTabManagement(sess({ backend_type: "ssh" })), false);
  assert.equal(canCreateTabKind(sess({ backend_type: "docker" }), "web"), false);
  // A record with no backend_type is a pre-#1592 local session.
  assert.equal(supportsTabManagement(sess({})), true, "a legacy row must not lose tab management");
});

test("closing follows the roster verdict, which is what the daemon actually enforces (#3060)", () => {
  // An off-box session that can create nothing can still close a tab it already
  // has — the daemon's CloseTab refuses only the agent tab.
  const offBox = sess({ backend_type: "ssh" });
  assert.equal(supportsTabManagement(offBox), false, "creates nothing");
  assert.equal(
    canCloseTabs(offBox),
    false,
    "and cannot close either: CloseTab opens with tabMutationTarget, which refuses TabManagement=false",
  );
  // Archived is the one case the daemon genuinely refuses.
  assert.equal(canCloseTabs(sess({ backend_type: "local", liveness: Liveness.Archived })), false);
});

test("each affordance asks its OWN daemon verdict (#3060)", () => {
  // The shared cause of the review round: one bool stood in for four different
  // daemon rules — create (per kind), close (no backend gate), and roster mutation
  // (Capabilities.TabManagement). A session can legitimately answer these
  // differently, and every combination below is one the daemon can produce.
  const createsNothingButHasTabs = sess({
    backend_type: "ssh",
    tab_kinds: [
      { kind: "shell", allowed: false, reason: "needs a local worktree" },
      { kind: "web", allowed: false, reason: "not restored on the sandbox path" },
    ],
    tab_roster_mutable: false,
  });
  assert.equal(supportsTabManagement(createsNothingButHasTabs), false);
  assert.equal(
    canCloseTabs(createsNothingButHasTabs),
    false,
    "closing is the ROSTER verdict: CloseTab delegates to tabMutationTarget, which this session fails",
  );
  assert.equal(canMutateTabRoster(createsNothingButHasTabs), false, "but its roster is fixed");

  // A metadata kind opening up must NOT drag rename/reorder open with it: those
  // still enter tabMutationTarget, which gates on TabManagement.
  const webAllowedOffBox = sess({
    backend_type: "ssh",
    tab_kinds: [
      { kind: "shell", allowed: false, reason: "needs a local worktree" },
      { kind: "web", allowed: true },
    ],
    tab_roster_mutable: false,
  });
  assert.equal(canCreateTabKind(webAllowedOffBox, "web"), true);
  assert.equal(canCreateTabKind(webAllowedOffBox, "shell"), false);
  assert.equal(
    canMutateTabRoster(webAllowedOffBox),
    false,
    "rename/reorder still hit tabMutationTarget, which the daemon refuses here",
  );
});

test("the create refusal is the daemon's own text, not a backend guess (#3060)", () => {
  const refused = sess({
    backend_type: "local",
    tab_kinds: [{ kind: "web", allowed: false, reason: "see #3062: not restored on the sandbox recovery path" }],
  });
  assert.equal(
    tabCreationUnavailableReason(refused),
    "see #3062: not restored on the sandbox recovery path",
    "the daemon names the requirement actually unmet; a client cannot know it",
  );
  // Archived still wins: the daemon refuses creation there for its own reason.
  assert.match(
    tabCreationUnavailableReason(sess({ backend_type: "local", liveness: Liveness.Archived })) ?? "",
    /Restore this session/,
  );
});

test("the tab-bar sig changes when WHICH kind is creatable changes (#3060)", () => {
  // Both snapshots allow something, so createReason is null in each and every other
  // value in the signature is equal. Only the SET differs. Without it in the sig the
  // bar does not rebuild, and the menu keeps offering the kind that just became
  // refused while omitting the one that just became available.
  const shellOnly = state({
    sessions: [
      sess({
        id: "a",
        tabs: [{ name: "agent", kind: 0 }],
        tab_kinds: [
          { kind: "shell", allowed: true },
          { kind: "vscode", allowed: false, reason: "no worktree to read" },
        ],
      }),
    ],
  });
  const vscodeOnly = state({
    sessions: [
      sess({
        id: "a",
        tabs: [{ name: "agent", kind: 0 }],
        tab_kinds: [
          { kind: "shell", allowed: false, reason: "no worktree to spawn in" },
          { kind: "vscode", allowed: true },
        ],
      }),
    ],
  });

  assert.equal(
    tabCreationUnavailableReason(vscodeOnly.sessions[0]),
    tabCreationUnavailableReason(shellOnly.sessions[0]),
    "premise: creation is available in both, so the reason cannot distinguish them",
  );
  assert.notEqual(tabBarSig(shellOnly), tabBarSig(vscodeOnly), "the bar must rebuild when the menu's kinds change");
});
