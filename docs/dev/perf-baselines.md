# Performance and visual baselines

Issue #3908, part of #3906. Run `make perf-container`. The performance job
runs on every PR and merge-group, including docs-only changes, and is a dependency
of the required **Build** check. It uses the same 35-minute harness / 40-minute job
limits as Web selftest. It builds the committed web bundle into `af`; the separate
Web job proves that bundle matches the TypeScript source.

## Isolation and fixture

The only entry point is `scripts/testbox.sh perf` (also `make perf-container`).
It reuses the Playwright image, cache volumes, read-only `/src`, per-run artifact
mount, PID and memory limits, and automatic container teardown from Web selftest.
No ports, host tmux socket, AF home, credentials or daemon are mounted. The daemon,
CLI, TUI and browser all run inside the container. Do not run the inner scripts
on the host or point them at an existing daemon.

First, the existing demo entry point seeds its three shell stand-ins; the demo
flows create the fourth. After photographing both themes, the harness stops and
**waits for the daemon to exit** before editing its per-repo `instances.json`.
`seed.mjs` preserves those four live sessions and adds 996 unique storage records,
then restarts the same daemon. The additional records are lost, startup-unknown,
with a persisted terminal recovery failure: inert, visible in the ordinary rail,
and never eligible for automatic spawning. They have no worktree directories or
agent processes. The browser asserts exactly 1,000 rendered rows; the driver
asserts `Sessions (1000)`; a tmux session-count ceiling detects accidental spawning.
This measures client session scale, not the CPU or IO load of 1,000 working agents.

## Method

Three fresh browser contexts run serially at 1440×900, followed by three driver
samples at 120×40 against the same daemon. No retries or timing-based success
criteria. Browser times use `performance.now()`. A MutationObserver detects the
expected rendered text/rows, followed by two `requestAnimationFrame` callbacks to
cross a paint opportunity. This measures DOM-terminal presentation rather than
socket-open or API-response time. Headless Chromium does not measure physical
monitor scanout. The observer and driver add overhead; comparisons keep it fixed.

| Metric | Start → observable completion |
| --- | --- |
| raw/gzip bytes | Sum of every shipped JS and CSS file under `web/dist`; each file gzip level 9 independently, excluding maps and images; recomputed three times |
| first terminal | Navigation time origin → first nonblank PTY glyph painted; select `add-json-export` as soon as the 1,000-row rail is available |
| keystroke echo | Browser keydown → that previously absent ASCII character appears in the terminal DOM and crosses a paint opportunity; actual PTY echo, no mocked socket |
| rail render | First Snapshot JSON decoded → all 1,000 rail rows present and a paint opportunity; includes row construction, layout and scheduling, excludes HTTP transfer |
| load layout shift | Sum of Layout Instability entry values through first-terminal paint, completed fixture transcript and initial event resync |
| snapshot layout shift | Reset accumulator, reconnect the real event WebSocket, rename the real diff tab via the daemon API, await its new label plus the accepted resync Snapshot and paint; includes recent-input shifts, so these are unfiltered shift sums, not Core Web Vitals CLS session windows |
| TUI full frame | Driver sends `,` → config overlay's completed footer appears in tmux capture; includes dispatch, View/layout, renderer flush, shell/driver and capture overhead |
| TUI key-to-render | Driver sends Escape → overlay footer disappears and the 1,000-session rail is visible again; same end-to-end transport |

TUI frame time is a user-observable full-frame turnaround, **not isolated Go View
CPU time**. The two TUI cases exercise opening and dismissing a full overlay over
the populated session model. The 5ms driver poll interval bounds observation
resolution in addition to capture/IPC cost. Timeouts only fail a missing event;
they never establish that a frame completed.

## Recorded baseline and budgets

Measured 2026-09-05 on Linux amd64, Node/Chromium from the pinned
Playwright 1.56.1 Noble image, Go 1.25.0, 4GiB container memory limit. The warm
end-to-end run took about three minutes, including the existing PR-badge sweep.
The committed `scripts/perf/baselines.json` is the budget source. The table below
reports the arithmetic mean, range and population standard deviation across three
runs. Each CI run uploads the individual samples, summary JSON/table, and any
Playwright traces/diff images under the `perf-baselines` artifact.

| Metric | Mean | Min–max | SD | Budget |
| --- | ---: | ---: | ---: | ---: |
| raw_bytes | 878059.000 | 878059.000–878059.000 | 0.000 | 921961.950 |
| gzip_bytes | 196682.000 | 196682.000–196682.000 | 0.000 | 206516.100 |
| first_terminal_ms | 6974.800 | 5912.400–7654.300 | 760.926 | 13949.600 |
| echo_ms | 388.000 | 380.700–400.400 | 8.814 | 776.000 |
| rail_ms | 932.967 | 894.600–968.400 | 30.200 | 1865.933 |
| load_shift | 0.000 | 0.000–0.000 | 0.000 | 0.010 |
| snapshot_shift | 0.000 | 0.000–0.000 | 0.000 | 0.010 |
| frame_ms | 480.005 | 375.747–679.179 | 140.888 | 960.011 |
| key_render_ms | 350.640 | 264.172–408.009 | 62.222 | 701.279 |

Budget = baseline mean + margin. For deterministic bundle bytes the margin is
5%, large enough for small features but small enough to catch an unexpected
payload increase. Timing margin is 100% of the baseline, with a 50ms absolute
floor, to tolerate shared-runner scheduling and sub-frame observation noise.
Layout-shift margin is an absolute 0.01 (multiplying a zero baseline would allow
no noise). These are deliberately initial regression budgets, not latency SLOs;
P3 should tighten them after the implementation improves. CI compares the
three-run mean and fails on missing, negative or non-finite samples, missing
budgets, or a mean above its budget. It never learns a new baseline in CI.

To deliberately rebaseline, run `AF_PERF_RECORD=1 make perf-container`, inspect
`web/test-results/<run>/metrics.json` and `baselines.json`, then copy the latter to
`scripts/perf/baselines.json` and update this table with `metrics.md`. Explain the
reason in the PR. A slower result is evidence to investigate, not an automatic
reason to move a budget.

## Demo stills and intentional redesigns

`playwright.visual.config.ts` drives the **same six demo beats** as the recorder:
dashboard, new-session, agent-tab, review, tasks and config-accounts, in light and
dark. It omits video, conversion and video pacing. It waits for final stand-in
output and a stable terminal before shooting. Goldens are committed under
`web/selftest/goldens`; missing goldens fail normally. Playwright pixel-diffs each
stabilized image, permits **zero differing pixels** above its 0.2 per-pixel color
distance threshold, and uploads actual/expected/diff images on failure.

The browser wall clock is fixed at 2000-01-01 so relative pane ages clamp to zero;
its timers still advance. Only nondeterministic regions are suppressed: terminal
cursor and task schedule/next-run metadata (which depends on the daemon's current
clock). The surrounding task rows, names, controls and layout remain checked.
The agent-tab still uses completed output in both themes, rather than racing an
intermediate line as a video can. This gives intentional redesigns a stable oracle.

Update goldens explicitly, inside the same fence:

```bash
AF_UPDATE_GOLDENS=1 make perf-container
# Use the run directory printed by the harness; review every changed image.
cp web/test-results/<run>/goldens/*.png web/selftest/goldens/
make perf-container
```

Commit the reviewed PNGs with the design change. Update mode writes candidates
to the artifact mount, never to the read-only checkout. `CI` forbids both golden
updates and baseline recording. `make demo-assets` remains the paced documentation
video recorder; it does not silently overwrite the regression goldens.
