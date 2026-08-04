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
  `deadcode -test ./...`, `scripts/lint-file-length.sh` — plus `go test` on
  **only the non-daemon package you changed**. Then push and let CI run the
  rest, and fix what CI reports on your PR head.
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
deadcode -test ./...   # should produce no output
scripts/lint-file-length.sh   # or: make lint-file-length
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

**Never run a `git stash` command that touches the shared stack (#2801)** — that
is `git stash` / `stash push`, `pop`, `drop`, `clear`, `branch`, `list`, and
`apply` of a `stash@{N}` entry. **`git stash store` is banned too**, and it is the
easy one to reach for by accident: it is the documented companion to `stash
create`, and it pushes that commit straight onto the shared stack. Pin created
commits under `refs/worktree/…` instead, exactly as below.

Exactly two forms are fine, and they are the substitutes below: **`git stash
create`**, which writes a dangling commit and never touches `refs/stash`, and
**`git stash apply <a ref you named yourself>`**.

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
# one chain: if the commit does not happen, nothing is parked and $wip must stay
# unset — see below for why an unconditional $wip is dangerous
git add -A && git commit -qm "wip: set aside" && wip=$(git rev-parse HEAD)
# …do the thing that needed a clean tree, and do not commit while parked…
git reset "$wip^"               # a MIXED reset: tracked edits come back
                                # unstaged, new files untracked again
```

Use a mixed reset, not `--soft`. `--soft` moves `HEAD` only, so after the
`git add -A` above every parked change comes back **staged** — and the next
`git commit` you make for unrelated reasons then sweeps the whole WIP into it.
Neither reset restores an intentional staged/unstaged split; mixed at least
restores the common case exactly.

The `&&` before `wip=` is load-bearing. `git commit` fails on an already-clean
tree and whenever a pre-commit hook rejects, and an unconditional
`wip=$(git rev-parse HEAD)` after that names **a real commit you care about** —
`git reset "$wip^"` then uncommits *that* instead of restoring anything. Chained,
a failed commit leaves `$wip` unset and the reset refuses. If the commit did not
happen, stop: nothing was parked.

`$wip` is not decoration either. `HEAD~1` names *one commit back from wherever
you are now*, so a cherry-pick, revert, or urgent fix while parked makes
`git reset HEAD~1` uncommit **that** and strand the WIP in your history. Resetting
to `"$wip^"` is still only right while `$wip` is the tip — check with
`git rev-parse HEAD`. If you did commit on top, the WIP is already in the branch:
drop that one commit with `git rebase --onto "$wip^" "$wip"`, then
`git cherry-pick -n "$wip"` if you want its content back as uncommitted changes.

**`git stash create` plus a per-worktree ref** — closest to `git stash`, and
`refs/worktree/` genuinely *is* unshared, so no sibling can see or consume it:

```bash
sha=$(git stash create "wip")   # dangling commit; refs/stash is NOT touched
if [ -z "$sha" ]; then
  echo "nothing recorded — do NOT clean the tree"
# the trailing "" means "must not already exist" — a second save under the same
# name would otherwise orphan the first, leaving it fsck-only
elif git update-ref refs/worktree/af-wip "$sha" ""; then
  git checkout -- :/            # only now: create records, it does not clean.
                                # `:/` is the repo root — a bare `.` cleans only
                                # below your cwd while create recorded the lot
else
  echo "af-wip is taken — pick another name; tree left dirty on purpose"
fi
# …later. Delete the ref only if the apply succeeded: a conflicted apply exits
# non-zero, and that is precisely when you still need the pointer.
git stash apply refs/worktree/af-wip && git update-ref -d refs/worktree/af-wip
```

**That guard is the load-bearing line, not decoration.** `git stash create`
records nothing and prints an empty sha in two different situations — a clean
tree, and a failure — and the failure is easy to hit: a half-added `git add -N`
file makes it exit non-zero with `Entry … not uptodate`. Clean the tree after
that and you have destroyed exactly the work you were setting aside.

Two more ways `create` differs from `push`: it does not clean the working tree
(`push` does that; `create` only records), and it does not capture untracked
files — `git add` them first, or use the scratch commit above.

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
