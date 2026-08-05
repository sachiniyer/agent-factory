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
   - scripts/lint-file-length.sh
   - go test ./<only the package you changed>/...  (skip if it is daemon/ or app/)
   - <any task-specific gate>

   NOT deadcode. It is whole-program reachability analysis, not a lint, and a
   fleet of sessions running it at once buries the box. CI's Lint job runs it on
   every push and will tell you if you left something unreachable.

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
   - No routine container runs — no make test-container,
     remote-roundtrip-container or playtest-container. CI covers the matrix, and
     a fleet of sessions each building a container buries the box.
   - Never run `scripts/tui-driver.sh` against a real repo (#1303).
   - No sub-sessions.
   - No dev-install.
   - No git stash command that reads or writes refs/stash (#2801). That is the
     test, not a list: bare `git stash` (with no subcommand it *is* push), push,
     save, pop, drop, clear, branch, list, store, and `show` or `apply` with no
     argument or a `stash@{N}` — all of them touch the shared stack. **Exactly
     two forms are fine**, and they are the recipe below: `git stash create`,
     which only writes a dangling commit, and `git stash apply <a ref you named
     yourself>`. That stack is shared by every worktree of the repo, so a sibling
     session can hand you its work or consume yours; it has already happened
     twice. Park work like this instead, as **one chain** — `refs/worktree/` is
     per-worktree, `refs/stash` is not:

         sha=$(git stash create "wip") && [ -n "$sha" ] \
           && git update-ref refs/worktree/af-wip "$sha" "" \
           && git reset -q && git -c core.hooksPath=/dev/null checkout -- :/
         # …then, to restore:
         git stash apply refs/worktree/af-wip \
           && git update-ref -d refs/worktree/af-wip

     That chain parks **modified tracked files**, staged or not. For untracked
     files or staged additions/renames, do not park — commit on your branch:

         git add -A && git commit -qm 'wip'

     You are on a session branch, so a wip commit costs nothing and squashes away
     before the PR. The chain above cannot undo a staged addition or rename: the
     cleanup turns the path back into an untracked file and the later apply then
     refuses rather than overwrite it.

     **Do neither while a merge, rebase, cherry-pick, revert or squash is
     paused.** Both moves run a reset or a commit, and both clear the sequencer
     state that `--continue` needs — you would lose the operation, not just the
     tree. Finish it or abort it first, then park.

     The chain is the safety property, not decoration. `git stash create` writes
     a dangling commit and touches nothing shared, but it prints an empty sha
     both on a clean tree and on failure, and it does not clean the tree itself —
     so an unchained clean after a failed create destroys exactly the work you
     were setting aside. The trailing `""` refuses to overwrite an earlier save.
     The cleanup is `git reset -q && git checkout -- :/`, both halves needed:
     `checkout` copies out of the **index**, so without the reset a staged edit
     stays staged and in the tree, and `:/` cleans from the repo root where a
     bare `.` would miss everything above your cwd. Untracked files are neither
     captured nor removed; they stay put.

     Everything a worker needs is in these two moves. CLAUDE.md's "Git hygiene in
     a shared repo" has the reasoning, and a third recipe that un-commits a
     scratch commit to get an actually-pristine tree — worth reading if you need
     that, but it carries guards for hooks, in-progress merges and crash recovery,
     and neither of the moves above needs them.

   **Deliver all of the above by the step 3 file recipe, never by pasting it into
   a double-quoted argument.** These bullets legitimately name commands in
   backticks, and inside `"<prompt>"` every one of those is command substitution
   running in root's own checkout: the bullets above mention `go test`,
   `make test-container` and `git stash`, so the prompt that forbids them would
   execute them, on the live tree, before the worker ever saw it. Writing the
   prompt to a file and sending its contents is what makes the text inert; that
   is the control, not this warning.

   That applies to the stash chain most of all: it contains
   `$(git stash create …)` and `"$sha"`, so hand-delivered through a double-quoted
   argument it would run `git stash create` in root's checkout and expand root's
   `$sha` into the worker's copy — corrupting the one recipe here whose failure
   loses work. There is no wording that makes a shell snippet safe to paste into
   a shell; use the file.

3. **Send or create the session** — the prompt must reach `af` as *data*. Write
   it to a file with your file-writing tool (not a shell heredoc), then send the
   file's contents:

   ```bash
   prompt="$(cat /path/to/prompt.txt)" \
     && af sessions send-prompt <name> "$prompt" --create \
     && rm -f /path/to/prompt.txt
   ```

   Read the file into a variable first, and chain. Inline as
   `send-prompt <name> "$(cat …)"`, a mistyped path only prints `cat`'s error and
   still calls `af` with an **empty** prompt — `send-prompt` accepts one — so you
   get a live worker sitting idle with no task, and the next line deletes the
   prompt file you were about to retype. Verified: unguarded, `af` is invoked with
   `prompt=[]`; chained, it is never invoked at all.

   **Never paste prompt text into `send-prompt <name> "<prompt>"` directly.**
   Inside those double quotes every backtick and `$(…)` is command substitution
   that runs in root's own checkout, before the worker exists. The box-safety
   rules above name `go test` and `git stash`; delivered that way, the prompt
   forbidding them executes them, on a live tree. Step 1 sends backticked
   notifications through a temp file for exactly this reason.

   A shell heredoc looks like the fix and is a worse trap, which is why it is not
   the recipe above. `cat > "$f" <<'PROMPT'` ends at the **first column-0 line
   matching the delimiter** — and prompt text routinely contains one. Dispatching
   work on this very skill does: the block would end at the word `PROMPT` in the
   quoted example, and everything after it runs as shell. Measured, with a prompt
   quoting a heredoc example:

   ```
   INJECTED-AND-EXECUTED
   commands executed: !!! GIT WAS EXECUTED: stash !!!
   ```

   If you have no choice but a heredoc, pick a delimiter you have confirmed is
   absent from the prompt, and keep the closing line at column 0 — indent it and
   the `send-prompt` call is swallowed into the file instead of running, so the
   dispatch silently never happens. `<<-` does not rescue that; it strips tabs,
   not spaces. Writing the file directly avoids the whole class.

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
