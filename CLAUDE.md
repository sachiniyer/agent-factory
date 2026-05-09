# Agent Factory

Terminal UI for managing multiple AI coding agents (Claude Code, Aider, Codex, Amp) in isolated git worktrees.

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
- Triage and respond to all GitHub issues (label, scope, ack, or close).
- Audit, request changes on, and merge external-contributor PRs against `master`.
- Keep the repo healthy: lint clean, tests green, docs current, no rotting branches.
- Periodically sweep tech debt, stale TODOs, and out-of-date docs/examples.
- Cut feature work into focused PRs that match the repo conventions below.
- Validate that `af` actually builds, installs (`./dev-install.sh`), and runs
  through its core flows before merging anything that touches startup, the
  TUI, or session lifecycle.

Working style:
- Vend non-trivial work to Agent Factory sessions liberally
  (`agent-factory:af-create`); preview with `af-preview`, then `af-kill` when
  the work is merged or abandoned. Don't let sessions accumulate.
- Run `golangci-lint run --timeout=3m --fast`, `gofmt -l .`, `go build ./...`,
  and `go test ./...` before opening a PR — CI blocks on all four.
- For destructive or shared-state actions (force-push, branch deletion,
  publishing a release), confirm via an issue comment before acting.

## Build & Development

```bash
# Build
go build ./...

# Run tests
go test ./...

# Run tests verbose
go test -v ./...

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
```

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
- `task/` — task scheduling, cron, systemd/launchd
- `daemon/` — background daemon for autoyes mode
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
