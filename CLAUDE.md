# Agent Factory

Terminal UI for managing multiple AI coding agents (Claude Code, Aider, Codex, Amp) in isolated git worktrees.

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
- `session/` — session management, backend, plugins
- `session/git/` — git worktree operations, GitHub integration
- `session/tmux/` — tmux PTY integration
- `config/` — configuration and state management
- `api/` — REST/JSON API for sessions and tasks
- `task/` — task scheduling, cron, systemd/launchd
- `daemon/` — background daemon for autoyes mode
- `cmd/` — CLI command utilities
- `log/` — logging

## Conventions

- All Go files must be `gofmt`-formatted
- PRs target `master` branch
- Keep PRs focused and small
- Run `go build ./...` and `go test ./...` before submitting
- Version is stored in `main.go` (`version` var) and auto-bumped by CI
