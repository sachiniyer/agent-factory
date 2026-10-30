import assert from "node:assert/strict";
import { test } from "node:test";
import { DocSync } from "./docs_sync.js";
import { runScenario, type MakeSync, type SyncDepsLike, type SyncLike } from "./docs_sync_harness.js";

const makeSync: MakeSync = (deps: SyncDepsLike): SyncLike => new DocSync(deps as ConstructorParameters<typeof DocSync>[0]);

// Random interleavings of user edits, agent edits, events, timers, document
// switches, tab closes, outages and conflict resolutions against an in-memory
// daemon whose replies are delayed and reordered. Invariants:
//   - no user keystroke is ever lost;
//   - the editor never shows content older than what the user typed;
//   - an unresolved conflict draft survives a document switch and a tab close;
//   - leaving during a conflict never writes conflict markers into the doc;
//   - after a reconnect the pane matches the daemon.
// This is the test the ten Codex findings on 3a091edf12 were missing: it fails on
// that engine (see the PR thread) and passes on the reducer.
const SEEDS = 300;

test("sync engine: no collisions, hundreds of random interleavings keep every invariant", async () => {
  for (let seed = 1; seed <= SEEDS; seed++) {
    const v = await runScenario({ seed, steps: 90, collide: false, makeSync });
    assert.equal(v, null, `seed ${seed}: ${v?.invariant}: ${v?.detail}\n${process.env.AF_TRACE ? v?.trace.join("\n") : ""}`);
  }
});

test("sync engine: with collisions, conflicts, switches and closes keep every invariant", async () => {
  for (let seed = 1; seed <= SEEDS; seed++) {
    const v = await runScenario({ seed: seed * 7919, steps: 110, collide: true, makeSync });
    assert.equal(v, null, `seed ${seed * 7919}: ${v?.invariant}: ${v?.detail}\n${process.env.AF_TRACE ? v?.trace.join("\n") : ""}`);
  }
});

test("sync engine: failed saves, lost replies and the page closing never lose an edit", async () => {
  for (let seed = 1; seed <= SEEDS; seed++) {
    const v = await runScenario({ seed: seed * 104729, steps: 120, collide: false, faults: 0.3, makeSync });
    assert.equal(v, null, `seed ${seed * 104729}: ${v?.invariant}: ${v?.detail}\n${process.env.AF_TRACE ? v?.trace.join("\n") : ""}`);
  }
  for (let seed = 1; seed <= SEEDS; seed++) {
    const v = await runScenario({ seed: seed * 15485863, steps: 120, collide: true, faults: 0.3, makeSync });
    assert.equal(v, null, `seed ${seed * 15485863}: ${v?.invariant}: ${v?.detail}\n${process.env.AF_TRACE ? v?.trace.join("\n") : ""}`);
  }
});

test("sync engine: every external event (remote delete, daemon restart, second window, hung and refused saves, head rolled back) keeps every invariant", async () => {
  for (const [collide, mult] of [[false, 6700417], [true, 67280421310721]] as const) {
    for (let seed = 1; seed <= SEEDS; seed++) {
      const v = await runScenario({ seed: Number((BigInt(seed) * BigInt(mult)) % 4294967291n), steps: 140, collide, faults: 0.25, chaos: true, makeSync });
      assert.equal(v, null, `seed ${seed} (collide ${collide}): ${v?.invariant}: ${v?.detail}\n${process.env.AF_TRACE ? v?.trace.join("\n") : ""}`);
    }
  }
});
