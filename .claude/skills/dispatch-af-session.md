---
name: dispatch-af-session
description: Dispatch and manage af sessions for Captain Claude issue work with safe prompts and root notification
user_invocable: true
---

# Dispatch AF Session

Dispatch focused implementation work to an `af` session with the standard
Captain Claude contract. **The session that writes the PR gates and merges it.**
Root does not sit between a finished PR and master — that queue is what turns a
green PR into a week-old PR.

## Steps

1. **Create a complete prompt** — every dispatch prompt carries the same
   contract:
   ```text
   Verify first: if this is already fixed on master, close the issue as stale
   and report that status.

   Task: <specific implementation request>

   You own this PR end to end, including the merge. Follow the gate-pr skill
   and merge it yourself when its gates pass. Do not hand the PR back to root.

   Local gates (cheap only, before opening the PR):
   - gofmt -l .
   - go build ./...
   - golangci-lint run --timeout=3m --fast
   - deadcode -test ./...
   - scripts/lint-file-length.sh
   - go test ./<only the package you changed>/...  (skip if it is daemon/ or app/)
   - <any task-specific gate>

   Do NOT run make test-container / remote-roundtrip-container /
   playtest-container as a routine gate — CI runs the full matrix on every
   push, and concurrent container runs take the shared box down. Push, let CI
   run the rest, and fix what it reports on your PR head.

   Merge gates (gate-pr, after the PR is open):
   - every triggered check finished, none failed
   - a Codex review for the exact head SHA — no review is NOT a clean review
   - zero unresolved inline findings, each cleared by an in-reply-to RESOLVED
   - merge with: gh pr merge <n> --squash --match-head-commit <gated sha>

   Sign the PR as Captain Claude.
   Include: Closes #<n>

   NOTIFY ROOT when the PR is merged, or when a gate blocks you and you
   cannot clear it yourself:
   af sessions send-prompt root "DONE <name>: <PR# merged | blocked on X>"
   ```

   If the notification message contains backticks, write it to a temp file and
   pass the file contents to `af sessions send-prompt`; inline backticks can
   shell-execute in root's repo:
   ```bash
   tmp="$(mktemp)"
   printf '%s\n' 'DONE <name>: PR #<n> merged (`golangci-lint` + CI green)' > "$tmp"
   af sessions send-prompt root "$(cat "$tmp")"
   rm -f "$tmp"
   ```

2. **Include box-safety rules** — every prompt must say:
   - No bare host `go test ./...`. Test only the package you changed, and if
     that package is `daemon/` or `app/`, run NO tests for it locally — push
     and let CI. They spawn real af daemons and drive real tmux on a box with
     ~15 live sessions.
   - No routine container runs; CI covers the matrix.
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

4. **Expect work to completion** — a session is done when its PR is **merged**,
   not when it is open. After notifying root it should continue to the next
   assigned slice when one exists, rather than idling on an open PR.

5. **Run the idle sweep** — periodically inspect sessions for stale, blocked,
   or completed work:
   ```bash
   af sessions list
   af sessions preview <name>
   ```

   Re-prompt sessions that have stopped without a root notification. A session
   sitting idle on an open, un-merged PR is the failure this contract exists to
   prevent — ask it which gate is blocking rather than merging on its behalf.

6. **Reap completed sessions** — once all PRs for a session's ticket have
   merged, archive it (restorable) rather than killing it:
   ```bash
   af sessions archive <name>
   ```

   Archive is the default "done" action because it stays restorable. Use
   `af sessions kill` only when you mean to permanently destroy the session and
   prune its owned branch. Do not reap a session while any of its PRs are still
   open — it is the one that has to clear the findings.
