# CLI reference

Everything the TUI does is also scriptable. The `af sessions` and `af tasks` command groups output JSON to stdout and errors to stderr, so they compose with `jq` and shell scripts. Pass `--json` to wrap output in a `{data, error}` envelope for structured error handling. The TUI and the CLI share the same state — you can mix them freely.

Run `af <command> --help` for the authoritative flag list of any command.

## `af` — the TUI

```bash
cd your-project    # must be a git repo
af                 # launch the TUI
```

| Flag | Description |
|------|-------------|
| `-p`, `--program` | Agent to run in new sessions, one of `claude`, `codex`, `aider`, `gemini`. To pass custom paths or flags, use `program_overrides` in the config instead — see [configuration.md](configuration.md#choosing-the-agent). |
| `-y`, `--autoyes` | Experimental: automatically accept agent prompts in all sessions. |

## `af sessions`

All subcommands accept `--repo <path>` to target a repository other than the current directory, and `--json` to wrap output in the shared envelope.

```bash
af sessions list                                          # list sessions in the repo
af sessions get <title>                                   # fetch one session
af sessions create --name <title> [--prompt "..."] [--program <agent>] [--here]
af sessions send-prompt <title> "..."                     # append a prompt to a session
af sessions send-prompt <title> "..." --create            # send-or-create
af sessions tab-create <title> --command "<cmd>"          # spawn a process tab in the session's worktree
af sessions tab-delete <title> --name <tab>               # delete a single tab (the daemon won't respawn it)
af sessions preview <title>                               # snapshot the session's pane
af sessions attach <title>                                # attach interactively (foreground)
af sessions whoami                                        # report the session this shell is inside
af sessions kill <title>                                  # kill the session, clean up its worktree
af sessions archive <title>                               # tmux down + worktree moved out; restartable
af sessions restore <title>                               # bring an archived session back
```

Flags:

- `create`: `--name` (required), `--prompt` (initial prompt to send), `--program` (agent enum, defaults to the configured `default_program`). `--here` (alias `--in-place`) attaches the session to the repo's **existing working tree at its current branch** instead of cutting a new worktree+branch: the agent runs in the repo root, no branch is created, and killing the session never removes the working tree or branch. Requires a git repository (the current directory, or `--repo`); incompatible with remote sessions. The title `root` (any casing) is reserved for the daemon-managed root agent — see the `root_agents` key in [configuration.md](configuration.md#root-agents-always-ensured).
- `send-prompt`: `--create` auto-creates the session if it doesn't exist; `--program` picks the agent when creating.
- `tab-create`: `--command` (required) is run in the session's git worktree as a new tab; `--name` sets the tab's display name (defaults to the command's basename, auto-suffixed `-2`, `-3`, … on collision). The resolved tab name is printed as `{"name": "..."}` so scripts/agents can address it. The tab persists and reconnects across a daemon/`af` restart like every other tab. Refused once a session already holds 9 tabs. **Not available for remote sessions:** they have no local worktree and the hook protocol can't run arbitrary commands — a remote session's only terminal tab comes from `remote_hooks.terminal_cmd` (see [remote-hooks.md](remote-hooks.md)).
- `tab-delete`: the counterpart of `tab-create` — `--name` (required) selects the tab to delete. The tab is removed from the daemon's session state and its tmux window is killed; the removal is persistent (the daemon won't respawn it, and it doesn't return on restart). The deleted tab's name is printed as `{"name": "..."}`. The agent tab can't be deleted — use `af sessions kill` to tear down the whole session. Targeting a missing tab or session is an error. Not available for remote sessions (their tabs are fixed by `remote_hooks` config).
- `archive`: tears down the session's tmux and **moves its git worktree** out to the global archive directory (`<AGENT_FACTORY_HOME>/archived/<repoID>/<title>/`), preserving the branch and any uncommitted changes. The session is not deleted — it becomes a quiescent **archived** row that survives restarts and is never auto-restored. Prints `{"ok": true, "title": "...", "archived_path": "..."}`. Not available for remote or in-place (`--here`) sessions (they don't own a relocatable worktree). Bring it back with `restore`.
- `restore`: the counterpart of `archive` — moves the worktree back next to the repo, re-registers it, re-spawns **only the agent** (shell/process tabs are not restored), and marks the session running. Prints `{"ok": true, "title": "...", "worktree_path": "..."}`. Fails if the session isn't archived, or if its origin repo is gone (the archived worktree is left intact for manual recovery). Honors `--repo` like `kill`.

## `af tasks`

Tasks deliver a prompt to an agent automatically — on a cron schedule or whenever a long-running watch script emits a stdout line. Full semantics (trigger × delivery matrix, watch-script contract, status model) live in [tasks.md](tasks.md). All subcommands accept `--repo <path>` and `--json`.

```bash
af tasks list
af tasks add --name <n> --prompt <p> --cron "0 9 * * *" [--target-session <title>] [--program <agent>]
af tasks add --name <n> --watch-cmd <cmd> [--prompt "... {{line}} ..."] [--target-session <title>]
af tasks get <id>
af tasks update <id> [--cron ...|--watch-cmd ...] [--prompt ...] [--target-session ...] [--program <agent>] [--enabled true|false]
af tasks trigger <id>          # run a cron task immediately (cron tasks only)
af tasks remove <id>
```

Exactly one of `--cron` / `--watch-cmd` per task. On `update`, setting one trigger clears the other. `--target-session ""` explicitly reverts to create-a-session-per-run; omitting the flag leaves it untouched. `--program` accepts the same agent enum as `tasks add`; omitting it keeps the task's current program.

## `af daemon`

The background daemon hosts task cron schedules, watch-task scripts, and autoyes mode. It starts on demand whenever `af` runs and an enabled task exists; installing it as a user-level autostart unit (systemd user service on Linux, launchd agent on macOS) keeps scheduled tasks firing after reboots. See [tasks.md](tasks.md#daemon-lifecycle).

```bash
af daemon install      # register autostart at login
af daemon uninstall    # remove the autostart unit (the daemon still starts on demand)
```

## Maintenance commands

```bash
af version             # print the version and the release URL
af debug               # print the resolved config and its path
af keys                # print the effective TUI key bindings (defaults + [keys] rebinds)
af upgrade             # self-upgrade to the latest GitHub release (Linux/macOS)
af doctor              # diagnose leaked processes/sessions/temp homes and daemon health
af doctor --fix        # also apply the safe remediations
af reset               # nuclear option — see below
```

`af doctor` is read-only by default: it reports orphaned processes left behind
by dead sessions, processes pegging a CPU core inside live sessions, `af_` tmux
sessions with no backing record, abandoned temp agent-factory homes, and daemon
problems (stale socket, stale pid file, a daemon still running a replaced
binary). With `--fix` it kills orphans whose ancestry markers prove they came
from a dead Agent Factory session and removes stale temp homes, logging each
action; anything it cannot verify is reported, never touched. Exits 1 when
unresolved issues remain.

`af reset` attempts to stop the daemon (and reports honestly if it couldn't — e.g. a source-built `agent-factory --daemon` that left no PID file), kills **all** Agent Factory tmux sessions, removes **every linked git worktree (and its branch)** from each repo that has stored sessions — including worktrees you created by hand — and deletes all stored session records. Use it to recover from a corrupted state, not for day-to-day cleanup — `af sessions kill <title>` (or `D` in the TUI) removes a single session cleanly.
