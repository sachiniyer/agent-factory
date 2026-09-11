# Performance and visual baselines

Issue #3908, part of #3906. Run `make perf-container`. The performance job
runs for PRs touching `web/` (including Playwright configs and goldens), `app/`,
`ui/`, `scripts/perf/`, or `scripts/container/`, and is a dependency
of the required **Build** check. It uses the same 35-minute harness / 40-minute job
limits as Web selftest. The shared scope job computes both decisions from the
same rename-safe diff, with a tested path list; docs-only and gate-only PRs skip
performance successfully. Unknown diffs and merge-group events run it. The Web
job owns the application typecheck and unit suite; perf compiles only its harness
and tests its budget checker. It builds the committed web bundle into `af`; the separate
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

The accepted resync also records `snapshot_rail_ms` in `web-runs.json`: Snapshot
JSON decoded → the accepted resync marker and two animation frames. This is a
supplemental update measurement; the original `rail_ms` still measures initial
construction, with its observer unchanged. Before reconnecting, the harness retains
all 1,000 row nodes. After resync it asserts identical row identities and zero DOM
writes inside the 996 unchanged seeded rows, including attribute writes and row
insertions/removals. The audit requires all 996 unique fixture IDs in both the
Snapshot and DOM; decorated display titles cannot produce an empty cohort. The
four live rows may receive real status updates. The DOM audit runs only during this
resync, so it adds no observation overhead to the initial rail or echo measurements.

`terminal_surface_ms` additionally records navigation origin → mounted xterm and
two animation frames, without requiring PTY output. The original first-terminal
metric still requires a painted PTY glyph. The separate attach browser tests hold
PTY output until after focus/input assertions and verify that a direct session
route starts its stream before initial construction of a 1,000-row rail.

TUI frame time is a user-observable full-frame turnaround, **not isolated Go View
CPU time**. The two TUI cases exercise opening and dismissing a full overlay over
the populated session model. The 5ms driver poll interval bounds observation
resolution in addition to capture/IPC cost. Timeouts only fail a missing event;
they never establish that a frame completed.

## Recorded baseline and budgets

The original P1 recording was measured on 2026-09-05; its layout-shift and TUI
entries remain unchanged. The three web latency baselines were tightened on
2026-09-06 for #3914 using the six after samples detailed below. Bundle baselines
were refreshed on 2026-09-07 for #4050 and on 2026-09-09 for #4018, each using
three container samples. Measurements use Linux amd64, Node/Chromium from the
pinned Playwright 1.56.1 Noble image, Go 1.25.0 and a 4GiB container memory limit.
The original warm end-to-end run took about three minutes.

The committed `scripts/perf/baselines.json` is the budget source. The table below
reports arithmetic mean, range and population standard deviation: six samples
for first terminal, echo and initial rail; three refreshed samples for bundle
bytes; the original three for layout shift and TUI metrics. Each CI run uploads
individual samples, summary JSON/table and any Playwright traces/diff images
under the `perf-baselines` artifact.

| Metric | Mean | Min–max | SD | Budget |
| --- | ---: | ---: | ---: | ---: |
| raw_bytes | 970703.000 | 970703.000–970703.000 | 0.000 | 1019238.150 |
| gzip_bytes | 216250.000 | 216250.000–216250.000 | 0.000 | 227062.500 |
| first_terminal_ms | 2870.417 | 2675.500–3033.000 | 118.562 | 5740.833 |
| echo_ms | 307.550 | 273.300–338.100 | 20.394 | 615.100 |
| rail_ms | 726.383 | 711.500–749.500 | 12.348 | 1452.767 |
| load_shift | 0.000 | 0.000–0.000 | 0.000 | 0.010 |
| snapshot_shift | 0.000 | 0.000–0.000 | 0.000 | 0.010 |
| frame_ms | 480.005 | 375.747–679.179 | 140.888 | 960.011 |
| key_render_ms | 350.640 | 264.172–408.009 | 62.222 | 701.279 |

