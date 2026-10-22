import assert from "node:assert/strict";
import test from "node:test";

import { taskVerbs } from "./tasks.js";
import type { TaskData } from "./types.js";

function task(over: Partial<TaskData> = {}): TaskData {
  return {
    id: "t1",
    name: "nightly sweep",
    prompt: "p",
    cron_expr: "0 3 * * *",
    project_path: "/repo",
    program: "claude",
    enabled: true,
    created_at: "2026-07-01T09:00:00Z",
    ...over,
  };
}

const noop = { edit() {}, trigger() {}, remove() {} };

test("an enabled cron task offers every verb, none blocked", () => {
  const verbs = taskVerbs(task(), noop);
  assert.deepEqual(verbs.map((v) => [v.label, v.blocked]), [
    ["Edit", undefined],
    ["Trigger", undefined],
    ["Remove", undefined],
  ]);
});

test("a disabled task keeps Trigger in the list, blocked with a reason", () => {
  // The phone menu renders this list alone, so dropping the verb would leave phones
  // with no Trigger entry at all.
  const trigger = taskVerbs(task({ enabled: false }), noop).find((v) => v.label === "Trigger");
  assert.equal(trigger?.blocked, "Enable the task to trigger it");
});

test("a watch task keeps Trigger in the list, blocked with a reason", () => {
  const trigger = taskVerbs(task({ cron_expr: "", watch_cmd: "true" }), noop).find((v) => v.label === "Trigger");
  assert.equal(trigger?.blocked, "Only scheduled tasks can be triggered");
});
