import assert from "node:assert/strict";
import test from "node:test";
import { buildTask, type AddTaskInput } from "./tasks.js";
import * as tasks from "./tasks.js";

const input: AddTaskInput & { onComplete: string } = {
  name: "Nightly", projectPath: "/repo", trigger: "cron", cron: "0 9 * * *",
  watchCmd: "", prompt: "Work", targetSession: "", program: "", onComplete: "archive",
};

test("buildTask sends the chosen spawned-session lifecycle", () => {
  assert.equal(buildTask(input).on_complete, "archive");
  assert.equal(buildTask({ ...input, onComplete: "kill" }).on_complete, "kill");
});

test("buildTask stores keep as empty and clears lifecycle for a target session", () => {
  assert.equal(buildTask({ ...input, onComplete: "keep" }).on_complete, "");
  assert.equal(buildTask({ ...input, onComplete: "" }).on_complete, "");
  assert.equal(buildTask({ ...input, targetSession: "reused" }).on_complete, "");
});

test("target-session state replaces the picker with its reason", () => {
  assert.equal(tasks.onCompleteUnavailableReason(""), null);
  assert.equal(tasks.onCompleteUnavailableReason("  "), null);
  assert.match(tasks.onCompleteUnavailableReason("reused")!, /target session.*reused/);
});
