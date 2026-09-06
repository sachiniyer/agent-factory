# Focused-session phone header · #3967

The six `before-phone-session-*` captures were recorded before implementation,
from master `3905ab48104334fce8cf126e4fdb43641ec8ad8f`, using the existing container
recorder/selftest (`make perf-container`). The only recorder change added the
360, 390 and 430px screenshots; the UI and bundle were unchanged. Both themes
use 812px height. The baseline visual suite and all performance budgets passed.

Before: the session title and project/view navigation disappear, while drawer,
tabs, PR link, Keyboard and Actions compete for one row.

After: project/view navigation remains above the pane; title, static Keyboard
and Actions have one row, with a horizontally scrolling tab row beneath. The
same session, viewport heights and themes are used. The title retains its full
text and tooltip while CSS truncates it with … when needed. Targets are 44px.

These are the real web client and an isolated daemon with the recorder's seeded
stand-in agent transcript, not production work or a layout mockup. The recorder
asserts header visibility, keyboard activation/Tab/Escape, title truncation,
separate rows, scrolling tabs, target heights and viewport fit at each width.

`unit-before.txt` records all three new unit tests failing before implementation.
The resize test also caught desktop Escape focusing the hidden phone trigger;
that regression was fixed before submission.

Current master already had `phone-session` and `phone-session-dark` goldens.
This PR refreshes and strengthens those existing cases, using the unchanged
`maxDiffPixels: 0` oracle. Performance budgets are unchanged.

## Unchanged performance budgets

Final `make perf-container` run with golden updates disabled. All visual checks
and all budgets passed. Three container samples per timing metric.

| Metric | Mean | Min–max | SD | Budget |
| --- | ---: | ---: | ---: | ---: |
| raw_bytes | 907937.000 | 907937.000–907937.000 | 0.000 | 921961.950 |
| gzip_bytes | 202029.000 | 202029.000–202029.000 | 0.000 | 206516.100 |
| first_terminal_ms | 2859.333 | 2789.200–2925.400 | 55.678 | 5740.833 |
| echo_ms | 322.933 | 307.000–341.100 | 14.011 | 615.100 |
| rail_ms | 714.200 | 683.600–737.200 | 22.532 | 1452.767 |
| load_shift | 0.000 | 0.000–0.000 | 0.000 | 0.010 |
| snapshot_shift | 0.000 | 0.000–0.000 | 0.000 | 0.010 |
| frame_ms | 74.143 | 66.528–80.893 | 5.896 | 960.011 |
| key_render_ms | 75.017 | 64.973–85.699 | 8.473 | 701.279 |
