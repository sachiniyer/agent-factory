# Agent Factory

Agent Factory (`af`) is a terminal UI for running **many AI coding agents at
once** — Claude Code, Codex, Aider, Gemini — each in its own isolated git
worktree. You spin up a session per task, hand each agent a prompt, and watch
their work stream live in a preview pane; when one is ready, you attach
full-screen and take over. Nothing shares a working tree, so agents never step
on each other, and every session is a real branch you can review and merge like
any other.

It runs on Linux and macOS (and Windows under WSL), needs only **tmux**, **git**,
and at least one agent installed — no Go, no daemon to babysit. Start with
[Getting started](getting-started.md).

## What it does

- **Runs agents in parallel, in isolation.** Each session is a dedicated git
  worktree on its own branch. Five agents can refactor five corners of the same
  repo simultaneously and never collide. See
  [Worktree-isolated agents](concepts/worktree-agents.md).
- **Keeps you in one screen.** A sidebar lists every session with live status;
  the preview pane snapshots any agent's terminal without attaching; `↵`
  attaches you full-screen and a detach key drops you back. Extra **tabs** run
  shells or long-lived processes (a dev server, `btop`) alongside the agent in
  the same worktree.
- **Survives restarts and works unattended.** A background **daemon** owns all
  state and keeps things alive: it re-spawns a session's process if it dies,
  runs scheduled and event-driven **tasks**, and can auto-accept agent prompts
  so work proceeds while you're away. See [The daemon](concepts/daemon.md) and
  [Tasks & automation](tasks.md).
- **Automates on a schedule or a signal.** A task delivers a prompt to an agent
  on a **cron** schedule ("triage issues every morning at 9") or whenever a
  **watch script** emits a line (a new row in a queue, a failing build). Each
  run can spawn a fresh session or target an existing one.
- **Scriptable end to end.** Everything the TUI does, the `af` CLI does too, as
  JSON on stdout — so it composes with `jq` and shell. The same operations are
  also a small local **HTTP/JSON API** over a Unix socket, for calling `af` from
  another language or from inside an agent. See the
  [CLI reference](reference/cli.md) and [HTTP API reference](reference/api.md).
- **Reaches beyond your laptop.** With **remote hooks**, sessions can run on a
  backend you define — your own launch/list/attach/delete scripts — and appear
  in the same sidebar with the same attach/kill/preview experience. See
  [Remote hooks](remote-hooks.md).
- **Handles usage limits gracefully.** When Claude or Codex hits a usage limit,
  `af` marks the session and can auto-resume it once the window elapses; task
  runs that hit a limit are parked and resumed, never counted as failures. See
  [Usage limits](usage-limits.md).

## How it fits together

```
        ┌──────────────────────────────────────────────┐
        │  af TUI  (sidebar · preview · attach · tabs)  │
        └──────────────────────┬───────────────────────┘
                               │ read-only projection + RPC
        ┌──────────────────────┴───────────────────────┐
        │  daemon  — single writer of all state         │
        │  · re-spawns dead sessions                    │
        │  · runs cron + watch tasks                    │
        │  · autoyes / usage-limit auto-resume          │
        │  · serves the HTTP/JSON API                   │
        └──────────────────────┬───────────────────────┘
                               │ owns
        ┌──────────────────────┴───────────────────────┐
        │  sessions = git worktrees, one agent each     │
        │  local  ·  or remote (via remote hooks)       │
        └───────────────────────────────────────────────┘
```

The TUI and the CLI are both thin front-ends; the daemon is the single source of
truth that owns every write. That is why the TUI, the CLI, and the HTTP API can
never show you three different worlds. Read [The daemon](concepts/daemon.md) for
why that design matters.

## Where to go next

- **New here?** [Getting started](getting-started.md) — install, first session,
  attach and detach.
- **Want the model in your head?** The *Concepts* section:
  [worktree-isolated agents](concepts/worktree-agents.md),
  [the daemon](concepts/daemon.md), [the TUI](concepts/tui.md),
  [tasks](tasks.md), [remote hooks](remote-hooks.md).
- **Scripting or integrating?** The [CLI reference](reference/cli.md) and
  [HTTP API reference](reference/api.md) — both generated from the code, so they
  never drift.
