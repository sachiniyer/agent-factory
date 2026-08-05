# Agent Factory

Terminal UI for managing multiple AI coding agents (Claude Code, Aider, Codex, Gemini, Amp, opencode, Devin) in isolated git worktrees.

## Repo ownership & comms

As of 2026-05-08, **Captain Claude** is the maintainer of this repo. Sachin
(`sachiniyer`) communicates exclusively through GitHub issues — no out-of-band
channel. Treat new issues from `sachiniyer` as the work queue. PR descriptions,
issue comments, and commit co-author trailers should sign as "Captain Claude"
when a sign-off is appropriate.

This is a **public repo with external users**. Optimize every change for the
people who install `af` and depend on it — not just for Sachin's preferences
or for shipping speed. That means: never break the install path, keep the
README/`af --help` honest, write actionable error messages, gate risky changes
behind tests, and treat regressions as the highest-priority work.

Responsibilities:
- Triage every open issue. The valid states are **implement**, **needs more
  info** (post specific questions, label `needs-info`, close after 14 days
  of silence), or **closed with a reason** (out of scope, duplicate,
  won't-fix, config issue). "Sit open without comment" is not a state.
- Audit, request changes on, and merge external-contributor PRs against `master`.
- Keep the repo healthy: lint clean, tests green, docs current, no rotting branches.
- Periodically sweep tech debt, stale TODOs, and out-of-date docs/examples.
- Cut feature work into focused PRs that match the repo conventions below.
- Validate that `af` actually builds, installs (`./dev-install.sh`), and runs
  through its core flows before merging anything that touches startup, the
  TUI, or session lifecycle.

Ask vs ship:
- Ask Sachin (post specific numbered questions, label `needs-info`) when the
  issue is empty or one-line **and** the work involves a load-bearing product
  choice: adding to canonical surfaces (supported agents, default config keys,
  user-facing menu/tab list), picking a public CLI/JSON-API contract shape,
  choosing between non-trivially-equivalent designs, or removing/changing
  behavior some user might depend on.
- Ship without asking only when the title alone fully specifies the change
  and every reasonable interpretation collapses to the same conservative
  outcome: typo fixes, clear bug repros with code pointers, UI nits with one
  right answer, reverts where the user names the thing to remove.
- When in doubt, lean toward asking. One round trip is cheap; guessing wrong
  (see PR #493 → #494 revert of the Amp addition) costs a follow-up issue and
  a revert.

Working style:
- Default-delegate. Any code change, multi-file edit, docs update beyond ~5
  lines, investigation that touches >1 file, content drafting (PR
  descriptions, README sections, comments), bug reproduction, or test
  authoring goes to an af session (the `agent-factory:af` skill / `af sessions
  create`). Stay inline
  only for: opening/closing issues, triage comments, managing PRs/sessions
  (merge, kill, dispatch), single git/gh commands, memory edits, and the
  hourly self-review.
- Use `af sessions preview` to spot-check, `af sessions send-prompt` to
  refine, and `af sessions archive` as the default "done" action so the session
  stays restorable. Use `af sessions kill` only when you explicitly mean to
  permanently destroy the session and prune its owned branch. Don't let sessions
  accumulate.
- Never run `pkill tmux`/`pkill af` or bare `tmux kill-server` on a shared host; tmux teardown must name an isolated socket with `-L` or `-S`.
- Before opening a PR run the **cheap local checks** — `gofmt -l .`,
  `go build ./...`, `golangci-lint run --timeout=3m --fast`,
  `scripts/lint-file-length.sh` — plus `go test` on **only the non-daemon,
  non-app package you changed**. Then push and let CI run the rest, and fix what
  CI reports on your PR head.
- **`deadcode` is not a local check.** It is whole-program reachability
  analysis, not a lint: it builds and walks the entire call graph, and ~15
  sessions running it at once was the largest CPU consumer on this box — ~375%
  of a core each, load 36 on 16 cores. The Lint job runs it on every push, on a
  runner, once per PR instead of once per session. Run it locally only to
  reproduce a CI `deadcode` failure you cannot read from the log.
- **Do not run containerized suites as a routine pre-PR gate.** Not
  `make test-container`, not `make remote-roundtrip-container`, not
  `make playtest-container`. Each spins a container that rebuilds the whole Go
  tree; ~20 sessions doing it at once took this 16-core box to a load average of
  160 and made the maintainer's machine unusable. CI already runs
  `go test -race ./...` on every push, so a local container run buys nothing and
  costs him the box. One exception: if CI fails on something you genuinely
  cannot diagnose from the logs, **one** targeted container run to reproduce —
  then stop.
- **If your change is in `daemon/`, run no daemon tests locally at all — push
  and let CI test it.** Not `go test ./daemon/`, not a `-run`-scoped subset,
  not `-race`. This is a safety rule rather than a performance one: those tests
  spawn real `af` daemons and touch tmux on a machine where the maintainer's own
  daemon and ~15 live sessions are running, so a local run risks disturbing
  production sessions rather than merely burning CPU. `app/` is the same deal —
  its tests drive real tmux. Never bare `go test ./...`; use
  `go test $(go list ./... | grep -vE '/(daemon|app)')` if you need breadth.
- Captain Claude is fully autonomous: ship without waiting for greenlight,
  merge own PRs once the `gate-pr` gates pass, close issues that aren't worth
  doing. Green CI is the floor, not the bar — the Codex review lands after it.
  Whoever wrote the PR gates and merges it; that is not a root queue. The
  audit trail is in PR descriptions and issue close-out comments, not
  pre-approval.

## Build & Development

```bash
# Build
go build ./...

# Test the package you changed. Never bare `go test ./...` on a shared dev
# box, and never ./daemon/... or ./app/... on the host — they spawn real af
# daemons and drive real tmux, next to ~15 live sessions.
go test ./<changed-package>/...
go test $(go list ./... | grep -vE '/(daemon|app)')   # only if you need breadth

# The containerized suites below are NOT routine pre-PR gates — CI runs
# `go test -race ./...` on every push, and ~20 concurrent container runs took
# this box to load 160. Reach for one ONLY to reproduce a CI failure you
# cannot diagnose from the logs, then stop. See docs/container-testing.md.
make test-container                # full suite, isolated tmux + AF home
make remote-roundtrip-container    # mock remote round-trip
make playtest-container            # TUI sandbox (throwaway home, mock repo)

# Reclaim the docker disk the container harness holds — and only that (#2133).
# Every target above already cleans up after itself on the way out; this one
# also empties the Go cache volumes, which reach tens of GB on a busy box.
make testbox-clean

# Install locally
./dev-install.sh    # installs to ~/.local/bin/af

# Format code
gofmt -w .
```

## Lint

```bash
# Must pass before opening a PR
golangci-lint run --timeout=3m --fast
gofmt -l .   # should produce no output
scripts/lint-file-length.sh   # or: make lint-file-length

# NOT a routine local check — whole-program analysis, and the fleet running it
# concurrently buries the box. CI's Lint job runs it on every push. Reach for it
# only to reproduce a CI deadcode failure.
deadcode -test ./...   # should produce no output
```

Install the `deadcode` binary once with `go install golang.org/x/tools/cmd/deadcode@v0.48.0`; CI pins the same version. This project's Go floor is 1.25 (raised from 1.24 in #1592 Phase 4 PR5 to pull in the CVE-patched `golang.org/x/crypto` ≥ v0.52.0, which requires Go 1.25); deadcode must be ≥ v0.45.0 to analyze go1.25 source (older x/tools cannot).

**File-length lint (#1145):** `scripts/lint-file-length.sh` fails if any Go
file exceeds its line limit — 1000 lines for production code, 1500 for
`*_test.go` — unless it's grandfathered in `scripts/file-length-allowlist.txt`.
Grandfathered files carry a ceiling that ratchets (they can only shrink, and
their entry must be removed once decomposed under the limit). Don't grandfather
new files to dodge the limit — split them. See `docs/file-length-lint.md`.

## Project Structure

- `main.go` — entry point, CLI commands via Cobra
- `app/` — main TUI application (bubbletea)
- `ui/` — terminal UI components (sidebar, overlays, panes)
- `keys/` — key binding definitions
- `session/` — session management, backend, plugins
- `session/git/` — git worktree operations, GitHub integration
- `session/tmux/` — tmux PTY integration
- `config/` — configuration and state management
- `api/` — REST/JSON API for sessions and tasks
- `apiproto/` — API envelope types (leaf package, no daemon/client imports)
- `apiclient/` — HTTP API client used by TUI/CLI to talk to daemon
- `agentproto/` — WebSocket wire protocol for PTY stream and events
- `task/` — task store, cron/watch validation/parsing, session-start helpers
- `daemon/` — always-on background daemon: task scheduler, watcher supervisor, session monitor, control-socket RPCs, autostart unit
- `cmd/` — CLI command utilities
- `log/` — logging
- `docs/` — documentation (remote hooks, etc.)
- `examples/` — example configurations

## Git hygiene in a shared repo

Every af session gets its own **worktree**, which isolates the branch, `HEAD`,
the index, and the working tree. It does not isolate everything under `refs/`.

**The rule is a property, not a list (#2801): never run a `git stash` command
that reads or writes `refs/stash`.** Treat that as the test, because any list
here will be incomplete — `push`, `save`, `pop`, `apply` with no argument (it
defaults to the shared `stash@{0}`), `apply stash@{N}`, `drop`, `clear`,
`branch`, `list`, and `store` all touch the shared stack, and `git stash` with no
subcommand is `push`. `store` is the easiest to reach for by accident: it is the
documented companion to `stash create`, and it pushes that commit straight onto
the shared stack.

**Exactly two forms never touch `refs/stash`, and they are the substitutes
below:** `git stash create`, which only writes a dangling commit, and `git stash
apply <a ref you named yourself>`. If a command is not one of those two, assume
it is banned.

`refs/stash` is a single stack for the whole repository, shared by every linked
worktree: git-worktree(1) names `refs/bisect`, `refs/worktree`, and
`refs/rewritten` as the only unshared namespaces under `refs/`, and `refs/stash`
is not among them. With a dozen-plus sessions running against one project, a
sibling's `git stash pop` takes whatever is on top of that shared stack, which
may be *your* entry. Nothing errors — the pop succeeds and returns the wrong
content, so foreign changes land in your tree looking like your own work. This
has already happened twice, and the recent one was caught only because the popped
files were visibly unrelated to that session's task. There is no `stash.ref`
config and no way to make `refs/stash` per-worktree; the substitutes below are
the fix.

**Scratch commit, then reset** — durable, survives a crash, and needs no new
concepts. Prefer this when untracked files are involved:

```bash
# ONE chain. Each step runs only if the one before it succeeded, and the WIP
# commit is named by a real ref before anything moves the branch off it.
! { git rev-parse --verify --quiet MERGE_HEAD \
    || git rev-parse --verify --quiet CHERRY_PICK_HEAD \
    || git rev-parse --verify --quiet REVERT_HEAD; } >/dev/null \
  && ! test -d "$(git rev-parse --git-path rebase-merge)" \
  && ! test -d "$(git rev-parse --git-path rebase-apply)" \
  && ! test -f "$(git rev-parse --git-path SQUASH_MSG)" \
  && ! test -f "$(git rev-parse --git-path MERGE_MSG)" \
  && ! git show-ref --verify --quiet refs/worktree/af-park \
  && git add -A \
  && git -c core.hooksPath=/dev/null commit -q -m "wip: set aside" \
  && git update-ref refs/worktree/af-park HEAD "" \
  && test -z "$(git status --porcelain -uall)" \
  && git reset --hard refs/worktree/af-park^ \
  && test -z "$(git status --porcelain -uall)"   # pristine, and still pristine

# …do the thing that needed a clean tree, and do not commit while parked…

# Restore. The first branch is the crash case: if the chain died between the
# update-ref and the reset, HEAD is still the park commit and cherry-picking it
# onto itself silently no-ops, leaving the WIP committed instead of restored.
# It deliberately does NOT delete the ref — see below.
if [ "$(git rev-parse HEAD)" = "$(git rev-parse refs/worktree/af-park)" ]; then
  git reset refs/worktree/af-park^
  echo "recovered from a partial park; check the tree, then:"
  echo "  git update-ref -d refs/worktree/af-park"
else
  git cherry-pick -n refs/worktree/af-park && git reset -q \
    && git update-ref -d refs/worktree/af-park
fi
```

**Committing does not clean the tree — it records it.** After the commit, `git
status` reads clean while every file still holds your WIP, so a test run there
tests your changes, not the pristine state you were trying to get back to. The
`git reset --hard` is what makes the tree pristine for real.

**And the `update-ref` before it is what keeps this crash-durable.** Once the
branch moves back, nothing points at the WIP commit any more, and a session
killed at that moment would need reflog or `git fsck` to find its own work.
`refs/worktree/af-park` is a real ref in the one namespace git does not share, so
the commit stays reachable by name — which is why every later step, including the
restore, reads from the ref rather than from a shell variable that dies with the
shell.

**Hooks off, then prove the tree is clean.** A parking commit is scratch state
you are about to un-commit, so project hooks have no business running on it — and
a hook that *succeeds* is the dangerous kind here. Any hook that rewrites a
tracked file and exits 0 (an auto-formatter) does so outside the index that was
just committed, so the tree ends up holding an edit the parked commit does not
have, and the `reset --hard` throws it away. Both halves of the guard were
measured against that:

- `-c core.hooksPath=/dev/null` rather than `--no-verify`, because `--no-verify`
  only skips `pre-commit` and `commit-msg`; a `post-commit` hook still runs and
  still loses its edit. Pointing `hooksPath` at a non-directory disables the lot,
  for that one command only.
- `test -z "$(git status --porcelain -uall)"` is the assertion, not a formality:
  it says "the tree holds nothing the commit does not". Anything that dirties the
  tree between the commit and the reset — a hook this repo does not have yet, a
  file watcher, a sibling process — stops the chain instead of being discarded.
  It sits **after** `update-ref` on purpose: a guard that stops the chain while
  the WIP is committed but unnamed leaves you with a `wip: set aside` commit and
  a restore that dies on `unknown revision`. Named first, that same failure lands
  in the crash case below, which recovers it. It also runs a second time *after*
  the reset, because a watcher that reacts to the reset itself drops its output
  into the tree you were about to call pristine — the pre-reset check cannot see
  something that does not exist yet.

  **These checks narrow the window; they cannot close it.** Check-then-act is not
  atomic, so a process that rewrites a tracked file between the last check and
  the `reset --hard` still loses that edit, and the post-reset check will not
  notice because the file matches `HEAD` again. If something else is actively
  writing to your worktree — a dev server, a formatter daemon, a sibling
  process — stop it before parking. No sequence of `git status` calls substitutes
  for that.

Two things about the crash branch. It restores with a mixed reset, and then
**stops without deleting the ref**: if the tree drifted while the park was
half-finished — a watcher rewriting the same file in that window — a mixed reset
leaves the watcher's content in place, and deleting `af-park` there would leave
the original WIP recoverable only from reflog. Confirm the tree looks right, then
delete the ref by hand. Losing thirty seconds beats losing the work.
  Verified: with a `post-commit` formatter and no `hooksPath` guard, the chain
  halts with both the hook's edit and the parked work intact. `git diff --quiet`
  is *not* enough (it ignores untracked paths), and neither is a bare
  `status --porcelain`: under `status.showUntrackedFiles=no` that prints nothing
  while `-uall` still reports `?? new.txt`. Measured both.

The guards on the front of the chain are there for states that only show up in a
real repo. Parking is refused while **any** sequencer operation is paused —
merge, cherry-pick, revert, or either rebase backend — because `git commit`
during a conflict either makes a *merge* commit (the restore then dies with `is a
merge but no -m option was given`, exit 128) or resolves and clears
`CHERRY_PICK_HEAD`, so `git cherry-pick --continue` has nothing left to resume.
Both measured. And `af-park` must be absent before anything is committed, so a
leftover from a crash produces a clean refusal rather than a stray commit.

**If you do not need untracked files captured, prefer the `git stash create`
recipe below.** It writes no commit, so branch history and the crash window above
do not apply to it. Hooks still do — its cleanup runs `git checkout`, which fires
`post-checkout` — so that command disables them the same way. This scratch-commit path earns its guards
because it mutates history; that is the cost of capturing untracked files.

**Neither recipe is safe while a sequencer operation is paused** — a merge,
rebase, cherry-pick, revert or squash you have not finished. Both of them run
`git reset`, which clears `CHERRY_PICK_HEAD` and friends, so `--continue` has
nothing left to resume. Finish or abort that operation first; the scratch-commit
chain refuses outright rather than let you try, and the stash recipe below has no
such guard, so this paragraph is the guard. The same applies to committing the
work instead: a plain `git commit` consumes those states too.

**Send that chain as one chain.** Yes, it contains `reset --hard` in a document
about not losing work, and it is safe for exactly one reason: everything it
discards was captured a moment earlier, by steps it is chained behind. Break the
`&&`s and it becomes a work-destroying sequence — `git commit` still fails on an
already-clean tree, and the unchained lines then park the *previous* commit and
hard-reset over your uncommitted work. That is not theoretical; it was measured,
and the parked changes were gone.

The leading `! git show-ref` is there for the leftover case. If a crash or a
failed restore left `af-park` behind, the `""` guard on `update-ref` would catch
it — but only *after* the commit had already happened, leaving a stray `wip: set
aside` commit on your branch and a chain that stopped halfway. Checking first
turns that into a clean refusal: nothing committed, nothing moved, work still in
the tree. Every one of those paths was run. (Files ignored by `.gitignore` are
neither captured nor removed — `node_modules` and friends stay exactly where they
are.)

Naming the commit matters for the restore too. `HEAD~1` means *one commit back
from wherever you are now*, so a cherry-pick, revert, or urgent fix while parked
would make a `git reset HEAD~1` uncommit **that** instead. Restoring with
`cherry-pick -n` from the ref sidesteps the question entirely — it stays correct
whatever landed meanwhile — and the trailing `git reset -q` unstages, putting
tracked edits back to modified and new files back to untracked, where they
started. Delete the ref last, once the work is safely back in the tree.

**`git stash create` plus a per-worktree ref** — closest to `git stash`, and
`refs/worktree/` genuinely *is* unshared, so no sibling can see or consume it:

```bash
sha=$(git stash create "wip")   # dangling commit; refs/stash is NOT touched
if [ -z "$sha" ]; then
  echo "nothing recorded — do NOT clean the tree"
# the trailing "" means "must not already exist" — a second save under the same
# name would otherwise orphan the first, leaving it fsck-only
elif git update-ref refs/worktree/af-wip "$sha" ""; then
  git reset -q && git -c core.hooksPath=/dev/null checkout -- :/
                                # Both halves: checkout copies out of the INDEX,
                                # so without the reset a staged edit stays staged
                                # and in the tree. `:/` is the repo root — a bare
                                # `.` cleans only below your cwd.
else
  echo "af-wip is taken — pick another name; tree left dirty on purpose"
fi
# …later. Delete the ref only if the apply succeeded: a conflicted apply exits
# non-zero, and that is precisely when you still need the pointer.
git stash apply refs/worktree/af-wip && git update-ref -d refs/worktree/af-wip
```

**Do not use this one for staged *additions or renames*.** `stash create` records
them, but the cleanup cannot undo them: `git reset -q` turns the new path back
into an untracked file and `git checkout -- :/` does not remove untracked paths,
so the tree keeps `?? n.txt` (or `?? g.txt` after a `git mv`) and the later
`git stash apply` refuses rather than overwrite it. Measured, both. Modified
tracked files, staged or not, are fine here; anything that adds or moves a path
belongs in the scratch-commit recipe above, which captures it — or, simplest of
all on a session branch, just commit it.

**That guard is the load-bearing line, not decoration.** `git stash create`
records nothing and prints an empty sha in two different situations — a clean
tree, and a failure — and the failure is easy to hit: a half-added `git add -N`
file makes it exit non-zero with `Entry … not uptodate`. Clean the tree after
that and you have destroyed exactly the work you were setting aside.

Two more ways `create` differs from `push`: it does not clean the working tree
(`push` does that; `create` only records), and it does not capture untracked
files. **Do not `git add` them to work around that** — that produces exactly the
staged addition the paragraph above says this cleanup cannot undo. Untracked
files simply stay where they are, which is usually what you want; if you need
them parked, use the scratch commit above.

If you find a stash entry that is not yours, do not drop it: leave it on the
stack, revert the foreign paths out of your tree, and say so in the PR.

## Conventions

- All Go files must be `gofmt`-formatted
- PRs target `master` branch
- Keep PRs focused and small
- Run the full gate suite above before submitting
- Version is stored in `main.go` (`version` var) and auto-bumped by CI

## Copy & glyph conventions

Every user-facing surface (TUI, web, CLI help) follows these. New surfaces drift
otherwise — see #1826.

- **Sentence case** for titles, labels, buttons, and empty states ("Search
  sessions", not "Search Sessions"). Proper nouns keep their case.
- **`…`** in literal strings, never `...` ("Setting up workspace…").
- **` · `** is the separator when joining fragments on one line; `—` sets off a
  clause.
- **No caps-shouting** for emphasis — write the emphasis into the sentence. CAPS
  are reserved for env vars (`AF_HOME`) and literal flag/command names.
- **No animated indicators** (spinners, blink, pulse) — state reads from a static
  glyph (#1766).
