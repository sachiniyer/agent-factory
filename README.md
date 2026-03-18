# Agent Factory

A terminal UI that manages multiple AI coding agents (Claude Code, Aider, Codex, Amp) in separate git worktrees. Each agent runs in its own isolated workspace with full git integration, a kanban board for task tracking, and a programmatic API for orchestration.

Fork of [claude-squad](https://github.com/smtg-ai/claude-squad) with per-repo scoping, kanban board, programmatic API, and automated tasks.

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
| `N` | Create a session with an initial prompt |
| `Enter` / `o` | Attach to selected session |
| `Ctrl-w` | Detach from session |
| `D` | Kill (delete) selected session |
| `j` / `k` | Navigate sessions |
| `a` | Attach to an existing worktree |
| `Tab` | Switch between preview, diff, and terminal |

### Kanban Board

A per-repo kanban board with four columns: Backlog, In Progress, Review, Done. Tasks can be linked to sessions.

| Key | Action |
|-----|--------|
| `t` | Navigate to kanban board |
| `n` | Add new task (when focused) |
| `m` | Grab/drop task to move between columns |
| `d` | Delete task |
| `o` | Jump to linked session |
| `a` | Link/attach session to task |
| `c` | Clear all done tasks |

### Tasks

Create recurring automated tasks with cron expressions. Tasks are backed by systemd timers (Linux).

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

All data (sessions, board, tasks) is scoped to the current git repository. The TUI shows only what's relevant to the repo you're in.

- Sessions stored at `~/.agent-factory/instances/<repoID>/instances.json`
- Tasks stored at `~/.agent-factory/tasks/<repoID>/board.json`
- Configuration at `~/.agent-factory/config.json`

## Programmatic API

The `af api` subcommand provides a JSON CLI for driving Agent Factory without the TUI:

```bash
# Sessions
af api sessions list
af api sessions create --name my-task --prompt "fix the login bug"
af api sessions preview my-task
af api sessions diff my-task
af api sessions kill my-task

# Kanban board
af api board view
af api board add --title "fix auth" --status in_progress
af api board move <id> --status done
af api board link <id> --instance my-task

# Tasks
af api tasks list
af api tasks add --name "Daily triage" --prompt "triage new issues" --cron "0 9 * * *"
af api tasks remove <id>
```

All commands output JSON to stdout and errors to stderr. Use `--repo <path>` or `--repo-id <id>` to target a specific repository.

## Configuration

Configuration lives at `~/.agent-factory/config.json`:

```json
{
  "default_program": "claude --dangerously-skip-permissions",
  "auto_yes": false,
  "daemon_poll_interval": 1000,
  "branch_prefix": "username/",
}
```

| Field | Description |
|-------|-------------|
| `default_program` | AI agent command to run (auto-detected) |
| `auto_yes` | Auto-accept agent prompts |
| `daemon_poll_interval` | Daemon polling interval in ms |
| `branch_prefix` | Prefix for worktree branches (defaults to `username/`) |

Override the program per-session with `-p`:

```bash
af -p "aider --model ollama_chat/gemma3:1b"
```

## Upstream

For general documentation about the original claude-squad project, see [smtg-ai/claude-squad](https://github.com/smtg-ai/claude-squad).

## License

[GNU AGPL v3](LICENSE.md)
