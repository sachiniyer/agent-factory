# siyer-claude-squad

Fork of [claude-squad](https://github.com/smtg-ai/claude-squad) with per-repo scoping, task management, a programmatic API, and [NanoClaw](https://github.com/sachiniyer/nanoclaw) integration.

## What's different from upstream

### Per-repo scoping

Instances, tasks, and schedules are scoped to the current git repository instead of being global.

- **Instances** stored per-repo at `~/.claude-squad/instances/<repoID>/instances.json` (auto-migrated from the old global `state.json`)
- **Schedules** filtered to the current repo in the TUI and schedule list
- **`repoID`** derived from SHA-256 of the git root path — shared across `config.RepoContext`
- The daemon loads instances from all repos; the TUI scopes to whichever repo you're in

### Per-repo task list (press `t`)

A todo list overlay for managing tasks per repository.

- Press `t` to open the task list
- Add, toggle, and delete tasks
- Tasks stored at `~/.claude-squad/tasks/<repoID>/tasks.json`

### Attach to existing worktrees (press `a`)

Create a session against an existing git worktree instead of always creating a new one.

- Press `a` to see all worktrees for the current repo
- Worktrees that already have a session are annotated with `[has session]`
- Select a worktree, name the session, and it attaches to the existing branch

### `cs api` — Programmatic JSON API

A CLI subcommand tree for driving claude-squad without the TUI. All commands output JSON to stdout, errors to stderr.

```bash
# Sessions
cs api sessions list [--repo <path>]
cs api sessions get <title>
cs api sessions create --repo <path> --name <name> [--prompt <text>] [--program <prog>]
cs api sessions send-prompt <title> <prompt>
cs api sessions preview <title>
cs api sessions diff <title>
cs api sessions kill <title>
cs api sessions push <title> [--message <msg>]
cs api sessions pause <title>
cs api sessions resume <title>

# Schedules
cs api schedules list [--repo <path>]
cs api schedules add --repo <path> --prompt <text> --cron <expr> [--program <prog>]
cs api schedules remove <id>

# Tasks
cs api tasks list --repo <path>
cs api tasks add --repo <path> --title <text>
cs api tasks toggle <id> --repo <path>
cs api tasks remove <id> --repo <path>
```

Supports `--repo` and `--repo-id` flags for repo scoping from outside git directories (e.g. from NanoClaw containers or scripts).

### NanoClaw integration

Bidirectional bridge to a running [NanoClaw](https://github.com/sachiniyer/nanoclaw) instance.

- **NanoClaw tab** — 4th tab in the TUI (press `tab` to cycle). Shows chat history from nanoclaw's message database with scrolling.
- **Send messages** — Press `m` to compose a message to nanoclaw. Messages include repo metadata (path, repo ID, program).
- **Configuration** — Set `NANOCLAW_DIR` env var (defaults to `~/nanoclaw`). The tab only appears when a valid nanoclaw installation is detected.

```bash
NANOCLAW_DIR=~/nanoclaw cs
```

The nanoclaw side also includes:
- **Container-side MCP server** (`cs-bridge-mcp.ts`) — gives nanoclaw agents `claude_squad` tools for session/schedule/task management via IPC
- **Host-side bridge** (`cs-bridge.ts`) — picks up MCP requests from containers and runs `cs api` commands on the host

### Internal changes

- **`config.RepoContext`** — shared abstraction for repo identification and scoped path resolution
- **Repo-explicit task functions** — `LoadTasksForRepo`, `AddTaskForRepo`, `ToggleTaskForRepo`, `DeleteTaskForRepo` accept a `*config.RepoContext`
- **Exported `schedule.WaitForReady`** — reused by the API create command

## Upstream

For installation, usage, keybindings, and general documentation, see the upstream project: [smtg-ai/claude-squad](https://github.com/smtg-ai/claude-squad)
