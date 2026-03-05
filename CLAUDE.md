# Claude Squad

## Building & Installing

Build and install the `cs` binary from local source:

```bash
./dev-install.sh
```

This builds with `go build` and copies the binary to `~/.local/bin/cs`. Override the install directory with `BIN_DIR`:

```bash
BIN_DIR=/usr/local/bin ./dev-install.sh
```

To build without installing:

```bash
go build -o cs .
```

## Running Tests

```bash
go test ./...
```

## Project Structure

- `app/` — TUI application (bubbletea), main event loop in `app.go`, help screens in `help.go`
- `ui/` — UI components: list, menu, tabbed window, terminal pane
- `ui/overlay/` — Overlay components: text input, confirmation, task list, schedule list, selection
- `session/` — Instance lifecycle (start, pause, resume, kill)
- `session/git/` — Git worktree operations (create, remove, commit, push)
- `session/tmux/` — Tmux session management
- `schedule/` — Scheduled tasks (cron, systemd timers)
- `task/` — Simple per-repo task list
- `config/` — App configuration and state persistence
- `keys/` — Key bindings
- `daemon/` — Background daemon
- `microclaw/` — MicroClaw integration
- `api/` — HTTP API for external control
