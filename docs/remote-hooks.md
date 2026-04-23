# Remote Hooks

Agent Factory supports remote machine backends via user-provided hook scripts. When configured, remote sessions appear alongside local sessions in the TUI with the same attach/kill/preview experience.

## Configuration

Add remote hooks to your per-repo config at `~/.agent-factory/repos/<repoID>/config.json`:

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

Configuring `remote_hooks` enables the remote backend for that repo, but using it is explicit opt-in: press `N` in the TUI to create a remote session. Pressing `n` still creates a local tmux+git worktree session. When `remote_hooks` is absent, `N` is unavailable and all sessions are local.

## Script Protocol

All scripts must:
- Return exit code 0 on success, non-zero on failure
- Write JSON output to **stdout**
- Write progress/log messages to **stderr**
- Accept the flags documented below

### Session Names (slugs)

The `<name>` value passed to hooks is a slug derived from the user's session Title:

1. lowercase the Title
2. replace spaces with `-`
3. drop every character that is not `[a-z0-9-]`
4. trim leading/trailing `-`
5. if empty, use `session`

Examples: `"Fix Auth Bug"` → `fix-auth-bug`, `"my_app"` → `myapp`, `"af-test"` → `af-test`.

The slug is the session's stable identity in the hook protocol: it is the `--name` that `launch_cmd` is invoked with, the name `list_cmd` is expected to report back, the `--name` that `delete_cmd` receives, and the positional argument to `attach_cmd`. There is no hidden hash suffix — if your hook script computes `box-${NAME}` from the slug, it can rely on that value matching resources provisioned externally under the same slug.

Agent Factory enforces slug uniqueness at session-creation time (rejecting a second Title that reduces to an already-taken slug), so hook scripts can treat `--name` as a unique key without defensive rewriting.

### `launch_cmd`

Starts a new remote agent session.

**Arguments:** `--name <name> --json`

**stdout (JSON):**
```json
{"name": "fix-auth-bug", "status": "running"}
```

Required fields: `name` (string), `status` (string: `running`).
Additional fields are stored as metadata but not required.

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

Agent Factory also uses this script for the preview pane by running it in a background PTY and capturing its output.

## Example

See `examples/remote-hooks/` for skeleton scripts that implement this protocol.
