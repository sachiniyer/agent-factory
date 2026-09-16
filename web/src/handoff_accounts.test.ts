import { test } from "node:test";
import assert from "node:assert/strict";
import { handoffAccountChoices, handoffAccountPinned } from "./handoff_accounts.js";

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

// #4404 review: a routed session's account is af's pick, not the user's pin.
// The daemon releases it on an agent-only handoff, so the modal must keep the
// ordinary agent picker — including a target agent with no registered account.
test("only a pinned account demands a target account on handoff", () => {
 assert.equal(handoffAccountPinned(undefined, undefined), false, "an ambient session is unpinned");
 assert.equal(handoffAccountPinned("", false), false);
 assert.equal(handoffAccountPinned("work", undefined), true, "an older daemon's row without the bit stays a pin");
 assert.equal(handoffAccountPinned("work", false), true);
 assert.equal(handoffAccountPinned("work", true), false, "af's own pick is released, not carried");
});
