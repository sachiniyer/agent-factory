# CLI reference

Everything the TUI does is also scriptable. The `af sessions` and `af tasks` command groups output JSON to stdout and errors to stderr, so they compose with `jq` and shell scripts. The TUI and the CLI share the same state — you can mix them freely.

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

All subcommands accept `--repo <path>` to target a repository other than the current directory.

```bash
af sessions list                                          # list sessions in the repo
af sessions get <title>                                   # fetch one session
af sessions create --name <title> [--prompt "..."] [--program <agent>]
af sessions send-prompt <title> "..."                     # append a prompt to a session
af sessions send-prompt <title> "..." --create            # send-or-create
af sessions tab-create <title> --command "<cmd>"          # spawn a process tab in the session's worktree
af sessions preview <title>                               # snapshot the session's pane
af sessions attach <title>                                # attach interactively (foreground)
af sessions whoami                                        # report the session this shell is inside
af sessions kill <title>                                  # kill the session, clean up its worktree
```

Flags:

- `create`: `--name` (required), `--prompt` (initial prompt to send), `--program` (agent enum, defaults to the configured `default_program`).
- `send-prompt`: `--create` auto-creates the session if it doesn't exist; `--program` picks the agent when creating.
- `tab-create`: `--command` (required) is run in the session's git worktree as a new tab; `--name` sets the tab's display name (defaults to the command's basename, auto-suffixed `-2`, `-3`, … on collision). The resolved tab name is printed as `{"name": "..."}` so scripts/agents can address it. The tab persists and reconnects across a daemon/`af` restart like every other tab. Not available for remote sessions (no local worktree); refused once a session already holds 9 tabs.

## `af tasks`

Tasks deliver a prompt to an agent automatically — on a cron schedule or whenever a long-running watch script emits a stdout line. Full semantics (trigger × delivery matrix, watch-script contract, status model) live in [tasks.md](tasks.md). All subcommands accept `--repo <path>`.

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
af upgrade             # self-upgrade to the latest GitHub release (Linux/macOS)
af reset               # nuclear option — see below
```

`af reset` stops the daemon, kills **all** Agent Factory tmux sessions, removes **every linked git worktree (and its branch)** from each repo that has stored sessions — including worktrees you created by hand — and deletes all stored session records. Use it to recover from a corrupted state, not for day-to-day cleanup — `af sessions kill <title>` (or `D` in the TUI) removes a single session cleanly.
