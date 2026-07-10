# Why Agent Factory

Coding agents changed the bottleneck. The hard part is no longer typing every
line yourself; it is keeping multiple agent attempts isolated, observable, and
reviewable without turning your development machine into a pile of stray
terminals and mystery branches.

Agent Factory is built around one opinion: **agent work should become normal
git work as quickly as possible.** A task starts as a prompt, runs in an
isolated worktree, and comes back as a branch you can diff, test, review, merge,
archive, or delete.

## The Problem

Running one agent in one shell is simple. Running five agents across the same
repository is where the workflow starts to fray:

- agents edit the same checkout and overwrite each other;
- progress disappears into terminal tabs you are not watching;
- "done" is ambiguous until you find the files, branch, diff, and test state;
- background tasks need their own scheduler, restart policy, and logs;
- crashes, reboots, usage limits, and remote machines each create a new edge
  case.

You can solve pieces of that with tmux, shell scripts, cron, and manual
worktrees. Agent Factory packages the workflow into one terminal-native control
plane.

## The Agent Factory Bet

<div class="grid cards" markdown>

-   :material-source-branch:{ .lg .middle } __Isolation is the default__

    ---

    A normal session gets its own branch and git worktree. Parallel agents do
    not share a mutable checkout.

-   :material-eye-outline:{ .lg .middle } __Visibility beats babysitting__

    ---

    The TUI shows status and live Agent tab snapshots so you can scan many
    sessions without attaching to each one.

-   :material-database-sync-outline:{ .lg .middle } __One daemon owns state__

    ---

    The TUI, CLI, and HTTP API all read the daemon's projection and request
    changes through it. There is one writer and one source of truth.

-   :material-git:{ .lg .middle } __Review stays in git__

    ---

    Agent output is not trapped in an app-specific workspace. It is a branch
    with files you can inspect, test, push, and merge normally.

-   :material-console:{ .lg .middle } __Everything is scriptable__

    ---

    The same operations exposed in the TUI are available as JSON CLI commands
    and through a local HTTP/JSON API.

-   :material-history:{ .lg .middle } __Done work is recoverable__

    ---

    Archive is the default finish action: the session stops, but the worktree,
    branch, and record stay restorable.

</div>

## Where It Fits

Use Agent Factory when you want a local, terminal-native workflow for many
coding agents and you care about the path from prompt to reviewable branch.

It is especially useful when:

- you run Claude Code, Codex, Aider, Gemini, or Amp from the command line;
- you want each task isolated in a git worktree by default;
- you prefer a TUI and scriptable CLI over a desktop GUI;
- recurring prompts, watch scripts, or remote backends are part of your flow;
- you want explicit archive/restore/kill semantics around agent work.

It is probably not the right center of gravity if you want a visual kanban board
for a whole team, an IDE-like inline diff editor, or a general-purpose terminal
multiplexer for every shell on your machine. See the [comparison](comparison.md)
for those tradeoffs.

## Design Principles

- **Keep the user's repo understandable.** Branches, worktrees, and diffs should
  look like normal git artifacts.
- **Make background work visible.** A session can run unattended, but it should
  not be invisible.
- **Prefer recovery over deletion.** Archive first; kill only when you mean to
  discard.
- **Treat automation as first-class.** Cron tasks, watch scripts, CLI calls, and
  API calls should share the same state model as interactive use.
- **Stay local by default.** The HTTP API is local-only over a Unix socket, and
  remote execution is explicit through repo-owned hooks.
