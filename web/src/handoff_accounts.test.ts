import { test } from "node:test";
import assert from "node:assert/strict";
import { handoffAccountChoices } from "./handoff_accounts.js";

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
