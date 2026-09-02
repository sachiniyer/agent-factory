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

test("a row's mark and its words describe the SAME fact", () => {
  // The mark and the summary run one precedence chain. Where they disagreed, a
  // task with nothing to measure from that the daemon had also refused to arm got
  // the WARNING glyph explained by "Health unknown" — a marked row whose text is
  // about something else, which is the leading-fragment design failing at its one
  // job. Reachable because arming is layered on separately: unschedulable,
  // overdue and unassessable are mutually exclusive, but any of them can coexist
  // with not-armed.
  const both = task({ unassessable: true, arming: "not-armed" });
  assert.deepEqual(taskHealthMark(both), { icon: "triangle-alert", cls: "af-task-warn" });
  assert.equal(taskHealthSummary(both), "enabled but not armed", "the stronger fact wins BOTH");

  // And with nothing stronger, the unknown keeps its own muted mark and its own
  // words.
  const unknownOnly = task({ unassessable: true });
  assert.deepEqual(taskHealthMark(unknownOnly), { icon: "circle-question", cls: "af-task-unknown" });
  assert.equal(taskHealthSummary(unknownOnly), "Health unknown");
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
  // The not-armed fact lives in the HEALTH fragment, which leads the line and
  // carries the mark — not in the dim tail that ellipsizes first.
  assert.equal(taskArmingSummary(task({ arming: "not-armed" })), "");
});

test("an enabled task the daemon is not holding is marked, and says so", () => {
  // It will not fire. That is #2929 exactly, and af doctor already calls it
  // actionable; a green tick beside "enabled but not armed" in the dim tail was
  // the same false clean bill this feature exists to remove.
  const t = task({ arming: "not-armed" });
  assert.equal(taskNeedsAttention(t), true);
  assert.deepEqual(taskHealthMark(t), { icon: "triangle-alert", cls: "af-task-warn" });
  assert.equal(taskHealthSummary(t), "enabled but not armed");
});

test("a DISABLED task is never accused of being unarmed", () => {
  // The daemon reports arming for every task, and "disabled and therefore not
  // armed" is a true, unsurprising reading of the same field. A task nobody asked
  // to run is not a finding.
  const t = task({ enabled: false, arming: "not-armed" });
  assert.equal(taskNeedsAttention(t), false);
  assert.equal(taskHealthMark(t), null);
  assert.equal(taskHealthSummary(t), "");
});

test("ABSENT arming is not an accusation either", () => {
  // No daemon has reported — none running, or one still starting. Reporting that
  // as not-armed would mark every task on the box during a daemon restart.
  const t = task();
  assert.equal(taskNeedsAttention(t), false);
  assert.equal(taskHealthSummary(t), "");
});

test("a stronger verdict wins the line, as af doctor orders them", () => {
  // An overdue task is usually also unarmed. "It has missed 432 fires" is the
  // sentence worth the width, and doctor does not name a row twice either.
  const t = task({ overdue: true, missed_occurrences: 432, arming: "not-armed" });
  assert.equal(taskHealthSummary(t), "overdue · missed 432");

  const impossible = task({ unschedulable: true, unschedulable_reason: "no-occurrence", arming: "not-armed" });
  assert.equal(taskHealthSummary(impossible), "No upcoming run");
});

test("a mark always has words, on every state that can carry one", () => {
  // The glyph is aria-hidden — it exists to survive visual clipping, which is a
  // sighted-reader problem — so the VISIBLE summary is the only thing a screen
  // reader gets. A state that could be marked without words would be a row that
  // announces nothing at all. Exhaustive over the states taskHealthMark answers.
  const marked: TaskData[] = [
    task({ overdue: true, missed_occurrences: 5 }),
    task({ overdue: true, missed_occurrences: 0, missed_occurrences_capped: true }),
    task({ unschedulable: true, unschedulable_reason: "no-trigger" }),
    task({ unschedulable: true, unschedulable_reason: "invalid-expression" }),
    task({ unschedulable: true, unschedulable_reason: "no-occurrence" }),
    task({ unschedulable: true }),
    task({ arming: "not-armed" }),
    task({ unassessable: true }),
    task({ unassessable: true, arming: "not-armed" }),
  ];
  for (const t of marked) {
    assert.notEqual(taskHealthMark(t), null, `expected a mark for ${JSON.stringify(t)}`);
    assert.notEqual(taskHealthSummary(t), "", `a mark with no words for ${JSON.stringify(t)}`);
  }

  // And the converse, so the invariant is not satisfied by marking everything:
  // an unmarked row has nothing to announce either.
  const healthy = task();
  assert.equal(taskHealthMark(healthy), null);
  assert.equal(taskHealthSummary(healthy), "");
});
