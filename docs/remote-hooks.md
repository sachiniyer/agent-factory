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

When `remote_hooks` is absent, Agent Factory uses local tmux+git worktrees (default). When present, all sessions for that repo use the remote backend.

## Script Protocol

All scripts must:
- Return exit code 0 on success, non-zero on failure
- Write JSON output to **stdout**
- Write progress/log messages to **stderr**
- Accept the flags documented below

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
