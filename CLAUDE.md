# Agent Factory

Terminal UI for managing multiple AI coding agents (Claude Code, Aider, Codex, Gemini, Amp, opencode) in isolated git worktrees.

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
  stays restorable. Use `af sessions kill --force` only when you explicitly mean
  to permanently destroy the session and prune its branch. Don't let sessions
  accumulate.
- Run `golangci-lint run --timeout=3m --fast`, `gofmt -l .`, `go build ./...`,
  and the full test suite before opening a PR — CI blocks on all four. On a
  shared dev box run the suite as `make test-container` (never bare
  `go test ./...` on the host; see docs/container-testing.md).
- Captain Claude is fully autonomous: ship without waiting for greenlight,
  merge own PRs after CI green, close issues that aren't worth doing. The
  audit trail is in PR descriptions and issue close-out comments, not
  pre-approval.

## Build & Development

```bash
# Build
go build ./...

# Run the full test suite — inside a container, isolated from the host
# tmux server and real AF home (see docs/container-testing.md)
make test-container

# Focused remote-hook integration harness — mock remote round-trip
# inside the same container fence (see docs/container-testing.md)
make remote-roundtrip-container

# Host-side runs: never bare `go test ./...` on a shared dev box — the
# daemon package spawns real af daemons. Skip it, or use the container.
go test $(go list ./... | grep -v '/daemon')

# TUI play-testing — containerized sandbox (throwaway home, mock repo,
# private tmux); see docs/container-testing.md
make playtest-container

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
# Must pass before PR merge
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
- `daemon/` — always-on background daemon: task scheduler, watcher supervisor, autoyes mode, control-socket RPCs, autostart unit
- `cmd/` — CLI command utilities
- `log/` — logging
- `docs/` — documentation (remote hooks, etc.)
- `examples/` — example configurations

## Conventions

- All Go files must be `gofmt`-formatted
- PRs target `master` branch
- Keep PRs focused and small
- Run `go build ./...` and `go test ./...` before submitting
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