Budget = baseline mean + margin. For deterministic bundle bytes the margin is
5%, large enough for small features but small enough to catch an unexpected
payload increase. Timing margin is 100% of the baseline, with a 50ms absolute
floor, to tolerate shared-runner scheduling and sub-frame observation noise.
Layout-shift margin is an absolute 0.01 (multiplying a zero baseline would allow
no noise). These are regression budgets, not latency SLOs. P3 tightened only the first-terminal,
echo and initial-rail baselines while preserving this margin policy; #4050 and
#4018 refreshed only the bundle entries under the same 5% policy. CI compares the
three-run mean and fails on missing, negative or non-finite samples, missing
budgets, or a mean above its budget. It never learns a new baseline in CI.

To deliberately rebaseline, run `AF_PERF_RECORD=1 make perf-container`, inspect
`web/test-results/<run>/metrics.json` and `baselines.json`, then copy the latter to
`scripts/perf/baselines.json` and update this table with `metrics.md`. Explain the
reason in the PR. A slower result is evidence to investigate, not an automatic
reason to move a budget.

### Account handoff bundle refresh (#4018)

Recorded with `AF_PERF_RECORD=1 make perf-container` on PR head
`bdcda1c9b95f7687eb09b7f591906ccbf4d227d9`, artifact run `2357344-953435`.
The two bundle rows above come from that run's `metrics.md`; all three samples
were identical. Only those two entries were copied from the generated
`baselines.json`. Every recorded timing and layout-shift mean passed its existing
budget, so their baselines, samples, deviations and margins remain unchanged.

The reviewed PR adds 8,173 raw JavaScript bytes over master; CSS is unchanged and
the service worker changes only its generated cache stamp. That code provides the
same-agent and cross-agent account picker and its eligibility rules, the
version-bound account-handoff call and mixed-version warnings, explicit but
fail-closed recovery for ambiguous mission delivery, and project-menu focus
preservation. The previous bundle baseline also predates 40,662 bytes already
merged to master: master's 962,530-byte bundle passed the old 967,961.4-byte
budget, but left 5,431.4 bytes of headroom. The reviewed account-handoff delta
raised the total to 970,703 bytes, 2,741.6 bytes over that budget. This refresh
records the reviewed feature cost while preserving the 5% regression margin.

### Bundle refresh provenance (#4050)

Recorded with `AF_PERF_RECORD=1 make perf-container` on master commit
`db96729dcaed03285539fcc648c731f845da2bea`, artifact run `1135158-048d87`.
The two bundle rows above come from that run's `metrics.md`; all three samples
were identical. The generated `baselines.json` was copied, then all non-bundle
entries were restored from the committed baseline. Every recorded timing and
layout-shift mean passed its existing budget, so none needed to move.

