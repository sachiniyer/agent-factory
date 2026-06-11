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
  "detach_keys": "ctrl-w"
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
    "delete_cmd": "./infra/delete.sh"
  }
}
```

| Field | Scope |
|-------|-------|
| `default_program`, `program_overrides` | Valid globally **and** in-repo (in-repo wins). |
| `post_worktree_commands`, `remote_hooks` | **In-repo only.** The legacy `~/.agent-factory/repos/<repoID>/config.json` location keeps working for one more release (a deprecation warning in the log points at the new file) and is shadowed whenever the in-repo file sets the same key — including by an explicit empty value like `"post_worktree_commands": []`. |
| `auto_yes`, `daemon_poll_interval`, `branch_prefix`, `detach_keys` | Global only. Setting them in-repo is rejected with an error naming the key. |

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
| `~/.agent-factory/logs/task-<id>.log` | Per-task watch-script logs. |
| `~/.config/agent-factory/agent-factory.log` | Application log (`os.UserConfigDir` on other platforms). |

Setting the `AGENT_FACTORY_HOME` environment variable relocates the `~/.agent-factory` state directory — useful for sandboxed or test setups.
