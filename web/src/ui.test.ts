// Unit coverage for tabBarSig (#1737 follow-up). The signature decides WHEN the tab
// bar is rebuilt: it must change when anything the bar DRAWS changes (tab set, active
// index, shown-in-a-pane set, manageability) and must NOT change on an unrelated
// session-status snapshot. That second property is the fix — a status-only rebuild
// would replaceChildren() the bar and destroy the button a user just grabbed to drag a
// freshly-created tab, aborting the native HTML5 drag mid-gesture.

import { test } from "node:test";
import assert from "node:assert/strict";

import { tabBarSig } from "./ui.js";
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
