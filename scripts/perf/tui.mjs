import { execFileSync } from "node:child_process";
import { performance } from "node:perf_hooks";
import { writeFileSync } from "node:fs";
const driver = 'set -euo pipefail; source /work/scripts/tui-driver.sh; ';
const call = command => execFileSync("bash", ["-c", driver + command], { encoding: "utf8" });
const runs = [];
for (let i = 0; i < 3; i++) {
  // A full overlay frame: request -> completed footer visible through tmux.
  // This includes event dispatch, View/layout, Bubble Tea flush and driver cost.
  let start = performance.now();
  call("af_send ','; af_wait_for \"$_AF_CONFIG_HINT\" 30 'config frame painted'");
  const frame_ms = performance.now() - start;
  start = performance.now();
  call("af_send Escape; af_wait_gone \"$_AF_CONFIG_HINT\" 30 'session frame restored'; af_wait_for 'Sessions \\(1000\\)' 30");
  runs.push({ frame_ms, key_render_ms: performance.now() - start });
}
writeFileSync("/work/web/test-results/tui-runs.json", JSON.stringify(runs, null, 2));
execFileSync("tmux", ["kill-session", "-t", process.env.AF_DRIVER_SESSION]);
