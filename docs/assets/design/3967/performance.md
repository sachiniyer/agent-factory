# Unchanged performance budgets

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
