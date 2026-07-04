# `af` CLI reference

A complete reference of the current `af` command-line surface: every command and
subcommand, its flags, and the JSON (or text) shape it emits today.

This documents **existing behavior**. For narrative usage see
[cli.md](cli.md); for task semantics see [tasks.md](tasks.md); for configuration
see [configuration.md](configuration.md). Run `af <command> --help` for the
authoritative flag list of any command.

## Output conventions

The `af sessions` and `af tasks` command groups emit **JSON to stdout** and
**errors to stderr**, so they compose with `jq` and shell scripts. All other
commands (`daemon`, `doctor`, `version`, `debug`, `keys`, `upgrade`, `reset`)
emit human-readable text.

By default, the JSON commands print the **bare payload** documented below. Every
`af sessions` / `af tasks` subcommand also accepts an opt-in **`--json`** flag
that instead wraps output in a shared envelope — see
[JSON envelope (`--json`)](#json-envelope---json). The flag defaults off, so
existing scripts see byte-identical output.

**Errors.** On failure a JSON command writes `{"error":"<message>"}` to stderr
(compact, single line) and exits non-zero; cobra additionally prints an
`Error: …` line and usage. With `--json`, the stderr body is the envelope form
`{"data":null,"error":{"message":"<message>"}}` instead. The process exit code
is identical in both modes.

---

## `af` — launch the TUI

```bash
cd your-project    # must be a git repo
af                 # launch the terminal UI
```

| Flag | Description |
|------|-------------|
| `-p`, `--program` | Agent to run in new sessions: one of `claude`, `codex`, `aider`, `gemini`. For custom paths/flags use `program_overrides` in the config — see [configuration.md](configuration.md#choosing-the-agent). |
| `-y`, `--autoyes` | Experimental: automatically accept agent prompts in all sessions. |

(`--daemon` exists but is hidden — it is the internal entry point the daemon
supervisor uses, not a user-facing command.)

---

## `af sessions`

Manage sessions. All subcommands accept **`--repo <path>`** to target a
repository other than the current directory, and **`--json`** for envelope
output.

The read commands **`list`**, **`get`**, and **`whoami`** reflect the daemon's
authoritative in-memory state — the same live view the TUI shows — when a daemon
is running, so their output no longer lags behind the on-disk `instances.json`.
They never start a daemon: with none running (a script, CI, or a fresh shell)
they fall back to reading `instances.json` off disk, so they keep working with
no daemon present. Both sources return the same shape, sorted identically by
`(repo, title)`.

### `af sessions list`

List sessions in scope (the current/`--repo` repo, or every repo when run
outside one). Emits a JSON array of session objects (empty `[]` when none).

```json
[
  {
    "title": "fix-login",
    "path": "/home/you/.agent-factory/worktrees/…",
    "branch": "af/fix-login",
    "status": 1,
    "height": 0,
    "width": 0,
    "created_at": "2026-07-04T12:00:00Z",
    "updated_at": "2026-07-04T12:05:00Z",
    "auto_yes": false,
    "program": "claude",
    "tmux_name": "af_fix-login",
    "tabs": [ { "name": "agent", "kind": 0, "tmux_name": "af_fix-login" } ],
    "worktree": {
      "repo_path": "/home/you/project",
      "worktree_path": "/home/you/.agent-factory/worktrees/…",
      "session_name": "fix-login",
      "branch_name": "af/fix-login",
      "base_commit_sha": "…"
    }
  }
]
```

`status` is a numeric enum (`Running`, `Ready`, `Loading`, `Paused`, `Lost`,
`Dead`, …). Fields tagged `omitempty` in storage (`user_killed`, `tmux_name`,
`tabs`, `pr_info`, `backend_type`, `remote_meta`, worktree
`external_worktree` / `branch_created_by_us`) appear only when set. A corrupted
`instances.json` in any repo is reported as an error rather than silently
dropped (#730).

### `af sessions get <title>`

Fetch a single session by title. Emits one session object (same shape as a
`list` element). Errors if the title matches no session.

### `af sessions create`

Create a new session running an agent in its own git worktree. Emits the created
session object.

| Flag | Description |
|------|-------------|
| `--name` | **Required.** Session title. `root` (any casing) is reserved for the daemon-managed root agent. |
| `--prompt` | Initial prompt to send to the agent. |
| `--program` | Agent enum (`claude`/`codex`/`aider`/`gemini`); defaults to the configured `default_program`. |
| `--here` | Attach to the repo's **existing working tree at its current branch** — no new worktree/branch; killing the session preserves both. Requires a git repository. |
| `--in-place` | Alias for `--here`. |

### `af sessions send-prompt <title> <prompt>`

Send a prompt to an existing session. Emits `{"ok": true}` on success.

| Flag | Description |
|------|-------------|
| `--create` | Auto-create the session if it doesn't exist (routes through the daemon's serialized create-or-send path). |
| `--program` | Agent to run when `--create` makes a new session. |
| `--all` | Broadcast one prompt to every live session in scope. Takes a single positional (the prompt). |
| `--all-repos` | With `--all`, broadcast across every repo instead of only the current/`--repo` one (mutually exclusive with `--repo`). |
| `--include-root` | With `--all`, also deliver to the reserved root session (excluded by default). |

Broadcast (`--all`) emits a summary object and exits 0 even when some sessions
fail, so scripts can inspect per-session results:

```json
{
  "prompt": "run the tests",
  "scope": "repo:<id>",
  "delivered": 2,
  "failed": 0,
  "skipped": 1,
  "results": [
    { "title": "fix-login", "repo_id": "<id>", "status": "delivered" },
    { "title": "root", "repo_id": "<id>", "status": "skipped",
      "reason": "reserved root session excluded; pass --include-root to broadcast to it" }
  ]
}
```

Each result's `status` is `delivered`, `failed`, or `skipped`; `error` carries a
failure reason and `reason` explains an intentional skip.

### `af sessions tab-create <title>`

Spawn a process tab that runs a command in the session's git worktree. Emits the
resolved tab name: `{"name": "<tab>"}`.

| Flag | Description |
|------|-------------|
| `--command` | **Required.** Command to run in the session's worktree as a new tab. |
| `--name` | Tab display name; defaults to the command basename, auto-suffixed `-2`, `-3`, … on collision. |

Refused once a session already holds 9 tabs. Not available for remote sessions.

### `af sessions tab-delete <title>`

Delete a single named tab (the counterpart of `tab-create`). Emits the deleted
tab's name: `{"name": "<tab>"}`.

| Flag | Description |
|------|-------------|
| `--name` | **Required.** Name of the tab to delete. |

The removal is persistent (the daemon won't respawn it). The agent tab can't be
deleted — use `kill` to tear down the whole session. Not available for remote
sessions.

### `af sessions preview <title>`

Snapshot the session's terminal pane. Emits `{"title": "<title>", "content":
"<pane text>"}`.

### `af sessions attach <title>`

Attach interactively to the session's tmux terminal (foreground). Detach with
the configured detach key (default `Ctrl-b d`). **Interactive — emits no JSON.**

### `af sessions kill <title>`

Kill the session and clean up its worktree. Emits `{"ok": true}`.

### `af sessions archive <title>`

Archive a session: tear down its tmux and move its git worktree out to the global
archive directory (`<AGENT_FACTORY_HOME>/archived/<repoID>/<title>/`), preserving
the branch and any uncommitted changes. The session is not deleted — it becomes a
quiescent **archived** row that survives restarts and is never auto-restored.
Emits `{"ok": true, "title": "<title>", "archived_path": "..."}`.

Not available for remote or in-place (`--here`) sessions (they don't own a
relocatable worktree).

### `af sessions restore <title>`

Restore a previously archived session: move its git worktree back next to the
repository, re-register it, re-spawn **only the agent** (shell/process tabs are
not restored), and mark the session running. Emits `{"ok": true, "title":
"<title>", "worktree_path": "..."}`.

Fails if the session is not archived, or if its origin repository is gone (the
archived worktree is left intact for manual recovery). Honors `--repo` like
`kill`.

### `af sessions whoami`

Report the session the current shell is inside, by matching the tmux session
name against stored instances. Emits the matching session object (same shape as
`get`); errors if the shell is not inside a known Agent Factory tmux session.

---

## `af tasks`

Manage tasks that deliver a prompt to an agent automatically — on a cron
schedule or whenever a watch script emits a stdout line. Full semantics live in
[tasks.md](tasks.md). All subcommands accept `--repo <path>` and `--json`.

Task operations are **daemon-owned**: the daemon is the single writer that hosts
task scheduling, so the write commands (**`add`**, **`update`**, **`remove`**,
**`trigger`**) go through it. `add` / `update` / `remove` persist the change and
re-arm the daemon's schedules atomically — starting a daemon if none is running,
since a task is not schedulable without one. `trigger` fires through the same
in-daemon path the cron scheduler uses. The on-disk `tasks.json` format is
unchanged.

The read commands **`list`** and **`get`** reflect the daemon's authoritative
task view when one is already running, and **never start a daemon**: with none
running (a script, CI, or a fresh shell) they fall back to reading `tasks.json`
off disk. Both sources return the same shape.

### `af tasks list`

List tasks (filtered to `--repo` when given). Emits a JSON array of task objects
(empty `[]` when none):

```json
[
  {
    "id": "a1b2c3",
    "name": "morning-standup",
    "prompt": "summarize overnight CI",
    "cron_expr": "0 9 * * *",
    "target_session": "",
    "project_path": "/home/you/project",
    "program": "claude",
    "enabled": true,
    "created_at": "2026-07-04T09:00:00Z"
  }
]
```

Exactly one of `cron_expr` / `watch_cmd` is set. Fields tagged `omitempty`
(`name`, `cron_expr`, `watch_cmd`, `target_session`, `last_run_at`,
`last_run_status`) appear only when set.

### `af tasks get <id>`

Fetch one task by ID. Emits a single task object (same shape as a `list`
element).

### `af tasks add`

Create a task. Emits the new task's ID: `{"id": "<id>"}`.

| Flag | Description |
|------|-------------|
| `--name` | **Required.** Task name. |
| `--prompt` | Prompt to send. Required for `--cron` tasks; `--watch-cmd` tasks default to the emitted line (with `{{line}}` substituted when present). |
| `--cron` | Cron expression. **Exactly one** of `--cron` / `--watch-cmd`. |
| `--watch-cmd` | Long-running watch command; each stdout line triggers the task. |
| `--target-session` | Deliver into this session (auto-created if missing); empty creates a new session per run. |
| `--program` | Agent enum; defaults to the configured `default_program`. |

### `af tasks update <id>`

Update a task's properties (partial update — an omitted/empty flag means "leave
unchanged"). Emits the updated task object.

| Flag | Description |
|------|-------------|
| `--name` | New task name. |
| `--prompt` | New prompt (must be non-empty when given). |
| `--cron` | New cron expression (clears `watch-cmd`). |
| `--watch-cmd` | New watch command (clears `cron`). |
| `--target-session` | New target session; pass an empty value to revert to a new session per run. |
| `--enabled` | `true` or `false`. |
| `--program` | New agent enum. |

Setting one trigger clears the other so the exactly-one rule holds.

### `af tasks trigger <id>`

Run a cron task immediately (cron tasks only) through the daemon's shared firing
path — the same entrypoint the cron scheduler uses. Watch tasks and disabled
tasks are refused. Emits `{"ok": true}`.

### `af tasks remove <id>`

Remove a task. Emits `{"ok": true}`.

---

## `af api`

Print the catalog of the daemon-hosted HTTP/JSON API — the localhost-only Unix
socket that mirrors the `af sessions` / `af tasks` operations for tools and
agents that call the daemon directly. **Read-only and local:** it resolves the
socket path and reads the in-process route catalog but never dials the socket or
starts the daemon, so it works with no daemon running.

```bash
af api          # human-readable: resolved socket path, auth model, every endpoint + a curl example
af api --json   # the same catalog as JSON, wrapped in the {data,error} envelope
```

The default output prints the resolved socket path
(`$AGENT_FACTORY_HOME/daemon-http.sock`, falling back to
`~/.agent-factory/daemon-http.sock`), the auth model (owner-only `0600` Unix
socket; no TCP port, no token), and a table of every endpoint —
`GET /v1/health` plus a `POST /v1/<Method>` for each mirrored RPC — with a
ready-to-run `curl --unix-socket …` example per endpoint. `--json` emits a
machine-readable catalog (`{socket_path, auth, endpoints: [{method, path,
description, request_fields?}]}`) in the shared envelope.

The catalog is derived from the daemon's actual route table, so it never drifts
from what the server serves. Full request/response reference:
[http-api.md](http-api.md).

---

## `af daemon`

Manage the background daemon that hosts task cron schedules, watch-task scripts,
and autoyes mode. It starts on demand whenever `af` runs and an enabled task
exists; installing an autostart unit keeps schedules firing after reboots. See
[tasks.md](tasks.md#daemon-lifecycle). **Text output.**

```bash
af daemon install      # register autostart at login (systemd user unit / launchd agent)
af daemon uninstall    # remove the autostart unit (the daemon still starts on demand)
```

---

## Maintenance commands

**Text output** unless noted.

```bash
af version             # print the version and the release URL
af debug               # print the config path followed by the resolved config as JSON
af keys                # print the effective TUI key bindings as a table (ACTION/KEYS/DESCRIPTION/SOURCE)
af upgrade             # self-upgrade to the latest GitHub release (Linux/macOS)
af doctor              # diagnose leaked processes/sessions/temp homes and daemon health
af doctor --fix        # also apply the safe remediations
af reset               # nuclear cleanup — see below
```

- `af debug` prints `Config: <path>` then the resolved config marshaled as
  indented JSON (a diagnostic dump, not part of the scriptable `--json`
  surface).
- `af doctor` is read-only by default: it reports orphaned processes from dead
  sessions, CPU-pegging processes in live sessions, `af_` tmux sessions with no
  backing record, abandoned temp homes, and daemon problems. With `--fix` it
  kills verified orphans and removes stale temp homes; anything it cannot verify
  is reported, never touched. Exits 1 when unresolved issues remain.
- `af reset` stops the daemon (reporting honestly if it couldn't), kills **all**
  Agent Factory tmux sessions, removes **every linked git worktree and its
  branch** from each repo with stored sessions, and deletes all stored session
  records. For recovering from a corrupted state — not day-to-day cleanup
  (`af sessions kill <title>` removes a single session cleanly).

---

## JSON envelope (`--json`)

`af sessions` and `af tasks` accept an opt-in **`--json`** flag that wraps output
in a stable envelope shared by the CLI and (in a later change) a daemon-hosted
HTTP server. It is **additive**: the flag defaults off, and without it every
command prints the bare payloads documented above, byte-for-byte as before.

With `--json`, a success wraps the payload under `data` with `error: null`:

```jsonc
// af sessions send-prompt fix-login "hi" --json
{
  "data": { "ok": true },
  "error": null
}
```

and a failure sets `data: null` and populates `error.message` (written to
stderr, exit code unchanged):

```jsonc
{
  "data": null,
  "error": { "message": "instance \"nope\" not found …" }
}
```

Both members always serialize, so a consumer can branch on `error === null`
without a presence check.
