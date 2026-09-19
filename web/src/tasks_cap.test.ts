import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { buildTask, capUnavailableReason, parseCapInput, type AddTaskInput } from "./tasks.js";

// The task form's concurrency-cap field (#4180, surface parity).
//
// The gate and the parse are the parts two surfaces can disagree about — the
// disagreement the issue calls the real bug — so both halves are pinned against
// the shared vectors in task/testdata/cap_vectors.json, the same
// shared-source-of-truth contract schedule.test.ts reads. The Go twins
// (task.CapApplies / task.CapUnavailableReason / task.ParseCapInput) answer the
// same file; a change lands in the vectors and BOTH implementations, never one.

const here = dirname(fileURLToPath(import.meta.url));
const FIXTURE_PATH = join(here, "..", "..", "task", "testdata", "cap_vectors.json");

interface Vectors {
  applies: { name: string; is_watch: boolean; target: string; applies: boolean; reason?: string }[];
  parse: { name: string; raw: string; ok: boolean; value?: number }[];
}

const vectors = JSON.parse(readFileSync(FIXTURE_PATH, "utf8")) as Vectors;
assert.ok(vectors.applies.length > 0 && vectors.parse.length > 0, `no vectors loaded from ${FIXTURE_PATH}`);

for (const v of vectors.applies) {
  test(`cap gate: ${v.name}`, () => {
    const reason = capUnavailableReason(v.is_watch ? "watch" : "cron", v.target);
    if (v.applies) {
      assert.equal(reason, null);
    } else {
      assert.equal(reason, v.reason, "the refusal copy is the shared contract — same words as the TUI");
    }
  });
}

for (const v of vectors.parse) {
  test(`cap parse: ${v.name}`, () => {
    const got = parseCapInput(v.raw);
    if (v.ok) {
      assert.equal(got, v.value, `parseCapInput(${JSON.stringify(v.raw)})`);
    } else {
      assert.equal(got, null, `parseCapInput(${JSON.stringify(v.raw)}) must be refused`);
    }
  });
}

const input: AddTaskInput = {
  name: "Nightly", projectPath: "/repo", trigger: "watch", cron: "", watchCmd: "tail -f q",
  prompt: "Work {{line}}", targetSession: "", program: "", maxConcurrentRuns: 3,
};

test("buildTask carries the cap on a shape that can hold it", () => {
  assert.equal(buildTask(input).max_concurrent_runs, 3);
  assert.equal(buildTask({ ...input, maxConcurrentRuns: 0 }).max_concurrent_runs, 0,
    "explicit 0 is unlimited and still sent — same as the update path");
  assert.equal(buildTask({ ...input, maxConcurrentRuns: undefined }).max_concurrent_runs, 0,
    "an absent cap is unlimited, not omitted-dirty");
});

test("buildTask drops the cap on shapes that cannot carry it", () => {
  assert.equal(buildTask({ ...input, trigger: "cron", cron: "0 9 * * *", watchCmd: "" }).max_concurrent_runs, 0,
    "a cron task stores no cap, whatever the input carried");
  assert.equal(buildTask({ ...input, targetSession: "reused" }).max_concurrent_runs, 0,
    "a target-session task stores no cap, whatever the input carried");
  assert.equal(buildTask({ ...input, targetSession: "   " }).max_concurrent_runs, 3,
    "a whitespace target is NO target — CanonicalTargetSession's rule");
});
