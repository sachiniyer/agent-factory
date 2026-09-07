import { test } from "node:test";
import assert from "node:assert/strict";
import { handoffAccountChoices } from "./handoff_accounts.js";

test("handoff accounts belong to the selected agent and identify its project default", () => {
 const choices = handoffAccountChoices({ agents: ["claude", "codex"], defaults: { codex: "personal" }, entries: [
  { agent: "claude", name: "foreign", logged_in: true, dir: "", registration_only: false },
  { agent: "codex", name: "work", logged_in: true, dir: "", registration_only: false },
  { agent: "codex", name: "personal", logged_in: true, dir: "", registration_only: false },
 ] }, "codex", "work");
 assert.deepEqual(choices, [{ value: "personal", label: "personal (project default)" }]);
});
