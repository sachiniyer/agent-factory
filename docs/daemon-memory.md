# Daemon memory — sizing, and why `MemoryPeak` is not it

The Agent Factory daemon is a long-lived process, so sooner or later someone
looks at what it costs. On Linux the obvious place to look is the systemd unit,
which volunteers the answer in the journal every time it stops:

```
agent-factory-daemon.service: Consumed 14h 7min CPU time, 17.4G memory peak.
```

`systemctl --user show agent-factory-daemon.service -p MemoryPeak` says the same
thing, and across thirteen unit lifetimes on the box below it read as high as
**38.3 GB**. Those numbers are real, and they are not the daemon. In the one
lifetime where the daemon process itself was measured, it peaked at **92.6 MB**
while its unit peaked at 7.87 GB.

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

- The daemon process holds **30–90 MB** resident across the two daemons measured
  (a live one and an instrumented sandbox one — see the tables). The live
  daemon's `VmHWM` was **92,600 kB**, identical in all 60 samples over a
  20-minute window: a ceiling for that process, not a session delta.
- Go heap in-use: **2.5 MB at 0 sessions · 4.6 MB at 20 live sessions**, on the
  sandbox daemon — the only one that can be profiled, and instrumented to make
  that possible. 24 OS threads, constant.
- Sessions are close to free *to the daemon*: about **0.1 MB of heap and 0.3
  goroutines each**, from that sandbox 0 → 20 comparison. There is no
  per-session goroutine.
- **Size for ~128 MB.** That is comfortable for the daemon itself at the load
  measured, and it is the number to use on a small VPS.
- The unit's `MemoryPeak` and `MemoryCurrent` are **cgroup** figures. They read
  in the gigabytes on a busy box, and they are not daemon memory.

