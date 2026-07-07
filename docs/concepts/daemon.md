# The daemon

Behind the TUI is a long-lived background process: the **Agent Factory daemon**.
It is the piece that makes sessions survive restarts, keeps automations firing,
and guarantees the TUI, the CLI, and the HTTP API never disagree about what
exists. You rarely interact with it directly, but understanding it explains a lot
about how `af` behaves.

## The single-writer model

The daemon is the **single writer and owner of all state**. Session records,
task definitions, the worktrees on disk — the daemon performs every mutation.
The TUI and the `af` CLI are pure clients: they render a read-only projection of
the daemon's state and send it RPCs to *request* changes; they never write state
files themselves.

This is a deliberate design (the "#960 single-writer model"), and it's worth the
paragraph because it's why `af` doesn't corrupt itself:

- **One writer, no clobbering.** When two front-ends could both write the same
  state file, the last writer silently wins and the other's change vanishes.
  With a single writer, that entire class of bug cannot occur — every change is
  serialized through the daemon.
- **One source of truth.** The sidebar you see, the JSON `af sessions list`
  prints, and the HTTP API's `Snapshot` all read the *same* in-memory state. They
  can't drift, because there is only one authority.
- **Clients can come and go.** Close the TUI and your sessions keep running.
  Reopen it and it reconnects to the daemon's live state — nothing was lost,
  because the TUI was never holding the state in the first place.

## What the daemon does

- **Keeps sessions alive.** If a session's process dies unexpectedly, the daemon
  re-spawns it in place — the worktree and record are preserved, so the agent
  comes back where it was. The always-on [root agent](../configuration.md#root-agents-always-ensured)
  is maintained the same way.
- **Runs the scheduler.** All [tasks](../tasks.md) — cron schedules and
  watch-script triggers — are hosted by the daemon. It arms the timers, watches
  the scripts, and delivers prompts on time, whether or not a TUI is open.
- **Drives auto-yes.** In autoyes mode the daemon auto-accepts agent prompts so
  work proceeds unattended.
- **Handles usage limits.** With auto-resume enabled, the daemon parks a session
  that hit a Claude/Codex usage limit and resumes it once the window elapses —
  see [Usage limits](../usage-limits.md).
- **Serves the HTTP/JSON API.** The daemon exposes every session and task
  operation over a local Unix socket — see the
  [HTTP API reference](../reference/api.md).

## Lifecycle

The daemon starts **on demand**: whenever you run `af` and there is work to host
(an enabled task, autoyes, a root agent), `af` makes sure a daemon is running.
That means for interactive use you usually don't have to think about it at all.

To keep tasks and sessions running across logouts and reboots, install the
daemon's autostart unit once:

```bash
af daemon install     # systemd user service on Linux, launchd agent on macOS
af daemon status      # liveness, sockets, pid, and autostart state
af daemon restart     # restart the daemon process; sessions are re-adopted
af daemon uninstall   # remove the autostart unit
```

Because the daemon owns live state, you don't stop it by force while sessions
are running; let `af` manage its lifecycle. `af daemon restart` restarts only
the daemon process and re-adopts existing tmux sessions from persisted state.
`af daemon status` is the right tool when you want to know whether it's up and
where its sockets are.

## Sockets

The daemon listens on two local Unix sockets under `$AGENT_FACTORY_HOME`
(default `~/.agent-factory`): an internal control socket the TUI and CLI use, and
the HTTP/JSON socket (`daemon-http.sock`) for the public API. Both are
owner-only (`0600`) and local — never a TCP port, never the network. See the
[HTTP API guide](../http-api.md) for the transport and auth details.
