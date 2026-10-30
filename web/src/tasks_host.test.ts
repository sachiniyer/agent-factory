import assert from "node:assert/strict";
import test from "node:test";
import { buildTask, hostSelection, type AddTaskInput } from "./tasks.js";

const input: AddTaskInput = {
  name: "Nightly", projectPath: "/repo", trigger: "cron", cron: "0 9 * * *",
  watchCmd: "", prompt: "Work", targetSession: "", program: "",
};

// The host rides the task like on_complete does (#5571 W6.3).
test("buildTask sends the picked listed host and an empty one when none is picked", () => {
  assert.equal(buildTask({ ...input, host: "box1" }).host, "box1");
  assert.equal(buildTask({ ...input, host: "  box1 " }).host, "box1");
  // "" is omitted by the daemon's omitempty on store: no host is stored.
  assert.equal(buildTask(input).host, "");
  assert.equal(buildTask({ ...input, host: "" }).host, "");
  assert.equal(JSON.stringify(buildTask(input)).includes('"host":"box1"'), false);
});

// #5967: clearing a task's host to "Repo backend" must survive the re-render.
test("an untouched host field keeps its seed; a touched one keeps its value, even empty", () => {
  assert.equal(hostSelection(false, "", "box1"), "box1");
  assert.equal(hostSelection(true, "", "box1"), "");
  assert.equal(hostSelection(true, "box2", "box1"), "box2");
});
