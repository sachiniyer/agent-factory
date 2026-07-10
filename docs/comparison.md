# Comparison

Agent Factory is built around a specific workflow: give each agent a real git
worktree and branch, keep the sessions alive through a daemon, and make the same
state available from the TUI, JSON CLI, and local HTTP API.

That is not the only good center of gravity. A classic tmux setup gives you the
smallest set of primitives, and Herdr is a strong terminal-native agent
multiplexer with real panes, agent state, remote attach, and a socket API. Use
the table as a map of tradeoffs, not a scorecard.

## Feature Comparison

| Capability | Agent Factory | tmux + manual worktrees | Herdr |
|---|---|---|---|
| Worktree isolation | Built in for normal sessions: one session creates one branch and one git worktree. `--here` is the explicit in-place exception. | Possible, but you create, name, clean up, and review the worktrees yourself. | Supported through worktree-aware workspace commands; Herdr positions branch/diff review as a separate layer from its terminal multiplexer core. |
| Parallel agents | Built for multiple concurrent sessions in one repo, each scoped to its own worktree. | Works if you launch each agent in a separate pane and directory. Isolation depends on your discipline. | Built for many concurrent terminal agents, with pane/workspace organization and state rollups. |
| Daemon and automation | Background daemon owns state, re-spawns sessions, hosts cron/watch tasks, drives auto-yes, and can auto-resume Claude/Codex usage-limit waits. | tmux keeps panes alive, but scheduling, prompting, restart policy, and cleanup are custom scripts or cron jobs. | Background server keeps panes alive and exposes automation through CLI/socket APIs and plugins. A built-in cron/watch task scheduler is not documented as Herdr's main workflow. |
| CLI + HTTP API | `af` prints JSON for sessions/tasks, and the daemon exposes a local owner-only HTTP/JSON API over a Unix socket. | tmux itself is scriptable; an HTTP API is not part of the tmux/manual stack. | CLI plus a local socket API for workspaces, panes, agents, events, and worktrees; not an HTTP API. |
| Remote execution | Remote hooks let a repo define launch/list/attach/delete/terminal scripts for sessions elsewhere, shown in the same TUI. | SSH + tmux is a proven remote pattern, but agent/session metadata is manual. | First-class remote attach over SSH/direct remote client is a core feature. |
| Open source / license | Open source, GNU AGPL v3. | tmux is open source under the ISC-style tmux license; your glue scripts and chosen agents vary. | Open source AGPL-3.0-or-later, with commercial licensing offered. |
| Supported agents | Named agent choices are `claude`, `codex`, `aider`, `gemini`, and `amp`; configure paths/flags with `program_overrides`. | Anything you can run in a shell. | Broad terminal-agent support, with richer integrations for many listed agents and fallback terminal support for others. |

## When To Choose What

**Choose Agent Factory** when the unit of work is "one task, one branch, one
reviewable worktree" and you want sessions, scheduled prompts, event-driven
tasks, remote hooks, and API access to share one daemon-owned state model.

**Choose tmux + manual worktrees** when you want the fewest moving parts, already
have shell scripts that fit your team, and are comfortable owning all lifecycle,
cleanup, and automation policy yourself.

**Choose Herdr** when the live terminal workspace is the product: persistent
real panes, mouse-native terminal layout, agent state at a glance, direct
attach, remote SSH workflows, and an agent/socket control surface.

These tools can also layer together. For example, an Agent Factory remote hook
can target infrastructure that uses tmux or another multiplexer, and Herdr can
sit beside a worktree workflow when the main need is richer terminal layout and
agent-state awareness.

## Source Notes

- Agent Factory behavior is documented in [Worktree-isolated agents](concepts/worktree-agents.md),
  [The daemon](concepts/daemon.md), [Tasks & automation](tasks.md),
  [Remote hooks](remote-hooks.md), the [CLI reference](reference/cli.md), and
  the [HTTP API reference](reference/api.md).
- Herdr's public docs describe it as a terminal-native agent multiplexer with
  persistent panes, agent state, remote attach, CLI/socket APIs, and worktree
  methods: [Herdr](https://herdr.dev/), [Compare](https://herdr.dev/compare/),
  [Socket API](https://herdr.dev/docs/socket-api/), and
  [license](https://github.com/ogulcancelik/herdr/blob/master/LICENSE).
- tmux describes itself as a terminal multiplexer with detachable sessions; see
  the [tmux README](https://github.com/tmux/tmux/blob/master/README) and
  [license](https://github.com/tmux/tmux/blob/master/COPYING). The worktree
  primitive is git's [git-worktree](https://git-scm.com/docs/git-worktree).
