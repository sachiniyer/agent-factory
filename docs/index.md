---
template: home.html
hide:
  - navigation
  - toc
---

<p class="af-section-label">Core capabilities</p>
<h2 class="af-section-title">Everything you need to run agents at scale</h2>

<div class="grid cards" markdown>

-   :material-source-branch:{ .lg .middle } __Parallel agents, in isolation__

    ---

    Each session is a dedicated git worktree on its own branch. Five agents can
    refactor five corners of the same repo at once and never collide — every
    session is a real branch you review and merge like any other.

    [:octicons-arrow-right-24: Worktree-isolated agents](concepts/worktree-agents.md)

-   :material-view-dashboard-outline:{ .lg .middle } __One-screen workflow__

    ---

    A sidebar lists every session with live status; the preview pane snapshots
    any agent without attaching; `↵` takes you full-screen. Extra tabs run a
    shell or dev server alongside the agent in the same worktree.

    [:octicons-arrow-right-24: The TUI](concepts/tui.md)

-   :material-cog-sync-outline:{ .lg .middle } __A daemon that never sleeps__

    ---

    A background daemon owns all state: it re-spawns dead sessions, runs
    scheduled and event-driven tasks, and can auto-accept prompts so work
    proceeds while you're away — surviving restarts.

    [:octicons-arrow-right-24: The daemon](concepts/daemon.md)

-   :material-console:{ .lg .middle } __Scriptable, CLI + HTTP__

    ---

    Everything the TUI does, the `af` CLI does too — as JSON on stdout, so it
    composes with `jq`. The same operations are a local HTTP/JSON API over a
    Unix socket for calling `af` from any language.

    [:octicons-arrow-right-24: CLI reference](reference/cli.md)

-   :material-cloud-outline:{ .lg .middle } __Reaches beyond your laptop__

    ---

    With remote hooks, sessions run on a backend you define — your own
    launch/list/attach/delete scripts — and appear in the same sidebar with the
    same attach, kill, and preview experience.

    [:octicons-arrow-right-24: Remote hooks](remote-hooks.md)

-   :material-timer-sand:{ .lg .middle } __Handles usage limits__

    ---

    When Claude or Codex hits a usage limit, `af` marks the session and
    auto-resumes it once the window elapses. Task runs that hit a limit are
    parked and resumed — never counted as failures.

    [:octicons-arrow-right-24: Usage limits](usage-limits.md)

</div>

## How it fits together

The TUI and the CLI are both thin front-ends; the **daemon** is the single
source of truth that owns every write. That is why the TUI, the CLI, and the
HTTP API can never show you three different worlds.

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

Read [The daemon](concepts/daemon.md) for why that design matters.

## Where to go next

<div class="grid cards" markdown>

-   :material-rocket-launch-outline:{ .lg .middle } __New here?__

    ---

    Install `af`, create your first session, attach and detach.

    [:octicons-arrow-right-24: Getting started](getting-started.md)

-   :material-lightbulb-on-outline:{ .lg .middle } __Want the model in your head?__

    ---

    The concepts: worktree-isolated agents, the daemon, the TUI, tasks, and
    remote hooks.

    [:octicons-arrow-right-24: Concepts](concepts/worktree-agents.md)

-   :material-code-braces:{ .lg .middle } __Scripting or integrating?__

    ---

    The CLI and HTTP API references — both generated from the code, so they
    never drift.

    [:octicons-arrow-right-24: CLI reference](reference/cli.md)

</div>
