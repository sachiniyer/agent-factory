import test from "node:test";
import assert from "node:assert/strict";
import { keys, summarize, regressions } from "./check.mjs";
const row = value => Object.fromEntries(keys.map(k => [k, value]));
const budgets = () => Object.fromEntries(keys.map(k => [k, { baseline: 10, margin: 2 }]));
test("mean at budget passes; one metric above budget fails", () => {
  const metrics = summarize([row(11), row(12), row(13)]);
  assert.deepEqual(regressions(metrics, budgets()), []);
  metrics.echo_ms.mean += .001;
  assert.deepEqual(regressions(metrics, budgets()), ["echo_ms"]);
  assert.equal(metrics.rail_ms.min, 11);
  assert.equal(metrics.rail_ms.max, 13);
  assert.equal(metrics.rail_ms.stddev, Math.sqrt(2 / 3));
});
test("partial, corrupt and absent observations cannot silently turn green", () => {
  assert.throws(() => summarize([row(1), row(1)]));
  for (const bad of [undefined, NaN, Infinity, -1, "1"]) {
    assert.throws(() => summarize([row(1), { ...row(1), echo_ms: bad }, row(1)]));
  }
  assert.throws(() => regressions(summarize([row(1), row(1), row(1)]), {}));
  const b = budgets(); b.echo_ms.margin = -1;
  assert.throws(() => regressions(summarize([row(1), row(1), row(1)]), b));
});
