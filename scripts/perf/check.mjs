import assert from "node:assert/strict";
export const keys = ["raw_bytes", "gzip_bytes", "first_terminal_ms", "echo_ms", "rail_ms", "load_shift", "snapshot_shift", "frame_ms", "key_render_ms"];

export function summarize(runs) {
  assert.equal(runs.length, 3, "exactly three runs required");
  return Object.fromEntries(keys.map(key => {
    const samples = runs.map(r => r[key]);
    assert(samples.every(v => Number.isFinite(v) && v >= 0), `invalid/missing ${key}`);
    const mean = samples.reduce((a, b) => a + b, 0) / 3;
    return [key, { samples, mean, min: Math.min(...samples), max: Math.max(...samples),
      stddev: Math.sqrt(samples.reduce((s, v) => s + (v - mean) ** 2, 0) / 3) }];
  }));
}

export function budgetFor(budgets, key) {
  const b = budgets[key];
  assert(b && Number.isFinite(b.baseline) && b.baseline >= 0 && Number.isFinite(b.margin) && b.margin >= 0, `invalid/missing budget ${key}`);
  return b.baseline + b.margin;
}

export function regressions(metrics, budgets) {
  return keys.filter(key => {
    assert(Number.isFinite(metrics[key]?.mean), `missing metric ${key}`);
    return metrics[key].mean > budgetFor(budgets, key);
  });
}
