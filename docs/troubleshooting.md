# Troubleshooting

For anyone whose `af` is not behaving. After this page you will know which
command answers which kind of question, what `af doctor` can fix on its own, and
what to attach when you file an issue.

## Start with `af doctor`

One command covers both halves of "something is wrong":

```bash
af doctor --setup   # can this machine create a session at all?
af doctor           # what has accumulated on a machine already running af?
```

**`--setup`** is the first-run profile. It checks AF home writability, config
materialization and parsing, git and the current repo, git identity, tmux, your
configured agent commands, state and log storage, daemon health, and remote-hook
setup when this repo configures one. Run it after installing, and any time a
session refuses to start.

**Without a flag**, doctor runs the maintenance sweep instead — the problems
that accumulate silently rather than failing loudly:

- orphaned processes from sessions that no longer exist, processes that escaped
  a live session's pane, and processes pegging a CPU core for hours;
- `af_` tmux sessions with no backing session record;
- abandoned agent-factory homes and daemons under the temp dir, and temp
  directories holding nothing but a socket nobody answers on;
- daemon health: control socket, autostart unit, pid file, binary freshness;
- client/daemon version skew, and the ways a stale daemon survives an upgrade —
  a second daemon on this home, an autostart unit launching a different `af`
  binary than yours, several installs at different versions, sockets left behind
  with nobody answering;
- remote-hook setup for the current repo, and pinned host-key directories under
  `hook-hosts/` that no session owns.

Two flags are worth knowing:

- **`--verbose`** shows per-process findings instead of collapsed summaries.
- **`--fix`** applies the safe remediations — killing verified orphans and
  leaked daemons, removing stale temp homes and dead-socket directories.
  It is deliberately conservative: a daemon whose binary is merely *missing* is
  reported and not stopped, because `af upgrade` replaces the file in place and
  every healthy daemon looks that way until it restarts.

`--json` emits each check in the `{data, error}` envelope if you want to gate a
script on it.

## Common situations

**A session says Lost or Dead.** Its process or worktree went away — after a
crash, a reboot, or a worktree someone deleted by hand. Select it and press
**`r`**, or run `af sessions restore <title>`. Restore is the right move first;
`af sessions kill` is permanent and takes uncommitted work with it.

**A session is idle and you cannot tell why.** Every idle row carries the reason
the daemon can mechanically establish — `usage limit`, `process exited`,
`no change after delivery`, `pane changed · 12m ago`. It never claims the agent
*finished* or *asked a question*, because those render identically in a
terminal; read the pane to decide. The vocabulary is in the
[HTTP API guide](http-api.md#session-idle-diagnosis).

**A session is parked on a usage limit.** It carries a `[limit]` badge. Retry it
with **`c`** in the TUI or `af sessions retry-limit <title>`, hand the work to a
different agent, or turn on auto-resume. See
[accounts and usage limits](usage-limits.md). `af quota` reports what each agent
CLI exposes — and says `not reported` where a provider exposes no quota API,
which is `af` declining to guess rather than a ceiling of zero.

**The web client will not load.** The daemon serves it at
`http://127.0.0.1:8443` by default. Check the daemon is up
(`af daemon status`), that `network.listen_addr` is not set to `""` (which turns
the web client off), and that nothing else holds the port. Reaching it from
another machine is a different problem — see
[remote daemon access](remote-http-auth.md).

**Config edits do not seem to apply.** `af daemon status` reports whether the
running daemon has applied the config on disk. `af config get <key> --explain`
shows every candidate layer, whether it was present and allowed, and why it won
or lost. Note that a bare `af config get` reads the **current repository's**
effective config, and falls back to global outside a git repo.

**The daemon is wedged, or you upgraded and the old one is still serving.**
`af daemon restart` restarts the process and re-adopts existing tmux sessions
from persisted state; `af daemon adopt` hands a detached daemon back to the
installed autostart unit. Do not `kill -9` a daemon with live sessions.

## Where the details are

```bash
af debug         # config paths and the effective global config
af version       # client version
af daemon status # liveness, supervision ownership, config freshness
```

The application log is `~/.config/agent-factory/agent-factory.log` (and moves
into `$AGENT_FACTORY_HOME` when that variable is set); per-task watch logs are
`~/.agent-factory/logs/task-<id>.log`. Both rotate on `log_max_size_mb` and
`log_max_backups`. The full list is in
[configuration → where state lives](configuration.md#where-state-lives).

## Filing a bug

```bash
af bug-report
```

That bundles the daemon log tail, versions, configured tasks, session state, the
daemon health snapshot, and your global config — **redacted** — into one file
(`~/af-bug-report-<timestamp>.txt`), and opens a pre-filled GitHub issue draft
against [sachiniyer/agent-factory](https://github.com/sachiniyer/agent-factory).
The draft is never submitted for you: review it, attach the bundle, and click
Submit yourself.

Include repro steps and expected vs. actual behaviour alongside it.