That 128 MB sizes the daemon. It does not size what the daemon *launches* on
your behalf — see [what drives the cgroup number](#what-actually-drives-the-cgroup-number).

## What the daemon itself uses

Two daemons were measured, and they are not the same binary. The **live** daemon
is the release build: it carried 20 live sessions · 720 session records · 20
tasks and was sampled 60 times over 20 minutes through `/proc`. The **sandbox**
daemon is the same source revision **built with profiling instrumentation**
(`-tags afpprof`, plus an HTTP pprof listener — see
[profiling](#profiling-the-daemon)), run under a throwaway home with its own
sessions. It is the only one that could be profiled at all:

| measure | value | source |
|---|---|---|
| `VmRSS` | 45–60 MB | live daemon; oscillates with the GC cycle |
| `VmHWM` | 92,600 kB | live daemon, identical in every one of the 60 samples |
| OS threads | 24 | live daemon, constant across the window |
| Go heap in-use | 2.5 MB (0 sessions) · 4.6 MB (20 sessions) | sandbox daemon |
| goroutines | 14 (0 sessions) · 20 (20 sessions) | sandbox daemon |

**The probe is inside the sandbox numbers.** `net/http.ListenAndServe` alone
accounts for 512 kB of that 4.6 MB profile — about 11% — and the listener's
goroutines are in the goroutine counts. So the heap and goroutine figures
describe an instrumented build, not a release daemon; what survives that is the
**0 → 20 delta**, measured on one binary carrying one probe at both endpoints.
Lean on the delta, not on either absolute value. The `/proc` rows are the live,
uninstrumented daemon. Keep the two apart when quoting them.

The marginal cost of one **local, tmux-backed** session to the daemon is
therefore about **0.1 MB of heap and 0.3 of a goroutine**. That is a slope
between the sandbox daemon's two measured endpoints (0 and 20 sessions), not a
fitted curve — the shape between them was not sampled, and past 20 live sessions
is untested.

The backend matters, and only one was measured. Every session in both
measurements ran locally under tmux. A session on a remote-agent backend
(docker, ssh, sandbox) is polled through a different path and keeps a
daemon-side `remoteAgentClient` of its own — an HTTP transport, plus a
WebSocket when its stream is open (`session/agentserver_remote.go`) — which a
local session does not have. None were in the fleet measured, so **do not carry
this slope onto a remote fleet**; re-measure with the commands below instead.

The footprint above was observed while the daemon held **720 session records and
20 tasks**. Both counts were constant for the whole window, so that is the
record count the figures describe — not evidence that record count costs
nothing. No other record count was sampled.

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

| live sessions | daemon RSS | which daemon |
|---|---|---|
| 0 | ~55 MB | **sandbox** daemon (instrumented), throwaway home, idle |
| 20 | ~30 MB | **sandbox** daemon (instrumented), same binary, 20 sessions |
| 20 | 45–60 MB | **live** daemon (release build), sampled 60× over 20 minutes |

Read the rows as two separate daemons, not one curve. The live daemon was never
observed at 0 sessions, so there is no measured figure for an idle live daemon —
do not read the first row as one.

RSS is also not monotonic in session count, and cannot be. It rises and falls
with the Go GC cycle, and a large share of it is the mapped binary rather than
anything the session load touched — which is why the same sandbox binary reads
*higher* idle than it does under twenty sessions.

Two stable figures, and they answer different questions. **Session scaling rests
on the sandbox comparison alone** — heap in-use 2.5 → 4.6 MB across 0 and 20
sessions — because it is the only measurement with both endpoints. The live
daemon's `VmHWM` of 92,600 kB is an **upper bound on that process**: it had
already reached that mark before the sampling window opened and never moved
again, which shows the process was stable at 20 sessions, not what those 20
sessions cost.

So quote `VmHWM` as a ceiling, quote the sandbox delta for scaling, and quote a
single RSS reading for neither.

## The trap: `MemoryPeak` is a cgroup figure

`systemctl --user show agent-factory-daemon.service -p MemoryPeak`, and the
`Consumed …, N memory peak` line systemd writes to the journal on every stop, do
**not** report the daemon's memory. They report the high-water mark of the
*unit's cgroup*, which covers:

- the daemon process;
- its **direct children that are not rerouted into another scope** — everything
  spawned with a plain `exec`, including anything that outlives a stop under
  `KillMode=process`;
- **file-backed memory charged to this cgroup** — page cache is charged to
  whichever cgroup first instantiates a page, so this covers what these
  processes brought in, not everything they read: pages another cgroup had
  already cached stay charged there, and pages a child brought in stay charged
  here after that child has exited;
- kernel slab.

Not everything the daemon starts lands there. af deliberately puts two classes
of child in their own transient systemd scopes — tmux servers
(`session/tmux/server_scope_linux.go`) and long-lived watchers and editors
(`newDaemonChildCommand`, `daemon/child_scope_linux.go`) — and those scopes are
siblings of the daemon unit, not children of it. Agent processes are descendants
of a tmux server, so once that server is a scoped one they sit outside the
unit's figure, which is why the sizing section below counts them separately.

**One case where they do not: a legacy tmux server after an upgrade.**
`ensureDedicatedServer` deliberately leaves a foreign or legacy server alone
rather than moving it, and the unit's `KillMode=process` exists partly to
preserve servers an older daemon had already placed in the service cgroup
(`daemon/autostart.go`). Every agent under such a server is its descendant and
**does** count toward the unit's `MemoryPeak` — until a dedicated scoped server
replaces it. If your unit has been upgraded in place and its figure looks far
too large for a daemon, check whether a pre-upgrade tmux server is still living
in its cgroup before reading anything else into the number.

On the measured box, `MemoryPeak` ran **5.9–38.3 GB per unit lifetime**. One of
those thirteen lifetimes had the daemon process sampled through `/proc`: it
never exceeded **92.6 MB**, or **1.2%** of that lifetime's own 7.87 GB peak.

That comparison does not reach the other twelve. `VmHWM` is per-process and
disappears with the process, so no process-level figure exists for a daemon that
has already exited, and none was retained. Two of the twelve are still bounded,
by the unit figure rather than by process telemetry: the two shortest lifetimes
(24 and 74 minutes, before the fleet warmed up) had **unit** peaks of 73.5 MB
and 131.5 MB, so whatever the daemon held then fitted inside that. The remaining
ten have no daemon-level measurement at all — which is the reason to run the
commands below on your own box rather than reason backwards from a peak.

The clearest demonstration is that the cgroup number moves on its own. Over one
20-minute window — no restart, no config change, and no trend in the daemon's
own memory (`VmHWM` flat, `VmRSS` oscillating inside its usual band):

| cgroup measure | start | after 20 min |
|---|---|---|
| `MemoryCurrent` | 2.16 GB | 1.20 GB (−44%) |
| `anon` | 25–50 MB | same range — no trend |
| `file` (page cache) | 1,832 MB | 942 MB |
| `slab` | 431 MB | 367 MB |

The ~960 MB that `MemoryCurrent` shed is accounted for by the two cgroup lines
that fell: `file` by 890 MB and `slab` by 64 MB. That is the kernel reclaiming,
not the daemon shrinking — and the page makes no claim that process memory held
still, because it did not: `VmRSS` oscillated 45–60 MB across the same window as
the GC returned and re-took pages. `VmHWM` not moving says only that the
process's historical maximum was never exceeded. What the oscillation cannot do
is explain the cgroup drop: a ~15 MB swing is about 60 times too small for a
~960 MB fall, and even the daemon's *entire* resident set of 45–60 MB is 16–21
times smaller than it.

That makes a mostly-`file` peak **weak evidence of a leak**. It does not, on its
own, establish that there was no memory pressure. `file` also counts tmpfs and
shared memory, and dirty and writeback pages that must reach disk before they
can be dropped, so under a `memory.high` or `memory.max` limit reclaiming it
costs stalls and writeback rather than being free. Rule pressure out with the
signals that measure it, not with this table: `memory.events` (the `high`,
`max` and `oom` counters), the `file_dirty` and `file_writeback` lines of
`memory.stat`, and PSI in `memory.pressure`.

### Commands that do answer "how much memory is the daemon using"

```bash
# The daemon process: current resident set, and its own high-water mark.
grep -E 'VmRSS|VmHWM' \
  /proc/$(systemctl --user show agent-factory-daemon.service -p MainPID --value)/status

# Split the cgroup number into its parts. anon is anonymous memory the cgroup
# actually holds, file is file-backed memory — page cache, but also tmpfs/shmem
# and dirty or writeback pages — and slab is kernel structures. Read shmem,
# file_dirty and file_writeback too before calling any of file reclaimable.
grep -E '^(anon|file|shmem|file_dirty|file_writeback|slab) ' \
  "/sys/fs/cgroup$(systemctl --user show agent-factory-daemon.service -p ControlGroup --value)/memory.stat"

# For contrast — the high-water of all of the above, over every process that has
# ever lived in the cgroup. This is the number that is not daemon memory.
systemctl --user show agent-factory-daemon.service -p MemoryPeak -p MemoryCurrent
```

`VmHWM` is the figure to quote for the daemon itself: the process's own peak,
which does not fall back as the GC returns pages. Read it as a ceiling on that
one process — it is not a delta, and it dies with the process, so it says
nothing about a daemon that has already exited. `MemoryPeak` is exported only by
newer systemd; the journal's `N memory peak` line on stop is the same figure by
another route.

All three commands are read-only — they signal nothing and restart nothing.
`af daemon status` remains the supported way to ask about the daemon's health;
these are for the memory question specifically.

The trap is Linux-specific. On macOS the daemon runs under launchd, which has no
cgroup accounting, so there is no equivalent unit-level number to misread — but
the daemon figures above were measured on Linux, and macOS was not sampled.

## What actually drives the cgroup number

One measured driver, and one number that is easy to mistake for one. Neither is
the daemon's own memory.

### The measured driver: `post_worktree_commands`

**`post_worktree_commands` run inside the daemon's cgroup today** — the current
behaviour, and a bug being fixed under
[#3650](https://github.com/sachiniyer/agent-factory/issues/3650) rather than a
permanent property. When the daemon creates a worktree it runs your repo's
post-worktree hook as its own child, with no scope of its own, so whatever that
hook builds is accounted to the daemon unit — its anon memory *and* its page
cache. On the measured box, one repo's hook runs
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

### A high child-process rate, which is not a measured driver

Sampling the unit's `cgroup.procs` at full speed for 75 seconds found **1,243
distinct child processes — about 16.6 per second, on the order of 1.4M per
day**. By name, most were `tmux capture-pane` and the rest largely
`git rev-parse`. That rate is a measured fact and worth knowing on its own.

It is a rate, not a cost. No CPU time was measured here — sampling `cgroup.procs`
counts processes, and the heap profile counts allocations — and what these
children burn is child CPU rather than daemon CPU in any case. Nor does the rate
decompose into one capture per session per poll: on the local path a single poll
captures the pane **twice** (`CheckAndHandleTrustPrompt` in
`session/tmux/start.go`, then `HasUpdatedWithBaseline` in `session/tmux/io.go`),
and other contexts skip polling altogether. Take the 16.6/s as measured and
leave it there.

It is **not** an established cause of the cgroup's `file` figure, and this page
will not claim it is. Page cache is charged on first touch and shared
afterwards, so re-running the same handful of small binaries mostly reuses pages
that are already charged, and `capture-pane` talks to the tmux server rather
than reading much from disk. No cache delta was ever attributed to these
processes. The exec rate and the gigabytes are two separate measurements, and
only the hook correlation above connects anything to the peak.

## Sizing a machine

- **The daemon: ~128 MB**, at the load measured (20 live sessions · 720 records
  · 20 tasks). Untested above that.
- **Plus the agents.** Every session runs a real agent process under a real tmux
  server, and that is where session memory actually goes. It is outside the
  daemon's footprint and outside this page; no figure for it was measured here.
  It is outside the *unit's* figure too — but only once a scoped tmux server has
  replaced any legacy one left in the service cgroup by a pre-upgrade daemon.
- **Plus whatever `post_worktree_commands` builds**, per worktree — charged to
  the daemon's cgroup until #3650 moves it out.
- **Plus everything else a session can start.** A session may hold any number of
  extra process-bearing tabs — shell, process, editor — and there is no cap on
  how many (#3021). Watch tasks run their command, editors and watchers run
  theirs in sibling scopes, and a remote-agent backend adds its transport at
  both ends. None of that was measured here, and the scoped trees are outside
  the daemon unit's figure as well, so a busy host is under-sized if you count
  only the daemon, the agents and the hooks.

If a box is under memory pressure, do not start from the unit's `MemoryPeak`.
Read `VmHWM` for the daemon process, read `memory.stat` to see how much of the
cgroup figure is page cache, and read `memory.events` and `memory.pressure` to
find out whether that cache is actually costing anything. On the box above, that
sequence turned a gigabyte-scale "daemon leak" into a 92.6 MB process and a hook
building `node_modules` in the wrong cgroup.

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
window, plus 13 historical unit lifetimes read out of the journal and a
**second, sandbox daemon** — the same source revision built `-tags afpprof` with
a pprof listener, under a throwaway `AGENT_FACTORY_HOME` and `TMUX_TMPDIR` —
which supplied every heap, goroutine and 0-session figure, because the live
daemon exposes no profiling endpoint and was never observed idle.

Three limits worth carrying away. Rows say which daemon they came from, and the
two are not one series. The sandbox figures include the probe that made them
measurable. And **only one of the thirteen lifetimes has process-level
telemetry** — the journal's per-lifetime peaks are unit figures, so the older
lifetimes bound the daemon only as loosely as a cgroup number can.

Nothing here is extrapolated to other hardware, other session counts, or other
operating systems, and nothing was measured above 20 live sessions. Where a
figure is a slope between two points or a fit across lifetimes, it says so.
