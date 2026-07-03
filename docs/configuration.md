# Configuration

Agent Factory reads two config files, merged field by field:

1. **Global** — `~/.agent-factory/config.json`: your personal defaults, applied everywhere.
2. **In-repo** — `<repo-root>/.agent-factory/config.json`: checked into a repository, applied whenever `af` runs in that repo.

Precedence is **app defaults → global config → in-repo config**: an in-repo field overrides the global value only when it is set, and `program_overrides` merges per key (an in-repo entry wins for that agent; global entries for other agents still apply).

## Global config

`~/.agent-factory/config.json`:

```json
{
  "default_program": "claude",
  "program_overrides": {
    "claude": "/home/me/.local/bin/claude --dangerously-skip-permissions"
  },
  "auto_yes": false,
  "daemon_poll_interval": 1000,
  "branch_prefix": "username/",
  "detach_keys": "ctrl-w",
  "log_max_size_mb": 50,
  "log_max_backups": 2,
  "update_channel": "stable"
}
```

| Field | Description |
|-------|-------------|
| `default_program` | Default agent enum. Must be one of `claude`, `codex`, `aider`, `gemini`. |
| `program_overrides` | Optional map from agent enum to the full command string used when launching that agent. Use this to pin a path or pass flags (e.g. `--dangerously-skip-permissions`). Keys must be one of `claude`, `codex`, `aider`, `gemini`. |
| `auto_yes` | Auto-accept agent prompts (experimental). |
| `daemon_poll_interval` | Daemon polling interval in ms. |
| `branch_prefix` | Prefix for worktree branches (defaults to `username/`). |
| `detach_keys` | Key combination that detaches from an attached session (defaults to `ctrl-w`). |
| `log_max_size_mb` | Size cap in MB for `agent-factory.log` and the per-task watch-script logs before they are rotated (defaults to 50). Must be positive. |
| `log_max_backups` | How many rotated logs (`agent-factory.log.1`, `.2`, ...) to keep per log file; older ones are deleted (defaults to 2). `0` keeps none. |
| `update_channel` | Release channel that auto-update and `af upgrade` follow: `stable` (default) tracks manual `1.x.y` releases only; `preview` opts into the automatic `1.x.y-preview-z` prereleases cut every 3 hours. Any other value falls back to `stable` with a warning. See [release-process.md](release-process.md). |
| `root_agents` | Opt-in map of repositories that get an always-ensured `root` agent (default: none). See [Root agents](#root-agents-always-ensured). |

### Root agents (always-ensured)

`root_agents` opts a repository into a **root agent**: a reserved session titled `root` that the daemon guarantees is always running. It is created **in-place** at the repo root (the `af sessions create --here` shape — no worktree or branch is created; killing it never touches your working tree or branch), and if its tmux session dies or vanishes, the daemon re-creates it automatically.

```json
{
  "root_agents": {
    "/home/me/myrepo": {},
    "~/work/other": { "program": "claude --model opus", "auto_yes": false }
  }
}
```

Keys are repository paths (a leading `~` expands to your home directory). Per-repo profile fields:

| Field | Description |
|-------|-------------|
| `program` | Command the root session runs. Unlike `default_program` this may be a full command string; a bare agent enum name (e.g. `claude`) still resolves through `program_overrides`. Default: the repo's resolved `claude` command with `--dangerously-skip-permissions` ensured — the root agent is meant to operate autonomously. |
| `auto_yes` | Auto-accept the agent's prompts. Defaults to **true** for root agents (unlike the global `auto_yes`). |

Behavior and guarantees:

- **Strictly opt-in and global-only.** Nothing gets a root agent unless you add it here, in *your* `~/.agent-factory/config.json`. The key is rejected in in-repo configs, so cloning a repository can never opt your machine into an always-on agent.
- **Adopt, never clobber.** If a session titled `root` already exists and is alive — however it was created — the daemon leaves it completely alone. Only a `root` whose tmux has died (status `Dead`) or that is missing entirely is (re-)created.
- **The name `root` is reserved.** Normal session creation (TUI, `af sessions create`, the API, task spawns) rejects the title `root` (case-insensitively); auto-derived titles skip it.
- **An explicit kill is respected.** If you kill the `root` session (TUI `D`, `af sessions kill root`), the daemon does not resurrect it until the next daemon restart, at which point the configured state is re-asserted.
- **Failures back off and cap.** If ensuring a root repeatedly fails (e.g. the configured path is not a git repository), the daemon retries with exponential backoff and gives up for that repo after 6 consecutive failures until it restarts, logging each outcome to the application log.
- Changes to `root_agents` are picked up on the next **daemon restart**.

Because the default profile skips permission prompts and auto-accepts, only opt in repositories where you are comfortable with a fully autonomous agent running at the repo root.

### Choosing the agent

Override the agent for new sessions with `-p`:

```bash
af -p aider
```

`-p` and the per-task `program` field both accept a bare agent enum only (`claude`, `codex`, `aider`, `gemini`). To pass a custom path or flags for an agent, set `program_overrides.<agent>` in your config — every session that launches that agent will use the override.

## In-repo config

A repository can carry its own configuration in `<repo-root>/.agent-factory/config.json`, so every clone gets the same setup:

```json
{
  "default_program": "codex",
  "program_overrides": {
    "codex": "/usr/local/bin/codex --profile work"
  },
  "post_worktree_commands": ["npm install"],
  "remote_hooks": {
    "launch_cmd": "./infra/launch.sh",
    "list_cmd": "./infra/list.sh",
    "attach_cmd": "./infra/attach.sh",
    "delete_cmd": "./infra/delete.sh",
    "terminal_cmd": "./infra/terminal.sh"
  }
}
```

| Field | Scope |
|-------|-------|
| `default_program`, `program_overrides` | Valid globally **and** in-repo (in-repo wins). |
| `post_worktree_commands`, `remote_hooks` | **In-repo only.** The legacy `~/.agent-factory/repos/<repoID>/config.json` location keeps working for one more release (a deprecation warning in the log points at the new file) and is shadowed whenever the in-repo file sets the same key — including by an explicit empty value like `"post_worktree_commands": []`. |
| `auto_yes`, `daemon_poll_interval`, `branch_prefix`, `detach_keys`, `log_max_size_mb`, `log_max_backups`, `update_channel` | Global only. Setting them in-repo is rejected with an error naming the key. |

`post_worktree_commands` are shell commands run after each new worktree is created (e.g. `npm install`, `make build`) — they can also be edited from the TUI via the `H` (worktree hooks) key. `remote_hooks` configures a remote-machine backend; see [remote-hooks.md](remote-hooks.md) for the script protocol.

### Relative hook paths

Relative `remote_hooks` paths (like `./infra/launch.sh` above) resolve against the repository root — the repo whose `.agent-factory/config.json` was loaded; for sessions in linked worktrees that is the main repository root — so checked-in hook scripts work no matter what the working directory of `af` or its daemon is. Bare names without a path separator (e.g. `bash`) keep normal `$PATH` lookup. See [remote-hooks.md](remote-hooks.md#command-path-resolution) for the full rules.

### Trust

An in-repo config executes what it configures: `post_worktree_commands` run after each worktree is created, and `remote_hooks` and `program_overrides` values are invoked as shell commands. Cloning a repository and running `af` in it implies trusting that repo's `.agent-factory/config.json`. The first time a config carrying such fields loads (and whenever its content changes), `af` records one log line naming the fields and the file's content hash.

## Where state lives

All data (sessions, tasks) is scoped to the current git repository — the TUI shows only what's relevant to the repo you're in.

| Path | Contents |
|------|----------|
| `~/.agent-factory/config.json` | Global config. |
| `~/.agent-factory/instances/<repoID>/instances.json` | Persisted sessions, per repo. |
| `~/.agent-factory/tasks.json` | Tasks (see [tasks.md](tasks.md)). |
| `~/.agent-factory/logs/task-<id>.log` | Per-task watch-script logs. Rotated with the same `log_max_size_mb`/`log_max_backups` policy as the application log (`task-<id>.log.1`, `.2`). |
| `~/.config/agent-factory/agent-factory.log` | Application log (`os.UserConfigDir` on other platforms). Rotated once it exceeds `log_max_size_mb` (default 50 MB); the most recent `log_max_backups` rotations (default 2) are kept as `agent-factory.log.1`, `.2`. |

Setting the `AGENT_FACTORY_HOME` environment variable relocates the `~/.agent-factory` state directory — useful for sandboxed or test setups. When it is set, the application log also moves into that directory (`$AGENT_FACTORY_HOME/agent-factory.log`) so a relocated home is fully self-contained.