Raw bytes grew from the original 878,059 to 921,868; gzip bytes grew from
196,682 to 205,077. P3's committed bundle already contained 906,946 raw bytes
while retaining the original bundle baseline. Subsequent merged changes added
14,922 raw bytes: mutation provenance (#3962), task completion controls (#3961),
appearance settings (#3966), phone header/keybar/session layout (#3968, #3980,
#3984), theme/token updates (#3972, #3979), and tab labels (#4011). The old raw
budget left only 93.95 bytes of headroom on the measured master commit.

### P3 recording provenance (#3914)

Four complete `make perf-container` runs were executed serially in B1/A1/B2/A2
order on 2026-09-06, with three fresh browser contexts in each block. B is the
rail-diff parent `d2b2e91e01757663c7d896c92538c78ae40cf259`; A is the attach
implementation `e8464e816faf50aafe4535b0adacca6d699d84d5`. Later rebasing may
change commit IDs; the measured production bundle SHA-256 values are:

- B: `c59d1d0052cf3041da9792b2852f733948dc71946a279785ecfcb009f8591962`.
- A: `cb414f01af64609c3be7d765d33d32a304d37cd842d1ed9e8c01dbce5d05550b`.

Both measurement worktrees used the same instrumentation: the independent #3941
chrome capture-state wait on both sides, plus the terminal-surface probe added
to the before harness as well. These were test-only differences; neither
production bundle was changed. All four runs passed the existing budgets and
unchanged visual goldens. Artifacts retain raw `metrics.json` and `web-runs.json`.

| Block | Artifact run directory | first_terminal_ms mean (min–max) | echo_ms mean (min–max) | rail_ms mean (min–max) |
| --- | --- | ---: | ---: | ---: |
| B1 | `363772-bbcae3` | 3049.167 (2925.600–3189.100) | 364.267 (319.400–404.400) | 754.733 (737.200–779.300) |
| A1 | `1574812-f1ea23` | 2899.733 (2757.600–3033.000) | 291.367 (273.300–308.000) | 726.367 (718.800–731.900) |
| B2 | `2794144-efef7c` | 2989.800 (2890.100–3110.900) | 261.533 (229.100–285.800) | 730.300 (718.700–741.300) |
| A2 | `4018718-1ef677` | 2841.100 (2675.500–2926.100) | 323.733 (314.400–338.100) | 726.400 (711.500–749.500) |
| Before pooled, n=6 | — | 3019.483 (2890.100–3189.100) | 312.900 (229.100–404.400) | 742.517 (718.700–779.300) |
| After pooled, n=6 | — | 2870.417 (2675.500–3033.000) | 307.550 (273.300–338.100) | 726.383 (711.500–749.500) |

The committed three latency entries pool the six individual A samples, not the
rounded block means. Their margins remain 100% of the new baseline (50ms floor),
and every other baseline entry is unchanged. Both after blocks count equally;
the slower echo block is retained. Overlapping before/after echo ranges do not
establish a causal echo improvement from early attach. These are observed
regression thresholds, not a claim that all P3 timing targets were achieved.

The P1 first-terminal scenario still opens the rail and clicks a session.
Direct-route ordering is verified separately by the attach browser test;
`terminal_surface_ms` and `snapshot_rail_ms` remain supplemental observations
and do not replace the three existing budgeted latency metrics.

## Demo stills and intentional redesigns

`playwright.visual.config.ts` drives the **same demo stills** as the
recorder: the ten workflow scenes plus rail disclosures, phone layouts,
terminal actions, tab types, keyboard ownership, split panes, form disclosures,
confirmations, account registration and controlled recovery fixtures, all in light
and dark. It omits video, conversion and video pacing. It waits for
final stand-in output, a stable terminal and all retained seeded rows to report
`Needs you`, even when the rail is hidden. Chrome and split captures require
exactly four seeded rows; login/unavailable scenes have no application rail.
Completed terminal output alone precedes the daemon's idle observation on fast
runners, so #3941 checks these states before each application chrome capture. Goldens are committed under
`web/selftest/goldens`; missing goldens fail normally. Playwright pixel-diffs each
stabilized image, permits **zero differing pixels** above its 0.2 per-pixel color
distance threshold, and uploads actual/expected/diff images on failure.

The visual recorder normalizes the daemon-derived next-run timestamps to
2000-01-03 and the seeded nightly task’s cron to 14:00, so crossing an hour
cannot change the task editor still. The normal demo keeps the real dates and schedule. No timing measurement uses
this fixture. The browser wall clock is fixed at 2000-01-01 so relative pane ages clamp to zero;
its timers still advance. Only nondeterministic regions are suppressed: terminal
cursor and task schedule/next-run metadata (which depends on the daemon's current
clock). The surrounding task rows, names, controls and layout remain checked.
Metadata is hidden with screenshot-only CSS so it stays behind the task-form
modal; a rectangle mask would paint over the new form stills' fields.
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
