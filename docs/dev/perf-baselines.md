# Performance and visual baselines

Issue #3908, part of #3906. Run `make perf-container`. The performance job
runs for PRs touching `web/` (including Playwright configs and goldens), `app/`,
`ui/`, `config/` (the manifest supplies the Config pane's rows, #4362),
`scripts/perf/`, or `scripts/container/`, and is a dependency
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
were refreshed on 2026-09-07 for #4050, on 2026-09-09 for #4018, on
2026-10-01 for #5028, on 2026-10-01 for #5057, and on
2026-11-02 for #6622, each using
three identical deterministic samples. Measurements use Linux amd64, Node/Chromium from the
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
| raw_bytes | 1212950.000 | 1212950.000–1212950.000 | 0.000 | 1281826.800 |
| gzip_bytes | 275801.000 | 275801.000–275801.000 | 0.000 | 291585.800 |
| lazy_raw_bytes | 397628.000 | 397628.000–397628.000 | 0.000 | 417509.400 |
| lazy_gzip_bytes | 129634.000 | 129634.000–129634.000 | 0.000 | 136115.700 |
| codeblocks_raw_bytes | 321641.000 | 321641.000–321641.000 | 0.000 | 337723.050 |
| codeblocks_gzip_bytes | 125499.000 | 125499.000–125499.000 | 0.000 | 131773.950 |
| mermaid_raw_bytes | 5187694.000 | 5187694.000–5187694.000 | 0.000 | 5447078.700 |
| mermaid_gzip_bytes | 1500355.000 | 1500355.000–1500355.000 | 0.000 | 1575983.450 |
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

### Mermaid lazy chunk provenance (#6642)

A ```mermaid fence in a proposal doc renders as a diagram in the Docs view's
live preview. The renderer is mermaid 12.2.0, shipped as `web/dist/lazy/`
mermaid-frame.html — the render frame's own DOCUMENT, the whole library and
its postMessage bootstrap inlined into the page a sandboxed iframe navigates
to (#6669; originally `mermaid.js` plus `mermaid-*` split chunks, one lazy
file per diagram type). The Docs bundle navigates the frame to it only when a
doc holds a ```mermaid block; af-web.js carries none of it, and the frame
itself fetches nothing — the library is inline.

The `mermaid_*` entries measure the whole committed `lazy/mermaid*` set —
the payload the binary embeds — over `web/dist` with `bundleBytes` from
`scripts/perf/check.mjs`, the function `report.mjs` calls; `lazy_*` keeps
measuring only docs-editor.js and its shared chunk. `report.mjs`'s file
filter gained `.html` when the payload became a document (#6669), which also
brings `index.html` into the shell sums on every side of a comparison — the
methodology, not the bytes, moved. Committed values are deterministic, three
identical samples, margin baseline x 0.05, same as the other byte budgets.
Not recorded with `make perf-container`; the figures are byte-exact.

The same refresh moves `raw_bytes` to 1,377,536 (master's committed dist at
`ea1b463514` was already 1,376,749 — the recorded 1,364,595 predated it — and
the new `.cm-md-mermaid` CSS adds 787), `gzip_bytes` to 315,696 (master 315,607
+89), `lazy_raw_bytes` to 378,577 (master 371,254 +7,323 of widget, frame
loader, sanitizer and theme code in the Docs bundle) and `lazy_gzip_bytes` to
123,257 (120,175 +3,082). Every timing and layout-shift baseline is unchanged.

#6669 re-frames the render iframe as a sandboxed opaque origin (sandbox
without allow-same-origin) driven over a string-only postMessage protocol
(`web/src/mermaid_wire.ts`) instead of a same-origin `contentWindow` call —
and, after the security review, as a real navigated document rather than
srcdoc: WebKit gives srcdoc the page's CSP, where 'self' matches nothing in
an opaque origin. The whole library therefore ships INLINE in the frame's own
`lazy/mermaid-frame.html` and the frame fetches nothing: no credential-less
subrequest an authenticating proxy could refuse, no CSP URL-source an IPv6 or
proxied origin would have to spell, and no `Access-Control-Allow-Origin`
grant anywhere (the chunk CORS the first cut added is gone).

Against master's committed dist measured with the same `.html`-inclusive
filter: the `mermaid.js`/`mermaid-*` set (5,187,694 raw / 1,507,069 gzip) is
replaced by the single frame document (5,256,397 / 1,484,809) — +68,703 raw /
**−22,260 gzip**, a single-file bundle being smaller compressed than the split
set's per-chunk framing. (The script is authorized by sha256 now rather than a
stamped nonce, so the served bytes are static and the document's ETag survives
a daemon restart; `lazy/mermaid-frame.sha256`, the sidecar the CSP names, is
44 bytes and outside the `.js|.css|.html` filter.) `lazy_*` moves to 399,058 /
129,835 (+651 / +149 of epoch-guarded frame loading and the wire-source
gates) and the shell entries are byte-identical (+0 / +0 — `index.html` counts
on both sides). Every entry stays inside its existing budget —
`mermaid_gzip_bytes` lands 5% under — so no baseline moves. Every timing and
layout-shift baseline is unchanged.

