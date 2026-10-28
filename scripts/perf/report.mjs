import { readFileSync, writeFileSync, readdirSync } from "node:fs";
import { gzipSync } from "node:zlib";
import assert from "node:assert/strict";
import { keys, summarize, budgetFor, regressions } from "./check.mjs";
const root = "/work/web/test-results";
const web = JSON.parse(readFileSync(`${root}/web-runs.json`));
const tui = JSON.parse(readFileSync(`${root}/tui-runs.json`));
const files = readdirSync("/work/web/dist", { recursive: true }).filter(f => /\.(js|css)$/.test(f));
assert(files.length > 0);
const bundle = () => files.reduce((sum, file) => {
  const bytes = readFileSync(`/work/web/dist/${file}`);
  return { raw_bytes: sum.raw_bytes + bytes.length, gzip_bytes: sum.gzip_bytes + gzipSync(bytes, { level: 9 }).length };
}, { raw_bytes: 0, gzip_bytes: 0 });
assert.equal(web.length, 3); assert.equal(tui.length, 3);
const runs = web.map((row, i) => ({ ...row, ...tui[i], ...bundle() }));
const metrics = summarize(runs);
writeFileSync(`${root}/metrics.json`, JSON.stringify(metrics, null, 2) + "\n");
const table = "| Metric | Mean | Min–max | SD | Budget |\n| --- | ---: | ---: | ---: | ---: |\n";
let markdown = table;
if (process.env.AF_PERF_RECORD === "1") {
  assert(!process.env.CI, "CI cannot record baselines");
  const budgets = Object.fromEntries(keys.map(key => [key, {
    baseline: metrics[key].mean,
    samples: metrics[key].samples, stddev: metrics[key].stddev,
    // Bytes are deterministic. Timings allow shared-runner scheduling noise;
    // zero layout shift needs an absolute allowance instead of multiplication.
    margin: key.endsWith("bytes") ? metrics[key].mean * .05 : key.endsWith("shift") ? .01 : Math.max(metrics[key].mean, 50),
  }]));
  writeFileSync(`${root}/baselines.json`, JSON.stringify(budgets, null, 2) + "\n");
}
const budgets = JSON.parse(readFileSync(process.env.AF_PERF_RECORD === "1" ? `${root}/baselines.json` : "/work/scripts/perf/baselines.json"));
const failed = regressions(metrics, budgets);
for (const key of keys) {
  const m = metrics[key];
  const budget = budgetFor(budgets, key);
  markdown += `| ${key} | ${m.mean.toFixed(3)} | ${m.min.toFixed(3)}–${m.max.toFixed(3)} | ${m.stddev.toFixed(3)} | ${budget.toFixed(3)} |\n`;
  if (m.mean > budget) console.error(`${key}: ${m.mean} > ${budget}`);
}
writeFileSync(`${root}/metrics.md`, markdown);
console.log(markdown);
if (failed.length) process.exitCode = 1;
