# `setup_commands` — the session setup hook

A repo can declare shell commands that af runs **on the daemon's machine**
every time it provisions a session workspace — after the workspace, its ssh
route and its auth are up, before `post_worktree_commands` and the agent
start. It exists because a provisioned workspace is a fresh clone: it lacks
operator-local files (`.env`, dev certs, local config) and host-side setup
that a working session needs.

```toml
# .agent-factory/config.toml
setup_commands = ["./examples/hooks/session-setup.sh"]
```

Entries run sequentially in the checkout, under the same filtered environment
as `post_worktree_commands`. A failure does not block the session — it is
recorded on the session row (`af setup hook failed: …`) with the retained
output log, like a `post_worktree_commands` failure.

## Environment

| Variable | Meaning |
|---|---|
| `AF_SETUP_SESSION_ID` | Session id |
| `AF_SETUP_SESSION` | Session title |
| `AF_SETUP_SOURCE` | The session's source checkout on the daemon (also the working directory) |
| `AF_SETUP_MODE` | `local` or `remote` |
| `AF_SETUP_HOST` | The picked host name, when the session has one |
| `AF_SETUP_DEST` | Payload staging dir — its contents are copied into the fresh worktree before hooks run. A daemon-local path when no `AF_SSH` route is exported (every local session, and off-box runtimes with no ssh transport such as `backend = "docker"`); a path on the session's host for a remote session with an ssh route |
| `AF_SSH` | ssh command **without** the destination — the form `rsync -e` wants (remote, af-composed transports only) |
| `AF_SSH_DEST` | The destination (`user@host` or alias) |
| `AF_SSH_FULL` | The complete ssh command — always set when a transport exists, and the only ssh variable for a free-form `sandbox.ssh` command |

The ssh names reuse the session's own established connection — same contract
as `move_cmd` (`examples/move-script/move.sh` shows the same idioms). For a
free-form transport, pipe `tar` over `AF_SSH_FULL` instead of `rsync -e`.

## The example

`session-setup.sh` shows the two halves of the contract:

1. **Copying files in** — `.env*` and an editable list of uncommitted files
   are staged into `AF_SETUP_DEST` (locally, or over `rsync -e "$AF_SSH"` /
   `tar | $AF_SSH_FULL` remotely). af overlays them into the worktree.
2. **Running a command on the host** — a `mkdir -p` in the host's `HOME`,
   run locally or through `AF_SSH_FULL`.

The script is operator-controlled and is **not** confined by af on the remote
host — it can do whatever the new machine needs. The example deliberately
writes only into `AF_SETUP_DEST` and `HOME`; keep that discipline unless you
mean to change the host.
