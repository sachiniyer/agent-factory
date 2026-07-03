# Spike: embedded interactive terminal pane (#1089)

Throwaway proof-of-concept for rendering a **live, interactive tmux session
inside a non-fullscreen bubbletea pane** while an instances rail stays
visible. See `tmp_docs/spike-1089-embedded-terminal.md` for the full report.

**Do not merge.** Single-file demo, no tests, not wired into the real TUI.

## Run it

```bash
go run ./spike
```

The demo creates (if needed) and attaches to session `work` on an **isolated
tmux server** (`tmux -L spike1089`) — it never touches your default tmux
server or the agent-factory daemon.

Keys:

| key      | action                                            |
|----------|---------------------------------------------------|
| `enter`  | attach / re-attach the embedded terminal          |
| `tab`    | switch host focus between rail and terminal       |
| `ctrl-]` | detach (tmux session keeps running server-side)   |
| `q`      | quit (rail focus only)                            |

While the terminal pane is focused, every other key is forwarded into the
tmux session. Run `vim`, `less`, `htop`, `yes` in there — they render inside
the pane.

## Architecture (A from #1089)

```
tmux server (-L spike1089)
  └─ tmux attach-session  ← child process on a PTY (creack/pty)
       PTY output → charmbracelet/x/vt emulator (cell grid)
       emulator.Read (query replies + encoded keys) → PTY input
       cell grid → ANSI string → lipgloss box → bubbletea View()
       tea.KeyMsg → uv.KeyPressEvent → emulator.SendKey (mode-aware encoding)
       pane resize → pty.Setsize + emulator.Resize → tmux reflows
```

Cleanup after experimenting: `tmux -L spike1089 kill-server`
