import { test } from "node:test";
import assert from "node:assert/strict";
import { type HandoffLoadState, handoffAccountChoices, handoffLoadError } from "./handoff_accounts.js";

test("handoff accounts belong to the selected agent and identify its project default", () => {
 const choices = handoffAccountChoices({ agents: ["claude", "codex"], defaults: { codex: "personal" }, entries: [
  { agent: "claude", name: "foreign", logged_in: true, dir: "", registration_only: false },
  { agent: "codex", name: "work", logged_in: true, dir: "", registration_only: false },
  { agent: "codex", name: "personal", logged_in: true, dir: "", registration_only: false },
 ] }, "codex", "work");
 assert.deepEqual(choices.map(({ value, label, logged_in }) => ({ value, label, logged_in })),
  [{ value: "personal", label: "personal — project default", logged_in: true }]);
});

test("handoff accounts disclose missing credentials even for the project default", () => {
 const choices = handoffAccountChoices({ agents: ["claude"], defaults: { claude: "personal" }, entries: [
  { agent: "claude", name: "personal", logged_in: false, dir: "", registration_only: false },
 ] }, "claude");
 assert.match(choices[0].label, /not logged in/);
 assert.match(choices[0].note, /has no claude credential yet/);
 assert.equal(choices[0].logged_in, false);
});

// The dialog folds each load reply into these facts and re-derives the error
// line from all of them (#7102). Replaying both arrival orders must end on the
// same line; a registry failure must never end hidden.
type LoadReply = "catalog ok" | "catalog failed" | "registry ok" | "registry failed";
function settleInOrder(replies: LoadReply[], scoped: boolean, targetsWhenSettled: number): string | null {
 const state: HandoffLoadState = { catalogFailed: false, accountsFailed: false, scoped, targets: null };
 let catalog = false, registry = false;
 let line: string | null = null;
 for (const reply of replies) {
  if (reply === "catalog ok") catalog = true;
  if (reply === "catalog failed") state.catalogFailed = true;
  if (reply === "registry ok") registry = true;
  if (reply === "registry failed") { registry = true; state.accountsFailed = true; }
  if (catalog && registry) state.targets = targetsWhenSettled;
  line = handoffLoadError(state);
 }
 return line;
}

test("a registry failure stays on the handoff dialog whichever reply lands last", () => {
 for (const [scoped, targets, want] of [
  [false, 1, "Could not load accounts. You can still hand off to another agent using its ambient identity."],
  // A scoped session has no target BECAUSE the registry is unreadable, so
  // the line names the failure rather than an empty registry.
  [true, 0, "Could not load accounts. Try again to choose a registered target account."],
 ] as const) {
  assert.equal(settleInOrder(["registry failed", "catalog ok"], scoped, targets), want, `scoped=${scoped}: registry first`);
  assert.equal(settleInOrder(["catalog ok", "registry failed"], scoped, targets), want, `scoped=${scoped}: catalog first`);
 }
});

test("an agent-list failure outranks the registry's in either order", () => {
 const want = "Could not load the agent list. Try again.";
 assert.equal(settleInOrder(["registry failed", "catalog failed"], false, 0), want);
 assert.equal(settleInOrder(["catalog failed", "registry failed"], false, 0), want);
 assert.equal(settleInOrder(["catalog failed", "registry ok"], true, 0), want);
});

test("loaded registries report only a missing target, and nothing while a load is pending", () => {
 assert.equal(handoffLoadError({ catalogFailed: false, accountsFailed: false, scoped: false, targets: null }), null);
 assert.equal(handoffLoadError({ catalogFailed: false, accountsFailed: false, scoped: false, targets: 2 }), null);
 assert.equal(handoffLoadError({ catalogFailed: false, accountsFailed: false, scoped: false, targets: 0 }),
  "No other agent is available to hand off to.");
 assert.equal(handoffLoadError({ catalogFailed: false, accountsFailed: false, scoped: true, targets: 0 }),
  "No registered target account is available to hand off to.");
});
