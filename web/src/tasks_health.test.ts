// Schedule health in the task list (#3626): the web's half of #3623.
//
// The daemon derives every verdict and puts it on the records ListTasks returns,
// so these are rendering rules, not derivation rules — and the point of pinning
// them is that the rail and this list must say the same words about the same
// task. Where a rule mirrors ui/automations.go, the test says so, because that
// is the invariant worth failing on.

import assert from "node:assert/strict";
import test from "node:test";

import { taskArmingSummary, taskHealthMark, taskHealthSummary, taskNeedsAttention } from "./tasks.js";
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

test("an overdue task is marked, and reads the same words as the rail", () => {
  const t = task({ overdue: true, missed_occurrences: 432 });
  assert.equal(taskNeedsAttention(t), true);
  assert.deepEqual(taskHealthMark(t), { icon: "triangle-alert", cls: "af-task-warn" });
  // ui/automations.go's attentionFragment, verbatim.
  assert.equal(taskHealthSummary(t), "overdue · missed 432");
});

test("a capped count renders as a floor, never as an exact number", () => {
  // The daemon's walk budget saturates; "10000" and "at least 10000" call for the
  // same action, but only one of them is true.
  const t = task({ overdue: true, missed_occurrences: 10000, missed_occurrences_capped: true });
  assert.equal(taskHealthSummary(t), "overdue · missed 10000+");
});

test("a count nobody took is not a count of zero", () => {
  // The budget can be spent before the daemon reaches a task, and it says so by
  // capping a zero — the task is still proven overdue.
  const t = task({ overdue: true, missed_occurrences: 0, missed_occurrences_capped: true });
  assert.equal(taskHealthSummary(t), "overdue");
});

test("a task the scheduler cannot fire is marked, and says which shape", () => {
  // The shape comes from the DAEMON's classifier, not from a second look at
  // cron_expr here — re-deriving it is what had other surfaces calling an absent
  // expression invalid. These are ui/automations.go's three strings, verbatim.
  const cases: [TaskData["unschedulable_reason"], string][] = [
    ["no-trigger", "No trigger"],
    ["invalid-expression", "Invalid cron expression"],
    ["no-occurrence", "No upcoming run"],
  ];
  for (const [reason, want] of cases) {
    const t = task({ unschedulable: true, unschedulable_reason: reason });
    assert.equal(taskNeedsAttention(t), true);
    assert.equal(taskHealthSummary(t), want);
  }
});

test("an unschedulable verdict with no reason still says the verdict", () => {
  // A daemon older than the reason field sends `unschedulable` alone. The row
  // must still be marked and still say something true — the one thing it must not
  // do is guess which shape by looking at cron_expr.
  const t = task({ cron_expr: "99 * * * *", unschedulable: true });
  assert.equal(taskNeedsAttention(t), true);
  assert.equal(taskHealthSummary(t), "Cannot be scheduled");
});

test("an unknown is neither healthy nor a failure", () => {
  const t = task({ unassessable: true });
  // Not counted as attention: that count answers "is anything wrong?", and an
  // unknown is not an answer of yes — the same line af doctor draws.
  assert.equal(taskNeedsAttention(t), false);
  assert.deepEqual(taskHealthMark(t), { icon: "circle-question", cls: "af-task-unknown" });
  assert.equal(taskHealthSummary(t), "Health unknown");
});

test("an unknown is muted in the text as well as the glyph", () => {
  // The summary takes the MARK's class, so the words and the glyph explaining
  // them cannot end up in different colours. Painting "Health unknown" with the
  // warning token would present something unestablished as a failure.
  const t = task({ unassessable: true });
  assert.equal(taskHealthMark(t)?.cls, "af-task-unknown");

  const overdue = task({ overdue: true, missed_occurrences: 2 });
  assert.equal(taskHealthMark(overdue)?.cls, "af-task-warn");
});

test("a healthy task carries no mark and no health text", () => {
  const t = task();
  assert.equal(taskNeedsAttention(t), false);
  assert.equal(taskHealthMark(t), null);
  assert.equal(taskHealthSummary(t), "");
});

test("next run comes from the live entry, and its absence is not an accusation", () => {
  assert.equal(taskArmingSummary(task({ next_run_at: "2026-09-03T03:00:00Z", arming: "armed" })), "next run 2026-09-03T03:00:00Z");
  assert.equal(taskArmingSummary(task({ arming: "not-armed" })), "enabled but not armed");
  // ABSENT arming means no daemon has reported on it — none running, or one
  // still starting. It must never render as "not armed".
  assert.equal(taskArmingSummary(task()), "");
  // Nor is a DISABLED task accused of being unarmed; it is not expected to fire.
  assert.equal(taskArmingSummary(task({ enabled: false, arming: "not-armed" })), "");
});
