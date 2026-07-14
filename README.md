# Agent Factory

[![Latest release](https://img.shields.io/github/v/release/sachiniyer/agent-factory?sort=semver)](https://github.com/sachiniyer/agent-factory/releases/latest)
[![Docs](https://img.shields.io/badge/docs-live-2e7c80)](https://sachiniyer.github.io/agent-factory/)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPL--3.0-2e7c80)](LICENSE.md)

[![Agent Factory demo video preview: multiple AI coding agents running in isolated git worktrees, with live Agent tabs, scheduled automations, helper tabs, and full-screen attach](docs/assets/demo.gif)](docs/assets/demo.mp4)

**Demo video:** [MP4](docs/assets/demo.mp4) · [WebM](docs/assets/demo.webm) · [GIF fallback](docs/assets/demo.gif)

Agent Factory (`af`) is a terminal UI for running many AI coding agents at once:
Claude Code, Codex, Aider, Gemini, and Amp. Each normal session gets its own git
worktree and branch, so parallel agents do not trample the same checkout. A
daemon keeps sessions and automations alive, while the TUI, JSON CLI, and local
HTTP API all read the same state.

Fork of [claude-squad](https://github.com/smtg-ai/claude-squad), extended with
per-repo scoping, task automation, remote hooks, a programmatic CLI, and a
branded docs site.

**Full docs:** [sachiniyer.github.io/agent-factory](https://sachiniyer.github.io/agent-factory/)

## Why Agent Factory

- **Worktree isolation:** one agent, one branch, one reviewable working tree.
- **One-screen supervision:** watch agents live, attach full-screen, and add
  helper tabs beside an agent in the same worktree.
- **Daemon-owned state:** sessions, tasks, usage-limit recovery, and APIs share
  one source of truth.
- **Automation:** cron tasks and watch scripts can create sessions or deliver
  prompts into existing ones.
- **Scriptable control:** `af sessions` and `af tasks` print JSON; the daemon
  also exposes a local HTTP/JSON API over a Unix socket.
- **Browser web client:** the same rail, live terminals, tabs, projects, and
  tasks in a browser. It is **bundled into the daemon** and on by default — start
  the daemon (any `af` command does) and open **<http://localhost:8443>**. No
  token, no login screen. Disable it with `listen_addr = ""`, or expose it to a
  network with a routable `listen_addr` — in which case set `require_token = true`
  or keep it behind a VPN/proxy, since auth is off by default. See the
  [web client guide](docs/web.md).
- **Remote hooks:** plug in your own launch/list/attach/delete scripts for
  remote session backends.

## Install

Prerequisites: **tmux**, **git**, and at least one supported agent CLI. No Go
toolchain is required for the prebuilt install path.

```bash
curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | sh
```

The installer places `af` in `~/.local/bin` by default. Override with
`AF_INSTALL_DIR`, pin with `--version`, or install from the
[latest release](https://github.com/sachiniyer/agent-factory/releases/latest).
Run `af upgrade` or rerun the script to update. Installed binaries auto-update
on the stable channel unless you opt into preview builds in config.

After install, run `af doctor --setup` to verify tmux, git, your configured
agent command, git identity, config/state/log storage, and daemon health before
creating your first session.

Build from source with Go 1.25+:

```bash
git clone https://github.com/sachiniyer/agent-factory.git
cd agent-factory
./dev-install.sh
```

## Quick Start

Run `af` inside a git repository:

```bash
cd your-project
af doctor --setup
af
```

Common TUI keys:

| Key | Action |
|---|---|
| `n` | Create a local worktree-backed session |
| `N` | Create a remote session when `remote_hooks` are configured |
| `Enter` | Interact with the selected tab in place |
| `Ctrl-]` | Leave in-pane interaction |
| `o` | Attach to the selected tab full-screen |
| `Ctrl-w` | Detach from a full-screen attach |
| `t` | Open a helper shell tab in the session worktree |
| `s` | Open the selected tab as a workspace pane |
| `S` | Commit a preview as a new workspace pane |
| `←` / `→` | Move focus between open workspace panes when a pane is focused |
| `a` | Archive or restore a session (default done action) |
| `D` | Permanently kill a session and clean up its worktree |
| `m` | Open the task manager |
| `y` | Copy the selected session's PR URL |
| `e` | Open the worktree hooks editor |
| `Ctrl-u` / `Ctrl-d` | Scroll the selected tab up/down |
| `Ctrl-p` | Switch the active project/repo without restarting |

Previous default keys are not built-in aliases. To restore the old visible
keymap, pin those bindings in `~/.agent-factory/config.toml`:

```toml
[keys]
archive = "A"
tasks = "S"
split_pane = "alt+s"
copy_pr = "P"
hooks = "H"
scroll_up = "shift+up"
scroll_down = "shift+down"
```

Everything the TUI does is also scriptable:

```bash
af sessions create --name fix-auth-bug --prompt "Fix the login redirect loop"
af sessions preview fix-auth-bug
af sessions tab-create fix-auth-bug --command "npm run dev"
af tasks add --name "Daily triage" --prompt "Triage open issues" --cron "0 9 * * *"
```

## Core Concepts

### Sessions And Tabs

Each normal session is a dedicated git worktree on its own branch. The agent
runs in that directory, helper tabs run beside it, and the branch remains a
normal git artifact you can diff, push, or turn into a pull request. `--here`
is available when you intentionally want an in-place session in the current
checkout instead.

### Tasks And Daemon

Tasks deliver prompts on a cron schedule or whenever a long-running watch script
prints a stdout line. A task can create a fresh session per fire or send the
prompt into a named target session. The background daemon hosts those schedules,
re-spawns sessions, drives auto-yes, and can park/resume Claude or Codex
sessions that hit plan usage limits.

### CLI And HTTP API

`af sessions` and `af tasks` emit JSON, so they compose cleanly with shell
scripts and other agents. The daemon exposes the same control surface as a local
owner-only HTTP/JSON API over `$AGENT_FACTORY_HOME/daemon-http.sock`.

```bash
curl --unix-socket ~/.agent-factory/daemon-http.sock http://localhost/v1/health
```

To drive the daemon from another machine, SSH in and run `af` there, or expose
the HTTP+token TCP listener to the network — it's on by default on loopback, so
this means pointing `listen_addr` at a routable host:port (`af token`,
`--daemon-url`). The listener is plain HTTP; put it behind a TLS-terminating
proxy or a private network — see [Remote daemon access](docs/remote-http-auth.md).

### Configuration

Configuration is TOML. Global defaults live in
`~/.agent-factory/config.toml`; repo-specific settings can live in
`.agent-factory/config.toml` inside the repository. Use `program_overrides` for
agent paths or flags.

```toml
default_program = "claude"
worktree_root = "sibling"

[program_overrides]
claude = "/home/me/.local/bin/claude --dangerously-skip-permissions"
```

## Platform Support

| Platform | TUI and sessions | Daemon autostart | Install |
|---|---|---|---|
| Linux | Supported and CI-tested | systemd user service | install script, tarball, source |
| macOS | Supported; release binaries are cross-compiled | launchd agent | install script, tarball, source |
| Windows via WSL2 | Supported as Linux inside WSL | requires systemd in the distro | install script inside WSL |
| Native Windows | Unsupported | unsupported | no binaries |

tmux is required on every supported platform. Native Windows is not a target;
use WSL2 and keep repositories on the Linux filesystem for best git/worktree
performance.

## Documentation

- [Documentation site](https://sachiniyer.github.io/agent-factory/) - overview,
  getting started, concepts, guides, and generated reference pages.
- [Comparison](docs/comparison.md) - Agent Factory vs. tmux/manual worktrees and
  peers such as Herdr.
- [CLI guide](docs/cli.md) and [generated CLI reference](docs/reference/cli.md).
- [HTTP API guide](docs/http-api.md) and [generated API reference](docs/reference/api.md).
- [Configuration](docs/configuration.md), [tasks](docs/tasks.md),
  [remote hooks](docs/remote-hooks.md),
  [remote daemon access](docs/remote-http-auth.md), and
  [usage limits](docs/usage-limits.md).
- [Container testing](docs/container-testing.md), [release process](docs/release-process.md),
  [release notes](docs/release-notes.md), and
  [release testing plan](docs/release-testing-plan.md).

## Maintenance

This repo is autonomously maintained by Captain Claude, an AI maintainer running
on Claude Code. The operating contract lives in [CLAUDE.md](CLAUDE.md).

When filing an issue, include steps to reproduce, expected vs. actual behavior,
`af version`, platform details, and logs when relevant. `af bug-report` gathers
versions, daemon health, task state, redacted session state, and the recent log
tail into a single report file for review.

## License

[GNU AGPL v3](LICENSE.md)
