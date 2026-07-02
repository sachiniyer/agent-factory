# Tasks

A task delivers a prompt to an AI agent session automatically. Every task has exactly one **trigger** — a cron schedule (`cron_expr`) or a long-running watch script (`watch_cmd`) — and one **delivery mode**: create a fresh session per fire, or send the prompt into an existing session (`target_session`).

Tasks are hosted by the agent-factory daemon, which starts automatically whenever `af` runs and an enabled task exists. There are no per-task OS scheduler units — see [Daemon lifecycle](#daemon-lifecycle) and [Migration notes](#migration-notes).

## Trigger × delivery matrix

|  | `target_session` empty | `target_session` set |
|---|---|---|
| **`cron_expr`** | create a session per fire | send `prompt` into the session at schedule |
| **`watch_cmd`** | each stdout line → create a session with the rendered prompt | each stdout line → send the rendered prompt into the session |

When `target_session` is set, the session title is looked up in the task's own repo (a same-titled session in an unrelated repo can never receive the prompt). If no session with that title exists at fire time, it is **auto-created** with the task's `project_path`/`program` and the rendered prompt as its initial prompt — same behavior as `af sessions send-prompt --create`.

## Task fields

Tasks live in `~/.agent-factory/tasks.json`. Manage them via `af tasks` (JSON CLI) or the TUI Tasks pane (`s` to create, `S` to list).

| Field | Meaning |
|---|---|
| `id` | 8-char hex identifier, generated on add |
| `name` | Display name; also seeds created-session titles |
| `prompt` | Prompt to deliver. Required for cron tasks. Optional for watch tasks: empty delivers the raw emitted line; otherwise every `{{line}}` occurrence is replaced with the line |
| `cron_expr` | Time trigger — 5-field cron expression (exactly one of `cron_expr` / `watch_cmd`) |
| `watch_cmd` | Event trigger — long-running command; each stdout line fires the task (exactly one of `cron_expr` / `watch_cmd`) |
| `target_session` | Deliver into this session by title (auto-created if missing). Empty = create a new session per fire |
| `project_path` | Repo the task operates on; also the watch script's working directory |
| `program` | Agent to run (`claude`, `aider`, …). Empty = configured `default_program` |
| `enabled` | Disabled tasks never fire; their watch script is stopped |
| `last_run_at` / `last_run_status` | Set by the daemon: `started` (session created), `sent` (prompt delivered into a session), and for watch tasks `stopped` / `errored` (see below) |

A task with both triggers set is always invalid. An enabled task must have exactly one; a disabled task with neither is tolerated as a draft. An enabled cron task must carry a non-empty prompt — there is no event line to fall back to. Watch tasks are exempt (empty prompt defaults to the emitted line). Disabled drafts are tolerated regardless of prompt.

## Cron tasks

`cron_expr` is a standard 5-field expression (`minute hour day-of-month month day-of-week`) with Vixie semantics, including the DOM/DOW OR rule when both fields are restricted. The daemon evaluates expressions in-process — what you write is exactly what is evaluated, with no conversion to OS timer formats.

```bash
af tasks add --name "Daily triage" --prompt "Triage open issues" --cron "0 9 * * 1-5"
af tasks trigger <id>     # run a cron task immediately
```

`af tasks trigger` (and the TUI `r` key) work for cron tasks only — a watch task has no event line to render its prompt with, so manual triggers are refused.

## Watch tasks

A watch task keeps a script running and turns its output into events:

```bash
af tasks add --name "gh-issues" --watch-cmd "./watch-issues.sh" \
  --prompt "Triage: {{line}}" --target-session captain
```

### Script contract

- The script is **long-lived**. The daemon runs it via `$SHELL -c <watch_cmd>` with the task's `project_path` as the working directory, and keeps it running while the task is enabled.
- **Each newline-terminated stdout line is one event.** Lines over 64KB are truncated to the cap (the remainder is discarded with a logged note). Unterminated trailing output at exit is not an event. Silence is fine — a quiet watcher is healthy; there is no output timeout.
- **stderr** appends to `~/.agent-factory/logs/task-<id>.log`, size-capped by the same `log_max_size_mb`/`log_max_backups` rotation as the main log. Use it for all logging — anything on stdout becomes an event.
- **Environment**: the script receives `AF_TASK_ID`, `AF_TASK_NAME`, and `AF_PROJECT_PATH` on top of the daemon's environment.
- **Exit 0 = intentional stop.** The task's status becomes `stopped` and the script is not restarted until the next daemon start, task edit, or re-enable.
- **Non-zero exit = failure.** The script is restarted with exponential backoff, 1s doubling to a 5-minute cap. A run that stays healthy for 10 minutes resets the backoff.
- **Crash-loop breaker**: 5 or more non-zero exits within 10 minutes set the status to `errored` and stop restarts. Re-arm by editing the task, toggling it, or restarting the daemon.
- **Rate limit**: at most 10 events per minute per task. Excess events are dropped — never silently: a warning is logged with a running drop counter.
- **Prompt rendering**: an empty `prompt` delivers the raw line; otherwise `{{line}}` is substituted. An event whose rendered prompt is empty is dropped with an error log.
- **Ordering**: deliveries are serialized per task in emission order. A slow delivery backpressures the script's stdout rather than reordering events.
- **Process tree**: each script runs in its own process group. On stop the group gets SIGTERM, then SIGKILL after 5 seconds — backgrounded children do not outlive the watcher. Scripts should treat SIGTERM as "clean up and exit".
- **No replay**: events are not persisted across daemon restarts. Scripts that poll should track their own cursor (see `examples/tasks/gh-issue-poll.sh`).

Edits to delivery fields (`prompt`, `target_session`, `program`) apply from the next event without restarting the script; edits to `watch_cmd`, `project_path`, or `name` restart it.

### Watch-task status

The TUI Tasks pane shows each watch task's supervision state, derived from the persisted fields:

- **watching** — enabled, script supervised by the daemon
- **stopped** — script exited 0 (or the task is disabled)
- **errored** — crash-loop breaker tripped; check `~/.agent-factory/logs/task-<id>.log`

## Daemon lifecycle

The daemon is the single scheduler host: it evaluates cron expressions, supervises watch scripts, and serves autoyes.

- Every `af` invocation ensures the daemon is running whenever an enabled task exists, and the daemon keeps running after the TUI exits.
- To keep tasks firing across **reboots** without opening `af`, register the user-level autostart unit (a systemd user service on Linux, a launchd agent on macOS):

```bash
af daemon install      # register autostart at login
af daemon uninstall    # remove it (the daemon still starts on demand)
```

- Task edits made through `af tasks` or the TUI take effect live — the daemon is poked to reload `tasks.json`. If the poke fails, the change applies at the next daemon start.

## Migration notes

Versions before #791 installed one systemd timer / launchd plist **per task** (`agent-factory-task-*`, `agent-factory-sched-*` units). That conversion layer is gone:

- The daemon evaluates cron expressions directly; the autostart unit registered by `af daemon install` is the only OS-level unit left.
- On first start, the daemon **sweeps** any leftover per-task units from older versions (disabled, deleted, logged) so tasks cannot double-fire.
- `tasks.json` is unchanged — existing tasks work as-is, and the new `watch_cmd` / `target_session` fields are optional extensions.

## CLI quick reference

```bash
af tasks list
af tasks add --name <n> --prompt <p> --cron "0 9 * * *" [--target-session <title>] [--program <agent>]
af tasks add --name <n> --watch-cmd <cmd> [--prompt "… {{line}} …"] [--target-session <title>]
af tasks get <id>
af tasks update <id> [--cron …|--watch-cmd …] [--prompt …] [--target-session …] [--program <agent>] [--enabled true|false]
af tasks trigger <id>          # cron tasks only
af tasks remove <id>
```

On `update`, setting one trigger clears the other (switching watch→cron requires a prompt). `--target-session ""` explicitly reverts to create-per-run; omitting the flag leaves it untouched. `--program` accepts the same agent enum as `tasks add`; omitting it keeps the task's current program.

## Examples

See `examples/tasks/` for runnable watch-script skeletons: a log tailer (`log-tail.sh`) and a GitHub issue poller (`gh-issue-poll.sh`).
