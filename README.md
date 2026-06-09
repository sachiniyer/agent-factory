# Agent Factory

A terminal UI that manages multiple AI coding agents (Claude Code, Aider, Codex, Gemini) in separate git worktrees. Each agent runs in its own isolated workspace with full git integration, automated tasks, and a programmatic API for orchestration.

Fork of [claude-squad](https://github.com/smtg-ai/claude-squad) with per-repo scoping, programmatic API, and automated tasks.

## Quick Start

### Prerequisites

- **Go 1.24+**
- **tmux** (terminal multiplexer)
- **git**
- An AI coding agent installed (e.g. [Claude Code](https://docs.anthropic.com/en/docs/claude-code))

### Install

```bash
# From source (recommended)
git clone https://github.com/sachiniyer/agent-factory.git
cd agent-factory
./dev-install.sh

# Or build manually
go build -o af .
```

The `dev-install.sh` script builds the `af` binary and installs it to `~/.local/bin/`.

### Usage

```bash
cd your-project    # must be a git repo
af                 # launch the TUI
```

Press `?` for the full keybindings help screen.

## Features

### Session Management

Each session runs an AI agent in an isolated git worktree with its own branch. Sessions persist across restarts.

| Key | Action |
|-----|--------|
| `n` | Create a new session |
| `N` | Create a new remote session (requires `remote_hooks` config) |
| `Enter` / `o` | Attach to selected session |
| `Ctrl-w` | Detach from session |
| `D` | Kill (delete) selected session |
| `j` / `k` | Navigate sessions |
| `a` | Attach to an existing worktree |
| `Tab` | Switch between preview and terminal |

### Tasks

Create recurring automated tasks with cron expressions. Tasks are scheduled by
the agent-factory daemon, which starts automatically whenever `af` runs and an
enabled task exists. To keep schedules firing across reboots without opening
`af`, install the daemon's autostart unit with `af daemon install`.

| Key | Action |
|-----|--------|
| `s` | Create a new task |
| `S` | List tasks |
| `r` | Run selected task now |

### GitHub PR Integration

Automatically detects pull requests for session branches via `gh pr view`.

| Key | Action |
|-----|--------|
| `p` | Open PR in browser |
| `P` | Copy PR URL to clipboard |

### Worktree Hooks

Per-repo shell commands that run when a new worktree is created (e.g. `npm install`, `make build`).

| Key | Action |
|-----|--------|
| `H` | Navigate to hooks section |

## Per-Repo Scoping

All data (sessions, tasks) is scoped to the current git repository. The TUI shows only what's relevant to the repo you're in.

- Sessions stored at `~/.agent-factory/instances/<repoID>/instances.json`
- Configuration at `~/.agent-factory/config.json`

## Programmatic API

The `af sessions` and `af tasks` subcommands provide a JSON CLI for driving Agent Factory without the TUI. All commands output JSON to stdout and errors to stderr; use `--repo <path>` to target a specific repository.

### Sessions

```bash
af sessions list                                              # list sessions in current repo
af sessions get <title>                                        # fetch one session
af sessions create --name <title> --prompt "fix the bug"       # create + start a session
af sessions preview <title>                                    # snapshot the session's pane
af sessions attach <title>                                     # attach interactively (foreground)
af sessions send-prompt <title> "..."                          # append a prompt to a session
af sessions send-prompt <title> "..." --create                 # send-or-create
af sessions whoami                                             # report the session this shell is inside
af sessions kill <title>                                       # kill + clean up the session
```

### Tasks

```bash
af tasks list                                                  # list scheduled tasks
af tasks add --name "Daily triage" --prompt "..." --cron "0 9 * * *" --program claude
af tasks get <id>                                              # fetch one task
af tasks trigger <id>                                          # run task immediately
af tasks update <id> --cron "..." --prompt "..." --enabled true
af tasks remove <id>                                           # delete a task
```

### Daemon

The background daemon hosts task cron schedules and autoyes mode. It starts on
demand; installing it as a user-level autostart unit (systemd user service on
Linux, launchd agent on macOS) keeps scheduled tasks firing after reboots.

```bash
af daemon install                                              # register autostart at login
af daemon uninstall                                            # remove the autostart unit
```

## Maintenance

This repo is autonomously maintained by Captain Claude, an AI maintainer running on Claude Code. The full operating contract lives in [CLAUDE.md](CLAUDE.md).

**Triage SLA.** Every open issue lands in one of three states within an hourly sweep: a plan plus a dispatched implementation, a `needs-info` clarification request, or a close with a stated reason. Issues do not sit silently.

**PR discipline.** CI green is the floor, not the ceiling. Every PR — including external contributions — is built on the maintainer's machine, the four CI gates re-run locally (`golangci-lint --fast`, `gofmt -l .`, `go build ./...`, `go test -race ./...`), and the change exercised end-to-end before merge. PRs touching `session/tmux/`, `session/git/`, `daemon/`, `task/`, or `api/` get extra real-binary verification.

**External users are first-class.** Install path, public CLI/API stability (`af sessions`, `af tasks`, REST `api/`), and regression coverage take priority over maintainer convenience or shipping speed. Auto-release tags `master` every 3 hours, so anything merged ships that fast.

**Filing useful issues.**

- A short title that names the affected area (e.g. "session resume on machine shutdown").
- Steps to reproduce, expected behavior, actual behavior.
- `af version` output and your platform (Linux/macOS).
- Logs from `~/.config/agent-factory/agent-factory.log` when relevant.

## Configuration

Configuration lives at `~/.agent-factory/config.json`:

```json
{
  "default_program": "claude",
  "program_overrides": {
    "claude": "/home/me/.local/bin/claude --dangerously-skip-permissions"
  },
  "auto_yes": false,
  "daemon_poll_interval": 1000,
  "branch_prefix": "username/"
}
```

| Field | Description |
|-------|-------------|
| `default_program` | Default agent enum. Must be one of `claude`, `codex`, `aider`, `gemini`. |
| `program_overrides` | Optional map from agent enum to the full command string used when launching that agent. Use this to pin a path or pass flags (e.g. `--dangerously-skip-permissions`). Keys must be one of `claude`, `codex`, `aider`, `gemini`. |
| `auto_yes` | Auto-accept agent prompts |
| `daemon_poll_interval` | Daemon polling interval in ms |
| `branch_prefix` | Prefix for worktree branches (defaults to `username/`) |

Override the per-session agent with `-p`:

```bash
af -p aider
```

`-p` and the per-task `program` field both accept a bare agent enum only. To
pass a custom path or flags for an agent, set `program_overrides.<agent>` in
your config — every session that launches that agent will use the override.

## Upstream

For general documentation about the original claude-squad project, see [smtg-ai/claude-squad](https://github.com/smtg-ai/claude-squad).

## Release Testing

See [docs/release-testing-plan.md](docs/release-testing-plan.md) for the release gate, manual smoke checks, and artifact verification checklist.

## License

[GNU AGPL v3](LICENSE.md)
