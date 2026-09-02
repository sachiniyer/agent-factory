# Daemon memory — sizing, and why `MemoryPeak` is not it

The Agent Factory daemon is a long-lived process, so sooner or later someone
looks at what it costs. On Linux the obvious place to look is the systemd unit,
which volunteers the answer in the journal every time it stops:

```
agent-factory-daemon.service: Consumed 14h 7min CPU time, 17.4G memory peak.
```

`systemctl --user show agent-factory-daemon.service -p MemoryPeak` says the same
thing, and across thirteen unit lifetimes on the box below it read as high as
**38.3 GB**. Those numbers are real. Almost none of it is the daemon — over the
same period the daemon process never held more than **92.6 MB**.

This page gives the sizing figures for the daemon process, the commands that
measure the process rather than its cgroup, and what actually drives the cgroup
number so you can predict it.

Every figure here was measured on **one machine on 2026-09-02** — a 16-core /
125 GB Linux box running 20 live sessions, 720 session records, and 20 tasks,
with the daemon supervised by a systemd user unit. They are single-box
measurements, not a benchmark across hardware. Treat them as the shape of the
thing and re-measure on yours with the commands below. The full investigation,
with the raw tables, is
[#3625](https://github.com/sachiniyer/agent-factory/issues/3625).

## The short answer

- The daemon process holds **30–90 MB** resident, and its high-water mark on
  that box was **92,600 kB** — identical in all 60 samples over a 20-minute
  window.
- Go heap in-use: **2.5 MB at 0 sessions · 4.6 MB at 20 live sessions**. 24 OS
  threads, constant.
- Sessions are close to free *to the daemon*: about **0.1 MB of heap and 0.3
  goroutines each**. There is no per-session goroutine.
- **Size for ~128 MB.** That is comfortable for the daemon itself at the load
  measured, and it is the number to use on a small VPS.
- The unit's `MemoryPeak` and `MemoryCurrent` are **cgroup** figures. They read
  in the gigabytes on a busy box, and they are not daemon memory.

That 128 MB sizes the daemon. It does not size what the daemon *launches* on
your behalf — see [what drives the cgroup number](#what-actually-drives-the-cgroup-number).

## What the daemon itself uses

Live daemon, 20 live sessions · 720 session records · 20 tasks, sampled 60 times
over 20 minutes:

| measure | value | note |
|---|---|---|
| `VmRSS` | 30–90 MB | oscillates with the GC cycle |
| `VmHWM` | 92,600 kB | identical in every one of the 60 samples |
| Go heap in-use | 2.5 MB (0 sessions) · 4.6 MB (20 sessions) | |
| OS threads | 24 | constant across the window |
| goroutines | 14 (0 sessions) · 20 (20 sessions) | no per-session goroutine |

The marginal cost of one live session to the daemon is therefore about **0.1 MB
of heap and 0.3 of a goroutine**. That is a slope between the two measured
endpoints (0 and 20 sessions), not a fitted curve — the shape between them was
not sampled, and past 20 live sessions is untested.

Record count did not move anything over the range measured: the same daemon held
720 session records and 20 tasks throughout.

### Allocation rate is not footprint

The daemon allocates far more than it holds: about **876 MB cumulative in ten
minutes at 20 live sessions**, roughly 5 GB/h — and essentially all of it is
collected, which is why the in-use figures above stay in single-digit megabytes.
Pane capture and JSON encoding dominate it. A high allocation rate is a GC and
CPU cost, not a memory footprint, and it is not evidence of a leak. (This figure
alone comes from the sandbox daemon's heap profile — see
[profiling](#profiling-the-daemon) — not from the live daemon.)

One line of it is worth fixing: re-reading, migrating and re-marshalling the
state file on essentially every access accounts for about 23% of that
cumulative allocation, tracked in
[#3652](https://github.com/sachiniyer/agent-factory/issues/3652).

## How memory scales with sessions

| live sessions | daemon RSS | how to read it |
|---|---|---|
| 0 | ~55 MB | idle daemon; includes the mapped binary text |
| 20 | ~30–60 MB | live daemon, across a 20-minute sampling window |

RSS is not monotonic in session count, and cannot be. It rises and falls with
the Go GC cycle, and a large share of it is the mapped binary rather than
anything the session load touched — which is why the idle figure can read
*higher* than a sample of the busy one.

The stable figures are the two that did not move: `VmHWM` (92,600 kB, unchanged
across the whole window) and Go heap in-use (2.5 → 4.6 MB). Both say the same
thing — 20 sessions cost the daemon a couple of megabytes. Quote `VmHWM`, not a
single RSS reading.

## The trap: `MemoryPeak` is a cgroup figure

`systemctl --user show agent-factory-daemon.service -p MemoryPeak`, and the
`Consumed …, N memory peak` line systemd writes to the journal on every stop, do
**not** report the daemon's memory. They report the high-water mark of the
*unit's cgroup*, which covers:

- the daemon process;
- every other process in that cgroup — everything the daemon spawns, including
  anything that outlives a stop under `KillMode=process`;
- **page cache** for every file anything in the cgroup read or wrote;
- kernel slab.

On the measured box, `MemoryPeak` ran **5.9–38.3 GB per unit lifetime** while
the daemon process itself never exceeded **92.6 MB**. Against the 7.87 GB peak
of the lifetime being sampled, the daemon accounted for **1.2%** of it, and a
smaller fraction still of the 38.3 GB maximum.

The clearest demonstration is that the cgroup number moves on its own. Over one
20-minute window — no restart, no config change, and no change at all in the
daemon's own memory:

| cgroup measure | start | after 20 min |
|---|---|---|
| `MemoryCurrent` | 2.16 GB | 1.20 GB (−44%) |
| `anon` | 25–50 MB | same range — no trend |
| `file` (page cache) | 1,832 MB | 942 MB |
| `slab` | 431 MB | 367 MB |

The daemon freed nothing. The kernel reclaimed page cache, which is reclaimable
by definition. A cgroup peak that is mostly `file` is not memory pressure, and
it is not a leak.

### Commands that do answer "how much memory is the daemon using"

```bash
# The daemon process: current resident set, and its own high-water mark.
grep -E 'VmRSS|VmHWM' \
  /proc/$(systemctl --user show agent-factory-daemon.service -p MainPID --value)/status

# Split the cgroup number into its parts. anon is memory actually held, file is
# reclaimable page cache, slab is kernel structures.
grep -E '^(anon|file|slab) ' \
  "/sys/fs/cgroup$(systemctl --user show agent-factory-daemon.service -p ControlGroup --value)/memory.stat"

# For contrast — the high-water of all of the above, over every process that has
# ever lived in the cgroup. This is the number that is not daemon memory.
systemctl --user show agent-factory-daemon.service -p MemoryPeak -p MemoryCurrent
```

`VmHWM` is the figure to quote: it is the process's own peak, it does not fall
back as the GC returns pages, and on the measured box it did not move at all.
`MemoryPeak` is exported only by newer systemd; the journal's `N memory peak`
line on stop is the same figure by another route.

All three commands are read-only — they signal nothing and restart nothing.
`af daemon status` remains the supported way to ask about the daemon's health;
these are for the memory question specifically.

The trap is Linux-specific. On macOS the daemon runs under launchd, which has no
cgroup accounting, so there is no equivalent unit-level number to misread — but
the daemon figures above were measured on Linux, and macOS was not sampled.

## What actually drives the cgroup number

Two things, neither of them the daemon's own memory, and both charged to the
unit.

**Child processes, at a high rate.** Sampling the unit's `cgroup.procs` at full
speed for 75 seconds found **1,243 distinct child processes — about 16.6 per
second, on the order of 1.4M per day**. Most are `tmux capture-pane`, one per
session per poll, plus `git rev-parse`. Each is tiny and short-lived, but the
page cache they touch is charged to the unit and stays charged until the kernel
reclaims it. That is what fills the `file` line in the table above.

**`post_worktree_commands`, which run inside the daemon's cgroup today** — the
current behaviour, and a bug being fixed under
[#3650](https://github.com/sachiniyer/agent-factory/issues/3650) rather than a
permanent property. When
the daemon creates a worktree it runs your repo's post-worktree hook as its own
child, so whatever that hook builds is accounted to the daemon unit — its anon
memory *and* its page cache. On the measured box, one repo's hook runs
`make dev_install`, building a ~2 GB `node_modules` per worktree. Across 13 unit
lifetimes:

| correlated with `MemoryPeak` | Pearson r |
|---|---:|
| sessions on the repo with the heavy hook | **+0.950** |
| unit uptime | +0.229 |
| sessions on repos with no such hook | −0.258 |

The fit on those lifetimes was `MemoryPeak ≈ 6.3 GB + 0.65 GB` per session on
the hook-carrying repo. So the unit's peak is predictable — but it predicts from
the hook, not from how long the daemon ran or how many sessions it hosted. One
lifetime with 42 sessions on hook-less repos peaked at 7.8 GB; one with 48
sessions on the hook-carrying repo peaked at 38.3 GB.

That is a description of one box over 13 lifetimes, not a formula for yours. The
transferable part is the mechanism: heavy post-worktree hooks land on the daemon
unit.

#3650 moves those spawns into their own transient scope — a sibling under
`app.slice` rather than a child of the daemon unit, so the build is off the
daemon's books and still survives a daemon restart. The watcher and the VS Code
start gate already route through a scope (#2299); the post-worktree hook and the
archive hook do not yet. Until that lands, size the **box** for what your hooks
build — not the daemon.

## Sizing a machine

- **The daemon: ~128 MB**, at the load measured (20 live sessions · 720 records
  · 20 tasks). Untested above that.
- **Plus the agents.** Every session runs a real agent process under a real tmux
  server, and that is where session memory actually goes. It is outside the
  daemon's footprint and outside this page; no figure for it was measured here.
- **Plus whatever `post_worktree_commands` builds**, per worktree — charged to
  the daemon's cgroup until #3650 moves it out.

If a box is under memory pressure, do not start from the unit's `MemoryPeak`.
Read `VmHWM` for the daemon process, then read `memory.stat` to see how much of
the cgroup figure is reclaimable page cache. On the box above, that pair turned
a 38 GB "daemon leak" into a 92.6 MB working set and a hook that builds
`node_modules` in the wrong cgroup.

## Profiling the daemon

**af exposes no pprof or expvar endpoint today**, and no `runtime.ReadMemStats`
output. There is nothing to curl, and nothing at `/debug/pprof` — if you go
looking for one, you will not find it. There are two routes, and only the first
of them exists.

### Route 1 — a build-tag probe against a sandbox daemon

This is what #3625 actually used, and it is the route available today:

1. Add a scratch file guarded by a build tag — `//go:build afpprof` — that
   starts `net/http/pprof` on a loopback listener when an env var is set.
   `net/http/pprof` is stdlib, so no dependency is added.
2. Build `af` with `-tags afpprof`.
3. Run that binary as a **sandbox daemon** under a throwaway
   `AGENT_FACTORY_HOME` and `TMUX_TMPDIR`, with its own sessions.
4. Profile the sandbox with `go tool pprof`, then delete the scratch file. It is
   scratch — it belongs in no commit.

Profile the sandbox, never the live daemon. The live daemon owns real state and
real sessions; answering a memory question is not worth rebuilding, restarting,
or signalling it. And it is usually unnecessary — `VmHWM` plus `memory.stat`
settled #3625 on their own, and the heap profile only confirmed what they
already said. Reach for a profile when you need to name a *retainer*, not to
answer "how big is it".

### Route 2 — a control-socket endpoint, filed and unbuilt

A pprof endpoint behind the existing control socket — `GET
/v1/debug/pprof/{profile}`, off by default, enabled by config key or env var,
served only on the unix control socket and **never** bound to a network
listener, stdlib only — is filed as
[#3651](https://github.com/sachiniyer/agent-factory/issues/3651) and
**deliberately left unbuilt**: queued, not urgent. It is in no release, it
answers no request today, and nothing in af will grow it by accident. Do not go
looking for it, and do not write a runbook that assumes it.

It is recorded here so the next investigation knows both routes exist: one you
can use now at the cost of a rebuild and a sandbox, and one that would remove
that cost if it is ever worth building.

## Where these numbers come from

All of it is one investigation on one machine, recorded in
[#3625](https://github.com/sachiniyer/agent-factory/issues/3625): a 16-core /
125 GB Linux box on 2026-09-02, fleet of 20 live sessions, 720 session records,
20 tasks, daemon under a systemd user unit, sampled 60 times across a 20-minute
window, plus 13 historical unit lifetimes read out of the journal and a sandbox
daemon for the heap profiles.

Nothing here is extrapolated to other hardware, other session counts, or other
operating systems, and nothing was measured above 20 live sessions. Where a
figure is a slope between two points or a fit across lifetimes, it says so.
