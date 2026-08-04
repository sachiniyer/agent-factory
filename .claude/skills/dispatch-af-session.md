---
name: dispatch-af-session
description: Dispatch and manage af sessions for Captain Claude issue work with safe prompts and root notification
user_invocable: true
---

# Dispatch AF Session

Dispatch focused implementation work to an `af` session with the standard
Captain Claude contract. Root verifies and merges; worker sessions do not.

## Steps

1. **Create a complete prompt** — every dispatch prompt carries the same
   contract:
   ```text
   Verify first: if this is already fixed on master, close the issue as stale
   and report that status.

   Task: <specific implementation request>

   Do NOT merge. Root verifies the PR.

   Gates:
   - golangci-lint run --timeout=3m --fast
   - gofmt -l .
   - go build ./...
   - make test-container
   - deadcode -test ./...
   - scripts/lint-file-length.sh
   - <any task-specific gate>

   Sign the PR as Captain Claude.
   Include: Closes #<n>

   NOTIFY ROOT:
   af sessions send-prompt root "DONE <name>: <PR#/status>"
   ```

   If the notification message contains backticks, write it to a temp file and
   pass the file contents to `af sessions send-prompt`; inline backticks can
   shell-execute in root's repo:
   ```bash
   tmp="$(mktemp)"
   printf '%s\n' 'DONE <name>: PR #<n> (`make test-container` passed)' > "$tmp"
   af sessions send-prompt root "$(cat "$tmp")"
   rm -f "$tmp"
   ```

2. **Include box-safety rules** — every prompt must say:
   - No bare host `go test`; use `make test-container` only.
   - Never run `scripts/tui-driver.sh` against a real repo (#1303).
   - No sub-sessions.
   - No dev-install.
   - Never run a `git stash` command that touches the shared stack (#2801) —
     `git stash`/`push`, `pop`, `drop`, `clear`, `branch`, `list`, `store`,
     `apply stash@{N}`. `refs/stash` is one stack shared by every worktree,
     so a sibling session's `git stash pop` can hand you its work or silently
     consume yours — it has already happened twice. `git stash create` and
     `git stash apply <your own ref>` are fine; they never touch `refs/stash`.
     Set changes aside worktree-locally instead:
     ```bash
     git add -A && git commit -qm "wip: set aside" && wip=$(git rev-parse HEAD)
     # …later, and only while $wip is still the tip: git reset "$wip^"
     # or, closest to stash — refs/worktree/ is per-worktree, refs/stash is not.
     # Keep it one chain: an empty sha means create recorded NOTHING, the
     # trailing "" refuses to overwrite an earlier save, and `:/` cleans from the
     # repo root (a bare `.` misses everything outside your cwd). Cleaning the
     # tree after any of those failures destroys the work you set aside.
     sha=$(git stash create "wip") && [ -n "$sha" ] \
       && git update-ref refs/worktree/af-wip "$sha" "" && git checkout -- :/
     # later, and only if the apply succeeds — a conflicted apply needs the ref:
     # git stash apply refs/worktree/af-wip && git update-ref -d refs/worktree/af-wip
     ```
     CLAUDE.md's "Git hygiene in a shared repo" has the caveats.

3. **Send or create the session**:
   ```bash
   af sessions send-prompt <name> "<prompt>" --create
   ```

   Use a unique session name that identifies the issue or slice. Keep the
   prompt self-contained because the receiving agent inherits no root context.

4. **Expect work to completion** — after notifying root, the worker should
   continue to the next assigned slice when one exists. It should not idle
   just because the first PR is open.

5. **Run the idle sweep** — periodically inspect sessions for stale, blocked,
   or completed work:
   ```bash
   af sessions list
   af sessions preview <name>
   ```

   Re-prompt sessions that have stopped without a root notification. Archive or
   kill only when the session is genuinely done or unrecoverable.

6. **Reap completed sessions** — once all PRs for a session's ticket have
   merged, clean up the session and its worktree:
   ```bash
   af sessions kill <name>
   ```

   Do not reap a session while any of its PRs are still under review.
