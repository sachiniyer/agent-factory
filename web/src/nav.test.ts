// Tests for the keyboard/focus state machine (#1693): the TUI-style nav-vs-attach
// model that makes j/k ALWAYS navigate the rail. These pin the exact transitions
// the play-test exercises — nav mode moves the selection, Enter attaches, a focused
// terminal sends to the agent, Escape returns to nav, and a modal owns the keyboard
// — with no DOM and no daemon, exactly as sessions.test.ts pins the list reducer.

import { test } from "node:test";
import assert from "node:assert/strict";

import { type NavContext, decideKey, nextSelection } from "./nav.js";

const IDS = ["a", "b", "c"];

function ctx(over: Partial<NavContext> = {}): NavContext {
  return {
    focus: "rail",
    modalOpen: false,
    orderedIds: IDS,
    selectedId: "b",
    tabCount: 1,
    activeTab: 0,
    tabManagement: true,
    ...over,
  };
}

test("nextSelection: j/k move within the list and clamp at the ends", () => {
  assert.equal(nextSelection(IDS, "b", 1), "c");
  assert.equal(nextSelection(IDS, "b", -1), "a");
  assert.equal(nextSelection(IDS, "c", 1), "c", "clamped at the bottom");
  assert.equal(nextSelection(IDS, "a", -1), "a", "clamped at the top");
});

test("nextSelection: from no selection, down lands first and up lands last", () => {
  assert.equal(nextSelection(IDS, null, 1), "a");
  assert.equal(nextSelection(IDS, null, -1), "c");
  assert.equal(nextSelection([], null, 1), null, "empty list has nothing to select");
});

test("nav mode: j/k and arrows move the selection", () => {
  assert.deepEqual(decideKey("j", ctx()), { kind: "select", id: "c" });
  assert.deepEqual(decideKey("ArrowDown", ctx()), { kind: "select", id: "c" });
  assert.deepEqual(decideKey("k", ctx()), { kind: "select", id: "a" });
  assert.deepEqual(decideKey("ArrowUp", ctx()), { kind: "select", id: "a" });
});

test("nav mode: an unrelated key is not ours (passes through)", () => {
  assert.deepEqual(decideKey("x", ctx()), { kind: "none" });
  assert.deepEqual(decideKey("Escape", ctx()), { kind: "none" }, "Escape does nothing in nav mode");
});

test("nav mode: Enter on a selection attaches; Enter with none selected is a no-op", () => {
  assert.deepEqual(decideKey("Enter", ctx({ selectedId: "b" })), { kind: "attach" });
  assert.deepEqual(decideKey("Enter", ctx({ selectedId: null })), { kind: "none" });
});

test("terminal mode: keys go to the agent; only Escape returns to the rail", () => {
  const t = ctx({ focus: "terminal" });
  assert.deepEqual(decideKey("j", t), { kind: "none" }, "j reaches the agent, does NOT navigate");
  assert.deepEqual(decideKey("ArrowDown", t), { kind: "none" });
  assert.deepEqual(decideKey("Enter", t), { kind: "none" }, "Enter reaches the agent");
  assert.deepEqual(decideKey("x", t), { kind: "none" });
  assert.deepEqual(decideKey("Escape", t), { kind: "toRail" }, "Escape is the escape hatch back to nav");
});

test("modal mode: the modal owns the keyboard — only Escape (to cancel) is ours", () => {
  const m = ctx({ modalOpen: true });
  assert.deepEqual(decideKey("Escape", m), { kind: "closeModal" });
  assert.deepEqual(decideKey("j", m), { kind: "none" }, "j types into the form, not the rail");
  assert.deepEqual(decideKey("Enter", m), { kind: "none" }, "Enter is the form's own submit");
});

test("modal precedence: a modal over a focused terminal still routes Escape to the modal", () => {
  const both = ctx({ modalOpen: true, focus: "terminal" });
  assert.deepEqual(decideKey("Escape", both), { kind: "closeModal" }, "modal wins over terminal");
});

test("nav mode: 1-9 switch to an existing tab of the selected session", () => {
  const three = ctx({ tabCount: 3, activeTab: 0 });
  assert.deepEqual(decideKey("2", three), { kind: "switchTab", index: 1 });
  assert.deepEqual(decideKey("3", three), { kind: "switchTab", index: 2 });
  // A digit past the last tab is a no-op (passes through).
  assert.deepEqual(decideKey("4", three), { kind: "none" }, "no 4th tab to switch to");
  // The already-active tab is a no-op (no needless terminal rebuild).
  assert.deepEqual(decideKey("1", three), { kind: "none" }, "already on tab 1");
  // With no selection there is nothing to switch.
  assert.deepEqual(decideKey("2", ctx({ selectedId: null, tabCount: 3 })), { kind: "none" });
});

test("nav mode: t creates a tab; w closes the active non-agent tab", () => {
  // t always works for a tab-managed session (mirrors TUI `t` → AddShellTab).
  assert.deepEqual(decideKey("t", ctx()), { kind: "newTab" });
  // w on the agent tab (index 0) is refused — kill the session instead.
  assert.deepEqual(decideKey("w", ctx({ activeTab: 0 })), { kind: "none" }, "agent tab is unclosable");
  // w on a non-agent tab closes it.
  assert.deepEqual(decideKey("w", ctx({ tabCount: 2, activeTab: 1 })), { kind: "closeTab" });
});

test("nav mode: remote sessions can't manage tabs (t/w pass through) but can still switch", () => {
  const remote = ctx({ tabManagement: false, tabCount: 2, activeTab: 0 });
  assert.deepEqual(decideKey("t", remote), { kind: "none" }, "no new tab on a remote session");
  assert.deepEqual(decideKey("w", ctx({ tabManagement: false, tabCount: 2, activeTab: 1 })), { kind: "none" });
  // Switching among the fixed tabs of a remote session is still fine.
  assert.deepEqual(decideKey("2", remote), { kind: "switchTab", index: 1 });
});

test("terminal mode: tab keys reach the agent, not the tab bar", () => {
  const t = ctx({ focus: "terminal", tabCount: 3, activeTab: 0 });
  assert.deepEqual(decideKey("2", t), { kind: "none" }, "a digit reaches the shell, not the tab bar");
  assert.deepEqual(decideKey("t", t), { kind: "none" }, "t reaches the agent");
  assert.deepEqual(decideKey("w", ctx({ focus: "terminal", tabCount: 2, activeTab: 1 })), { kind: "none" });
});
