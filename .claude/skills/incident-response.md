---
name: incident-response
description: Recover lost sessions, vanished worktrees, and daemon restarts without destroying resumable state
user_invocable: true
---

# Incident Response

Use this playbook when an `af` session, worktree, or daemon looks lost. Treat
lost sessions as recoverable until proven otherwise.

## Steps

1. **Classify before acting** — a `LOST` session is recoverable, not terminal.
   `Instance.Recover` re-spawns the agent and resumes it with the backend's
   continuation command, such as `codex resume --last` or `claude --continue`.

2. **Recover vanished worktrees** — if the worktree is gone but the branch is
   alive, re-add the worktree and then let lost-restore resume the session:
   ```bash
   git worktree add <path> <branch>
   af sessions restore <name>
   ```

   #1300 now rebuilds missing worktrees automatically during restore, so prefer
   `af sessions restore` when it can do the job.

3. **Avoid destructive cleanup** — `af sessions kill <name>` prunes the branch
   and discards resumable state. Use it only after confirming the worktree,
   branch, and commits are all unrecoverable or no longer needed.

4. **Know the known cause** — the worktree-vanish incident came from
   destructive `tui-driver` sandbox cleanup (#1303). The driver is now guarded
   fail-closed, but never run it against a real repo.

5. **Treat daemon restart as recoverable** — a daemon restart re-adopts live
   sessions and is safe when handled through `af`-managed daemon behavior. Do
   not kill the real daemon by process name.

6. **Never use broad host kills**:
   - Never run bare `tmux kill-server`; sandbox tmux work needs a private
     `TMUX` or `TMUX_TMPDIR`.
   - Never run bare `go test ./...` on the host; use `make test-container`.
   - Never `pgrep` and kill the real daemon by broad process name.

   If cleanup is required, scope it to the recorded sandbox path, private tmux
   socket, or exact PID that the sandbox created.
