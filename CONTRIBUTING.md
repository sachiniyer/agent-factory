# Contributing to Agent Factory

Thank you for your interest in contributing!

## Development Setup

```bash
git clone https://github.com/sachiniyer/agent-factory.git
cd agent-factory
go build -o af .
```

### Prerequisites

- Go 1.24+
- tmux
- git

### Running Tests

```bash
go test ./...
```

### Building & Installing

```bash
./dev-install.sh
```

This builds the `af` binary and installs it to `~/.local/bin/`. Override the install directory:

```bash
BIN_DIR=/usr/local/bin ./dev-install.sh
```

## Project Structure

| Directory | Description |
|-----------|-------------|
| `app/` | TUI application (bubbletea), main event loop |
| `ui/` | UI components: sidebar, kanban, terminal, overlays |
| `session/` | Instance lifecycle (start, pause, resume, kill) |
| `session/git/` | Git worktree operations |
| `session/tmux/` | Tmux session management |
| `schedule/` | Scheduled tasks (cron, systemd timers) |
| `task/` | Per-repo kanban board |
| `config/` | Configuration and state persistence |
| `api/` | Programmatic JSON CLI API |
| `daemon/` | Background daemon for auto-yes |
| `microclaw/` | MicroClaw integration bridge |
| `keys/` | Key bindings |

## Guidelines

- Run `go test ./...` before submitting
- Run `gofmt` on changed files
- Keep changes focused and minimal
- Add tests for new functionality
- Update the README if you change user-facing behavior

## Submitting Changes

1. Fork the repo and create a branch
2. Make your changes
3. Run tests: `go test ./...`
4. Submit a pull request

## License

By contributing, you agree that your contributions will be licensed under the GNU AGPL v3.
