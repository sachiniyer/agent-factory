# siyer-claude-squad

Fork of [claude-squad](https://github.com/smtg-ai/claude-squad) with per-repo scoping, task management, a programmatic API, and [MicroClaw](https://microclaw.ai) integration.

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

Supports `--repo` and `--repo-id` flags for repo scoping from outside git directories.

### MicroClaw integration

Bidirectional bridge to a running [MicroClaw](https://microclaw.ai) instance. MicroClaw runs directly on the host (no Docker containers), so agents have full bash and filesystem access.

- **MicroClaw tab** — 4th tab in the TUI (press `tab` to cycle). Shows chat history from microclaw's SQLite database with scrolling.
- **Send messages** — Press `m` to compose a message to microclaw. Messages include repo metadata and instructions for the agent to use `cs api` CLI commands directly.
- **Direct CLI access** — MicroClaw agents use `cs api` commands directly for session/task management (no MCP bridge needed).
- **Configuration** — Set `MICROCLAW_DIR` env var (defaults to `~/.microclaw`). The tab only appears when a valid microclaw installation is detected.

```bash
MICROCLAW_DIR=~/.microclaw cs
```

### Internal changes

- **`config.RepoContext`** — shared abstraction for repo identification and scoped path resolution
- **Repo-explicit task functions** — `LoadTasksForRepo`, `AddTaskForRepo`, `ToggleTaskForRepo`, `DeleteTaskForRepo` accept a `*config.RepoContext`
- **Exported `schedule.WaitForReady`** — reused by the API create command

## Upstream

For installation, usage, keybindings, and general documentation, see the upstream project: [smtg-ai/claude-squad](https://github.com/smtg-ai/claude-squad)
