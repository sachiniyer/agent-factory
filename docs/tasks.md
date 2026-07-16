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

Tasks live in `~/.agent-factory/tasks.json`. Manage them via `af tasks` (JSON CLI) or the TUI Tasks pane (`m` to open, `n` to create).

| Field | Meaning |
|---|---|
| `id` | 8-char hex identifier, generated on add |
| `name` | Display name; also seeds created-session titles |
| `prompt` | Prompt to deliver. Required for cron tasks. Optional for watch tasks: empty delivers the raw emitted line; otherwise every `{{line}}` occurrence is replaced with the line |
| `cron_expr` | Time trigger — 5-field cron expression (exactly one of `cron_expr` / `watch_cmd`) |
| `watch_cmd` | Event trigger — long-running command; each stdout line fires the task (exactly one of `cron_expr` / `watch_cmd`) |
| `target_session` | Deliver into this session by title (auto-created if missing). Empty = create a new session per fire |
| `max_concurrent_runs` | Cap on how many sessions this watch task may have in flight at once. `0` (the default) = unlimited. Excess events are queued in order and delivered as sessions finish — never dropped. Watch tasks with an empty `target_session` only (see [Limiting concurrent sessions](#limiting-concurrent-sessions)) |
| `project_path` | Repo the task operates on; also the watch script's working directory |
| `program` | Agent to run (`claude`, `codex`, `aider`, `gemini`, `amp`). Empty = configured `default_program` |
| `enabled` | Disabled tasks never fire; their watch script is stopped |
| `last_run_at` / `last_run_status` | Set by the daemon: `started` (session created), `sent` (prompt delivered into a session), `parked: usage limit` (the session hit a plan usage-limit wall at startup and is parked, not failed — see [usage-limits.md](usage-limits.md#task-runs-park-dont-fail)), and for watch tasks `stopped` / `errored` (see below) |

A task with both triggers set is always invalid. An enabled task must have exactly one; a disabled task with neither is tolerated as a draft. An enabled cron task must carry a non-empty prompt — there is no event line to fall back to. Watch tasks are exempt (empty prompt defaults to the emitted line). Disabled drafts are tolerated regardless of prompt.

`max_concurrent_runs` is rejected on a cron task or one with a `target_session` set, rather than stored and ignored: overlapping cron fires already coalesce, and deliveries into a single target session already serialize, so there would be nothing for the cap to bound.

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
- **Rate limit**: at most 10 events per minute per task. Excess events are dropped — never silently: a warning is logged with a running drop counter. Rate-dropped events are not queued for replay (the limit is protective policy against a chatty script, not an outage signal). This is a limit on the event *rate*, and is independent of `max_concurrent_runs`, which bounds in-flight *sessions* and queues rather than drops — the two are never the binding constraint at the same time (see [Limiting concurrent sessions](#limiting-concurrent-sessions)).
- **Prompt rendering**: an empty `prompt` delivers the raw line; otherwise `{{line}}` is substituted. An event whose rendered prompt is empty is dropped with an error log.
- **Ordering**: deliveries are serialized per task in emission order. A slow delivery backpressures the script's stdout rather than reordering events, and replayed events (below) land before newer live ones.
- **Process tree**: each script runs in its own process group. On stop the group gets SIGTERM, then SIGKILL after 5 seconds — backgrounded children do not outlive the watcher. Scripts should treat SIGTERM as "clean up and exit".
- **Delivery failures are queued and replayed**: an event whose delivery fails — the target session unreachable, e.g. during a tmux outage — is appended to a durable per-task queue under `~/.agent-factory/events/` instead of dropped, and replayed **in emission order, before newer live events**, once deliveries succeed again (rate-limited by the same 10/min window). The backlog survives daemon restarts and reloads. Semantics and bounds:
  - **At-least-once**: a daemon crash mid-replay redelivers at most one event. Prompts should tolerate a rare duplicate.
  - **Bounds**: at most 500 events / 256KB queued per task — overflow drops the *oldest* with a logged count; events older than **72h** are expired at replay time, also logged. Sources worth watching re-emit on their next poll, so scripts that poll should still track their own cursor (see `examples/tasks/gh-issue-poll.sh`).
  - **Disabled vs deleted**: a disabled task keeps its backlog and replays it on re-enable; a deleted task's queue is removed.

Edits to delivery fields (`prompt`, `target_session`, `program`) apply from the next event without restarting the script; edits to `watch_cmd`, `project_path`, or `name` restart it.

### Limiting concurrent sessions

A watch task that creates a session per event has no bound on how many of those sessions run at once — a burst of events means a burst of agents, each starting up, running its `post_worktree_commands`, and working in parallel. `max_concurrent_runs` bounds it:

```bash
af tasks add --name "DLQ triage" \
  --watch-cmd ./poll-dlq.sh --prompt "Triage: {{line}}" \
  --max-concurrent-runs 3
```

The default is `0` — unlimited, which is exactly the historical behavior. A cap is opt-in; existing tasks are unaffected.

How it behaves:

- **A session counts against the cap from the moment its create begins** — before the agent is up, and while its `post_worktree_commands` are still running. This is the window an external monitor cannot see, and why one that lists sessions and matches titles overshoots its own cap.
- **Events over the cap are queued, never dropped on the admission path.** They land in the same durable per-task queue as a failed delivery (above), replay **in emission order**, and survive daemon restarts. They share that queue's retention bounds: a task that stays at its cap past the 72h age limit, or accumulates more than the 500-event / 256KB backlog, expires or drops its *oldest* parked events exactly as the delivery-failure path does — the bound exists so a permanently-saturated cap cannot grow the queue without limit. In normal use sessions finish and the backlog drains long before that.
- **A slot frees when the session goes idle** — the agent finished the work the event asked for — or when it is lost/dead/archived. It does *not* wait for you to archive the session: a cap that only freed on archive would stall the task until a human intervened, and the backlog would age out. It also does not wait for `post_worktree_commands` to finish, so a hung hook cannot wedge the task permanently.
- **The cap is scoped to the task and its repo**, keyed on the task id recorded on each session it spawns — not on a session-title prefix.
- **A parked task is healthy**, not failing: it logs quietly and never raises the delivery-failure alarm.

Pick the cap from what a run actually costs. If each event triggers an expensive post-worktree build, a low cap keeps the machine usable; if runs are cheap, leave it unlimited and let the 10/min rate limit be the only bound.

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

- Task edits made through `af tasks` or the TUI go through the daemon: writes persist and the daemon re-arms its schedules atomically in one RPC. The daemon is the sole task writer; the TUI sends field-level patches (`UpdateTask(id, patch)`) so a single-field edit cannot clobber a concurrent edit another client made to a different field (#1700).

## Migration notes

Versions before #791 installed one systemd timer / launchd plist **per task** (`agent-factory-task-*`, `agent-factory-sched-*` units). That conversion layer is gone:

- The daemon evaluates cron expressions directly; the autostart unit registered by `af daemon install` is the only OS-level unit left.
- On first start, the daemon **sweeps** any leftover per-task units from older versions (disabled, deleted, logged) so tasks cannot double-fire.
- `tasks.json` is unchanged — existing tasks work as-is, and the new `watch_cmd` / `target_session` / `max_concurrent_runs` fields are optional extensions.

## CLI quick reference

```bash
af tasks list
af tasks add --name <n> --prompt <p> --cron "0 9 * * *" [--target-session <title>] [--program <agent>]
af tasks add --name <n> --watch-cmd <cmd> [--prompt "… {{line}} …"] [--target-session <title>] [--max-concurrent-runs <n>]
af tasks get <id>
af tasks update <id> [--cron …|--watch-cmd …] [--prompt …] [--target-session …] [--max-concurrent-runs <n>] [--program <agent>] [--enabled true|false]
af tasks trigger <id>          # cron tasks only
af tasks remove <id>
```

On `update`, setting one trigger clears the other (switching watch→cron requires a prompt when the resulting cron task is enabled). `--target-session ""` explicitly reverts to create-per-run; omitting the flag leaves it untouched. `--max-concurrent-runs 0` explicitly reverts to unlimited; omitting the flag leaves the current cap untouched. `--program` accepts the same agent enum as `tasks add`; omitting it keeps the task's current program.

## Examples

See `examples/tasks/` for runnable watch-script skeletons: a log tailer (`log-tail.sh`) and a GitHub issue poller (`gh-issue-poll.sh`).