**The wire cost the byte table does not show (#6669 re-review).** These
entries measure embedded bytes, not fetches-per-open — and the two diverge.
Under master's split chunks a first flowchart fetched ~29 files, 867 KB raw /
239 KB gzip. Under this design the frame's first open fetches the whole
document — 5.26 MB raw / 1.48 MB gzip — and on loopback first render is
~1.1 s in Chromium and ~3.5 s in WebKit (independent review's figures,
~0.30 s/~0.36 s on master): the price of the one-navigation property, since
any subresource the frame fetched would arrive at an authenticating proxy
with no credentials to answer. That is a first-open cost, paid once: the
document now carries `ETag` + `Cache-Control: no-cache`, so every later open
revalidates — measured on the wire as a 304 with a zero-length body in both
engines — and a frame re-open happens only when the renderer is torn down
(a self-navigation, a channel failure or a hung render), never on an ordinary
render or theme flip. On a 400 kbps link the 1.48 MB download is ~30 s, which
is why the open timeout is 60 s rather than the document's own 30 s.

### Code-fence grammar lazy chunk provenance (#6731)

A ```lang fence in a proposal doc renders as a finished code block in the Docs
view's live preview, syntax-highlighted when its language is known. The
grammars — a curated set of CodeMirror 6 languages and legacy stream modes —
ship as `web/dist/lazy/codeblocks.js`, fetched by the Docs bundle only when a
previewed doc holds a fenced block whose info string names a language. The
shell carries none of it, and a doc with no language-named fence never pays for
it.

`codeblocks.js` is not a standalone build: the Docs bundle and the grammar
entry are built together with esbuild code splitting, so the CodeMirror core
both need (`@codemirror/language` pulls in `view`/`state`) lands in a shared
`lazy/chunk-*.js` instead of being duplicated into the grammar file. The
`codeblocks_*` entries therefore measure `lazy/codeblocks.js` alone — the bytes
a doc with a code fence newly pays, the shared core already having arrived with
the Docs bundle — while `lazy_*` now measures `docs-editor.js` plus the shared
chunk: 397,628 raw / 129,634 gzip, up from 378,577 / 122,689. The shell
entries move within their existing budgets (+2,635 raw / +805 gzip of widget
and token CSS), so they are not rebaselined. Committed values are
deterministic, three identical samples, margin baseline x 0.05, same as the
other byte budgets, computed with `bundleBytes` from `scripts/perf/check.mjs`
over the PR's rebuilt `web/dist`. Not recorded with `make perf-container`; the
figures are byte-exact.

### Lazy bundle provenance (proposal Docs tab)

The Docs tab (proposal mode, step 2) ships its CodeMirror editor as
`web/dist/lazy/docs-editor.js`, a separate esbuild build that `af-web.js` imports
only when a Docs tab first opens. The shell metrics exclude `lazy/`, and the two
`lazy_*` entries measure it on its own, with the same 5% margin. Both lazy values
were computed with `bundleBytes` from `scripts/perf/check.mjs`, the function that
`report.mjs` calls, over the committed `web/dist`. Bundle bytes are deterministic,
so the three samples are identical. The same PR adds the Docs tab's loader, shell wiring and CSS to the shell, which then measures 1,149,376 raw and 260,459 gzip bytes over `web/dist` without `lazy/`. Both are inside the existing shell budgets (raw 1,182,374; gzip 268,549), so those baselines are not moved. Lazy bundle: 371,003 raw / 120,125 gzip.

### Bundle refresh provenance (#6662)

Master's committed bundle at `8bba37762d` is 1,364,595 raw / 312,686 gzip over `web/dist/**.{js,css}` (gzip level 9). The Backlog view removal (Refs #6662, branch `siyer/remove-backlog-web`: the view's model, loader, pane, cards, panel, inbox, effort and usage modules plus their CSS, the item-session chip on rail rows, the Backlog tab and its needs-you badge, and the project-switcher needs-you badges) takes it to 1,211,915 raw / 276,119 gzip, −152,680 raw / −36,567 gzip. The lazy Docs editor is unchanged (371,254 / 120,175 on this tree). Comments do not ship (esbuild strips them), so the shrink is removed feature code. Both shell entries are moved down to the deterministic byte sum from `bundleBytes` in `scripts/perf/check.mjs` (the function `report.mjs` calls) over the PR's rebuilt `web/dist`, three identical samples, margin baseline x 0.05. Every timing and layout-shift baseline is unchanged. Not recorded with `make perf-container`; the figure is byte-exact.

### Bundle refresh provenance (#6622)

Master's committed bundle at `d3e02d1f8c` is 1,339,655 raw / 307,228 gzip over `web/dist/**.{js,css}` (gzip level 9). The web sandbox-hosts management port (Refs #5571, branch `siyer/surface-parity-gap-68`: the Hosts Config section — list, add, remove, make-default, check — plus the connection-lifecycle fencing from its Codex review and their CSS) takes it to 1,364,595 raw / 312,686 gzip, +24,940 raw / +5,458 gzip. The lazy Docs editor is unchanged (371,254 raw on both trees; its gzip sum moves 2 bytes, 120,177 → 120,175, inside the existing budget, so those entries are not moved). Comments do not ship (esbuild strips them), so the growth is feature code. Both shell entries are moved. Each is the deterministic byte sum from `bundleBytes` in `scripts/perf/check.mjs` (the function `report.mjs` calls) over the PR's rebuilt `web/dist`, three identical samples, margin baseline x 0.05. Every timing and layout-shift baseline is unchanged. Not recorded with `make perf-container`; the figure is byte-exact.

### Bundle refresh provenance (#6582)

Master's committed bundle at `250fd7d06d` is 1,323,696 raw / 303,520 gzip over `web/dist/**.{js,css}` (gzip level 9). That is already within 2,259 gzip bytes of the 305,778.9 budget the #6298 refresh set, after the Backlog slices merged since. The effort cards port (Refs #6284, fork `f733c567f0`: group cards, progress and meta lines, New group and Add item, and the sticky item panel, with their CSS, plus the one-pass card counts and the panel reveal from its review) takes it to 1,337,886 raw / 306,713 gzip, +14,190 raw / +3,193 gzip. The lazy Docs editor is unchanged (371,254 / 120,177 on both trees). Moving the Backlog view into the lazy chunk would not fit: that chunk has about 6 KB of gzip headroom, and the view is several times that. Comments do not ship (esbuild strips them), so the growth is feature code. Both shell entries are moved. Each is the deterministic byte sum from `bundleBytes` in `scripts/perf/check.mjs` (the function `report.mjs` calls) over the PR's rebuilt `web/dist`, three identical samples, margin baseline x 0.05. Every timing and layout-shift baseline is unchanged. Not recorded with `make perf-container`; the figure is byte-exact.

### Bundle refresh provenance (#6298)

Master's committed bundle (measured at the last merge, after #6297 and #6462/#6464) is 1,245,090 raw / 284,004 gzip over `web/dist/**.{js,css}` (gzip level 9). The Backlog actions port (Refs #6284, fork `0bdb649b62`: runner bar, Start now, answer box, usage gates, settings) takes it to 1,275,573 raw / 291,218 gzip, +30,483 raw / +7,214 gzip; the lazy Docs editor is unchanged (371,003 / 120,125). Comments do not ship (esbuild strips them), so the growth is feature code. Both shell entries are moved; each is the deterministic byte sum from `bundleBytes` in `scripts/perf/check.mjs` (the function `report.mjs` calls) over a clean `npm ci` build, three identical samples, margin baseline x 0.05, and every timing and layout-shift baseline is unchanged. Not recorded with `make perf-container`; the figure is byte-exact.

### Bundle refresh provenance (#6297)

Master's committed bundle is 1,126,071 raw / 255,761 gzip over `web/dist/**.{js,css}` (gzip level 9) at the last merge of master. The Backlog view port (Refs #6284, fork `d7a0be4427`) takes the bundle to 1,243,724 raw / 283,682 gzip over master's 1,126,071 / 255,761 (#6289), +117,653 raw / +27,921 gzip (the shell metrics; the lazy Docs editor is unchanged by this PR: 371,003 / 120,125). The growth is feature code (the view, its model, loader and panel, about 3,000 lines of TypeScript and 900 lines of CSS) plus the web changes carried in by merging the daemon stack branch (#6295) and master, which carry the attention-dot rail work (#5581). Comments do not ship (esbuild strips them). Both entries are moved; each is the deterministic byte sum, three identical samples, margin baseline x 0.05, and every timing and layout-shift baseline is unchanged. Computed from the committed tree rather than recorded with `make perf-container`; the figure is byte-exact.

### Bundle refresh provenance (#6289)

Master's committed bundle (1,123,102 raw / 254,923 gzip over `web/dist/**.{js,css}`, gzip level 9, after #6290) is where this branch started from; #6289 (the task switch pending state) takes it to 1,126,071 raw / 255,761 gzip, +2,969 raw / +838 gzip. Comments do not ship (esbuild strips them), so the growth is the flip state machine's code. Both entries are moved to the deterministic byte sums, three identical samples, margin baseline x 0.05; every timing and layout-shift baseline is unchanged. Computed from a clean `npm ci` build of the merged tree rather than recorded with `make perf-container`, which is not a routine local run; the figure is byte-exact.

### Bundle refresh provenance (#6290)

Master's committed bundle (1,120,104 raw / 254,141 gzip over `web/dist/**.{js,css}`, gzip level 9) sat 319 bytes under the 1,120,423.5 raw budget, so any web change tripped it. #6290 (terminal links without confirm() and project switcher rows as links) takes the bundle to 1,123,037 raw, +2,933 raw over master; comments do not ship (esbuild strips them), so the growth is feature code. Only the raw entry is moved (gzip is unchanged at 253,171 and its budget still holds); it is the deterministic byte sum, three identical samples, margin baseline x 0.05, and every timing and layout-shift baseline is unchanged. Computed from the committed tree rather than recorded with `make perf-container`, which is not a routine local run; the figure is byte-exact and matches what the Performance job reported.

### Bundle refresh provenance (#6212)

Master's committed bundle (1,113,611 raw / 252,020 gzip over `web/dist/**.{js,css}`, gzip level 9) sat 431 bytes under the 252,451.5 gzip budget, so #6212 (one fenced model for the loaded archive window, #6206-#6209) could not land without moving it. After merging master (including #6216's resize fix) and the review fixes, the bundle is 1,117,448 raw / 253,171 gzip, +3,837 raw / +1,151 gzip over master. Comments do not ship (esbuild strips them from the unminified bundle), so the growth is the feature's code: the archive-window reconcile (`archive_window.ts`), the fenced `adopt`, and the dirty-epoch wiring in `index.ts`. Only the gzip entry is moved; raw is left alone. The gzip entry is the deterministic sum, three identical samples, margin baseline x 0.05; every timing and layout-shift baseline is unchanged. This was computed from the committed tree rather than recorded with `make perf-container`, which is not a routine local run; the figure is byte-exact.

### Bundle refresh provenance (#5681)

Master's bundle crossed both budgets on 2026-10-27 (run 40604391077: raw 1,074,498 > 1,074,413.55, gzip 242,312 > 239,969.1). The #5057 baseline (1,023,251 / 228,542) was already 32,832 raw / 9,051 gzip bytes stale when it merged, because the committed bundle was 1,056,083 / 237,593 at that commit, so the budget left almost no headroom. Slim CI (#5615) made this job master-only, so no PR saw the crossing. Committed `web/dist` growth since then, summed per merge (gzip level 9 over `web/dist/**.{js,css}`):

| Merge | Raw | Gzip |
| --- | ---: | ---: |
| #5562 config text-field focus | +545 | +84 |
| #5590 per-write config warnings | +76 | +12 |
| #4553 tab insert indicator off the bar | +259 | +99 |
| #5535 shared-modal focus containment | +597 | +151 |
| #4806 config provenance (TUI and web) | +8,224 | +1,981 |
| #5577 missing callback degrades create | +836 | +193 |
| #5058 split-pane close button Enter | +83 | +23 |
| #5413 skipped repos on Snapshot | +6,446 | +1,955 |
| #5576 unreachable state for remote sessions | +1,349 | +220 |
| Total over #5057 | +18,415 | +4,719 |

All of it is feature code (gzip ratio about 26%, in line with the bundle's ~22%, so no incompressible bloat). Both bundle entries were re-recorded with `AF_PERF_RECORD=1 make perf-container` (three identical samples) and copied alone; every timing and layout-shift baseline is unchanged: raw_bytes 1,023,251 → 1,074,498, gzip_bytes 228,542 → 242,312.

### Bundle refresh provenance (#5057)

PR #5057 adds `clearLoginDraft` (a one-line `ui.ts` helper plus a single gated
call site in `disconnect`) so an auth-driven return to the login view does not
pre-fill the paste form with a credential the daemon just rejected. The
`clearLoginDraft` change grew the gzip sum 32 bytes over the post-#5028 tree
(gzip_bytes 227,625 → 227,657). The PR was then merged with master, pulling in
the tab-drag release guard (#5037/#5053), whose bundled changes raised the
committed artifact to 1,023,251 raw / 228,542 gzip bytes — past the gzip value
recorded by the gzip-only refresh. Both bundle entries were therefore
re-recorded to the final committed tree using the same convention every entry
follows (baseline = recorded mean, margin = baseline × 0.05). Bundle bytes are
deterministic — `make web-build` reproduces `web/dist` byte-for-byte, which the
Web job verifies on every push — and the raw and gzip sums recompute identically
across three samples, so the recorded means equal the per-file `gzipSync(level: 9)`
sum over `web/dist/**.{js,css}` (`scripts/perf/report.mjs`):
raw_bytes 1,020,133 → 1,023,251, gzip_bytes 227,657 → 228,542. Every timing
and layout-shift baseline is unchanged.

### Bundle refresh provenance (#5028)

Measured on the PR's final tree by recomputing the identical metric locally
(`scripts/perf/report.mjs`: raw sum and per-file `gzipSync(level: 9)` over
`web/dist/**.{js,css}`) — raw_bytes 1,020,133, gzip_bytes 227,625. Bundle bytes
are deterministic — `make web-build` reproduces `web/dist` byte-for-byte, a
property the Web job verifies on every push — and the first intermediate
refresh (1,019,608 / 227,456) matched the "Performance and visual baselines"
job's measurement on run `36818955890` exactly, with the intermediate values
(1,019,736 / 227,483) corroborated by Codex review recomputing the same metric.
Only the two bundle entries were updated; every timing and layout-shift
baseline is unchanged.

The reviewed PR adds 1,635 raw JavaScript bytes over master for the
coast-applied/stop-count/live-stop ledger the #5020 selftest asserts against;
CSS is unchanged and the service worker changes only its generated cache
stamp. The previous bundle baseline also predates 47,795 raw bytes merged to
master since the #4018 refresh: master's 1,018,498-byte bundle passed the old
1,019,238.15 budget with only 740 bytes of headroom, so the ledger could not
fit under the old margin. This refresh records the reviewed instrumentation
cost while preserving the 5% regression margin.

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
terminal actions, keyboard ownership, split panes, form disclosures,
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
The Config view's two manifest-size numbers (the header count and the folded
advanced tier's "Show N advanced settings") are stated as a literal `N` for the
instant of each capture, so adding a config key no longer invalidates every
config golden. Their boxes and layout are still captured; the DOM text is
restored after the shot (`withManifestSizeFrozen`, web-demo.spec.ts).
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
to the artifact mount, never to the read-only checkout. It rewrites only the
goldens the gate rejects, using the gate's own comparator and threshold. Every
other candidate keeps its committed bytes, so `git status` after the copy lists
exactly the images your change moved. Update mode never rewrites an image on a
byte difference alone: the capture is not byte-reproducible below the
threshold. Two captures of one tree differed in 13 of 122 goldens, and every one
of them passed the gate (#4557). An image that passes the gate is already
correct, so a new copy of it is noise, not a change. `CI` forbids both golden
updates and baseline recording. `make demo-assets` remains the paced documentation
video recorder; it does not silently overwrite the regression goldens.
