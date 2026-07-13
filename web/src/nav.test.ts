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
  return { focus: "rail", modalOpen: false, orderedIds: IDS, selectedId: "b", ...over };
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
