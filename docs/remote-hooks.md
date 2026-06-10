# Remote Hooks

Agent Factory supports remote machine backends via user-provided hook scripts. When configured, remote sessions appear alongside local sessions in the TUI with the same attach/kill/preview experience.

## Configuration

Add remote hooks to the repo's own config file at `<repo-root>/.agent-factory/config.json` (check it into the repo so every clone gets the same backend):

```json
{
  "remote_hooks": {
    "launch_cmd": "/path/to/launch.sh",
    "list_cmd": "/path/to/list.sh",
    "attach_cmd": "/path/to/attach.sh",
    "delete_cmd": "/path/to/delete.sh"
  }
}
```

`remote_hooks` is an in-repo-only setting — it describes the repository, so it is not accepted in the global `~/.agent-factory/config.json`. The previous location, `~/.agent-factory/repos/<repoID>/config.json`, is **deprecated**: it keeps working for one more release as a fallback (with a warning in the log pointing here), and is ignored whenever the in-repo file sets `remote_hooks`. See the README's [Configuration](../README.md#configuration) section for the full precedence rules.

Configuring `remote_hooks` enables the remote backend for that repo, but using it is explicit opt-in: press `N` in the TUI to create a remote session. Pressing `n` still creates a local tmux+git worktree session. When `remote_hooks` is absent, `N` is unavailable and all sessions are local.

`launch_cmd`, `attach_cmd`, and `delete_cmd` are **required** — an empty value is rejected when the remote backend is resolved, with an error naming the missing field (e.g. `remote_hooks.launch_cmd is required`) rather than a cryptic `exec: no command` at operation time. `list_cmd` is **optional for import/sync only**: when it is empty, Agent Factory simply reports no remote sessions to enumerate (import/sync are skipped) and config validation does not reject it.

> **Note:** `list_cmd` is **required for restore**. To carry remote sessions across restarts, Agent Factory re-runs `list_cmd` at startup to confirm each persisted session is still alive (#645). If `list_cmd` is empty, restore cannot verify liveness and the session is dropped with an actionable error naming the missing field — so configure `list_cmd` whenever you want remote sessions to survive restarts.

## Script Protocol

All scripts must:
- Return exit code 0 on success, non-zero on failure
- Write JSON output to **stdout**
- Write progress/log messages to **stderr**
- Accept the flags documented below

### Session Names

The `<name>` value passed to hooks is a slug derived from the session title:

1. lowercase the title
2. replace spaces with `-`
3. drop every character that is not `[a-z0-9-]`
4. trim leading/trailing `-`
5. if empty, use `session`

Examples: `"Fix Auth Bug"` becomes `fix-auth-bug`, `"my_app"` becomes `myapp`, and `"af-test"` stays `af-test`.

This slug is the stable remote identity. Agent Factory passes it to `launch_cmd --name`, expects `list_cmd` to report it as `name`, passes it to `delete_cmd --name`, and passes it as the positional argument to `attach_cmd`. There is no hidden hash suffix.

When Agent Factory imports an existing remote session from `list_cmd`, the reported `name` is stored in `remote_meta.name` and remains authoritative even if the display title differs.

### `launch_cmd`

Starts a new remote agent session.

**Arguments:** `--name <name> --json`

**stdout (JSON):**
```json
{"name": "fix-auth-bug", "status": "running"}
```

Required fields: `name` (string), `status` (string: `running`).
Additional fields are stored as metadata but not required.

If `launch_cmd` exits 0 but its output cannot be parsed as the expected JSON object, Agent Factory assumes the remote session may have been created and invokes `delete_cmd --name <name>` best-effort to avoid orphaning it, then surfaces the parse error. Keep `launch_cmd` output limited to the JSON object (progress text on stderr) so this fallback is not triggered for healthy sessions.

### `list_cmd`

Lists all running remote sessions.

**Arguments:** `--json`

**stdout (JSON):**
```json
[
  {"name": "fix-auth-bug", "status": "running"},
  {"name": "refactor-api", "status": "stopped"}
]
```

Required fields per entry: `name` (string), `status` (string: `running`, `stopped`, or `error`).

### `delete_cmd`

Tears down a remote session and cleans up all associated resources.

**Arguments:** `--name <name> --json`

**stdout (JSON):**
```json
{"name": "fix-auth-bug", "deleted": true}
```

### `attach_cmd`

Gives interactive terminal access to a running session (e.g., SSH + tmux attach).

**Arguments:** `<name>`

**No JSON output** — this command takes over the terminal. It should behave like `ssh -t host "tmux attach"`.

Agent Factory runs this command behind a local PTY and intercepts the configured detach key before forwarding input to the hook process. On detach, Agent Factory terminates the local `attach_cmd` process; remote tmux sessions should survive the client disconnect and be attachable again later.

Agent Factory also uses this script for the preview pane by running it in a background process and capturing its output.

#### Preview output contract

Preview output is rendered **inside a TUI pane**, not on a raw terminal. Two rules apply to the captured stream:

- **Control sequences are stripped.** Cursor movement, erase, scroll-region, alt-screen, and mode sequences (e.g. `\033[H`, `\033[?1049h`, `\033[?25l`), OSC strings, and bare carriage returns are removed before rendering — only SGR color sequences (`\033[...m`) and plain text (with `\n`/`\t`) reach the pane. Scripts don't need to avoid emitting these, but they have no effect.
- **`\033[2J` (clear screen) starts a fresh snapshot.** Everything captured before the last `\033[2J` is discarded, so a clear-then-capture loop replaces the previous frame instead of concatenating stale ones.

A preview-friendly capture loop looks like:

```bash
while true; do
  printf '\033[2J\033[H'
  ssh user@host "tmux capture-pane -p -e -t $NAME"
  sleep 1
done
```

Each iteration becomes the new preview frame. `capture-pane -e` keeps colors; they are preserved in the pane.

## Example

See `examples/remote-hooks/` for skeleton scripts that implement this protocol.
