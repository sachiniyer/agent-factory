# Agent Factory

A terminal UI that runs multiple AI coding agents — Claude Code, Codex, Aider, Gemini — side by side, each in its own isolated git worktree. Start several agents on different tasks, watch their terminals live in the preview pane, attach to any of them, and automate the whole thing with scheduled tasks and a JSON CLI.

![Agent Factory demo: spinning up several AI coding agents, each in its own isolated git worktree, watching their work stream live in the preview pane, and attaching full-screen to one](docs/assets/demo.gif)

Fork of [claude-squad](https://github.com/smtg-ai/claude-squad) with per-repo scoping, a programmatic CLI, and automated tasks.

## Install

Prerequisites: **tmux**, **git**, and at least one AI coding agent (e.g. [Claude Code](https://docs.anthropic.com/en/docs/claude-code)). No Go required. Runs on Linux and macOS; on Windows, run it inside WSL — see [Platform support](#platform-support).

```bash
curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | sh
```

Installs the `af` binary (Linux/macOS, amd64/arm64) to `~/.local/bin` — override with `AF_INSTALL_DIR`, pin a release with `--version`. Re-run the script or `af upgrade` to update. Installed binaries also auto-update themselves along the stable channel; set `"update_channel": "preview"` in the global config to track preview builds instead (see [docs/release-process.md](docs/release-process.md)).

**Other ways:** grab a tarball from the [Releases page](https://github.com/sachiniyer/agent-factory/releases/latest), or build from source.

### Building from source

Needs **Go 1.24+**; installs to `~/.local/bin/af`:

```bash
git clone https://github.com/sachiniyer/agent-factory.git
cd agent-factory
./dev-install.sh
```

### Launch

```bash
cd your-project    # must be a git repo
af                 # launch the TUI
```

## Platform support

| Platform | TUI & sessions | Daemon autostart (`af daemon install`) | Install |
|----------|----------------|----------------------------------------|---------|
| Linux | ✅ Supported — where development and CI testing happen | ✅ systemd user service | `install.sh`, tarball, or source |
| macOS | ✅ Supported — expected to work; CI tests run on Linux only | ✅ launchd agent | `install.sh`, tarball, or source |
| Windows (WSL2) | ✅ Runs as Linux inside WSL | ⚠️ Requires systemd enabled in the distro | `install.sh` inside WSL |
| Windows (native) | ❌ Unsupported — does not build or run | ❌ | ❌ No binaries published |

Caveats:

- **tmux is load-bearing.** Every session, tab, and preview is a tmux session under the hood; `af` does not work without `tmux` on `PATH`, on any platform.
- **Native Windows is not a target.** The code depends on tmux and Unix-only syscalls (process-group kills, `SIGWINCH`, `flock`) and does not compile for `GOOS=windows`; no Windows binaries are published, and `af upgrade`/auto-update refuse to run there. Use WSL.
- **WSL:** `af daemon install` writes a systemd user unit, so your distro must have systemd enabled (the default on current WSL2; otherwise set `systemd=true` under `[boot]` in `/etc/wsl.conf`). Without it the daemon still starts on demand whenever `af` runs, but scheduled tasks stop firing once the WSL VM shuts down. Keep repos on the Linux filesystem (not `/mnt/c`) for git and worktree performance.
- **macOS is cross-compiled.** Release binaries are published for macOS (amd64/arm64) and the macOS-specific code paths (launchd, `open`, `pbcopy`) are unit-tested, but CI does not run on real macOS hardware.
- **Clipboard and browser keys** shell out per platform: `open`/`pbcopy` on macOS, `xdg-open` plus `wl-copy` or `xclip` on Linux and WSL. Copying a PR URL (`P`) needs one of those clipboard tools installed.
- **Prebuilt binaries are amd64/arm64 only.** On other architectures, build from source.

## Core features

Press `?` inside the TUI for the full keybindings list.

### Sessions

Each session is one agent running in its own git worktree on its own branch — agents never step on each other's changes or on your working tree. Sessions persist across restarts, and everything is scoped to the current repo: the TUI only shows sessions for the repository you launched it in.

| Key | Action |
|-----|--------|
| `n` | Create a new session |
| `Enter` | Interact with the session in its pane — every key (including `Tab`) goes to the agent; the sessions rail stays visible |
| `Ctrl-]` | Leave interactive mode, back to navigation |
| `o` | Attach to the selected session's active tab full-screen |
| `Ctrl-w` | Detach from a full-screen attach (configurable via `detach_keys`) |
| `D` | Kill the session and clean up its worktree |
| `A` | Archive the session (tmux down, worktree moved out, restartable) — on an archived row, restore it |
| `Tab` / `Shift-Tab` | Cycle focus forward / back: tree → open panes → automations |
| `1`–`9` | Jump straight to a tab by number |
| `t` | Open a new shell tab in the session's worktree |
| `w` | Close the active tab (the agent tab can't be closed) |

When a session's branch has an open pull request, `p` opens it in the browser and `P` copies its URL.

Press `A` to **archive** a session you're done with for now: its tmux is torn down and its worktree is moved out to a global archive directory (branch and uncommitted changes preserved), and the row drops into a collapsed **Archived** folder at the bottom of the rail. Archived sessions survive restarts and are never auto-restored — press `A` again on an archived row (or run `af sessions restore <title>`) to move its worktree back and bring the agent up. Not available for remote or `--here` sessions, which don't own a relocatable worktree.

#### Tabs

Every session opens with a single **agent** tab (the AI agent, shown as *Preview*). Press `t` to spawn **shell** tabs (*Terminal*) running `$SHELL` in the worktree (up to nine tabs per session), `w` to close the active one, and `1`–`9` to jump between them. `Enter` types into whichever tab is active, in place (`o` for a full-screen attach). Tabs are ephemeral but persisted: they survive an `af`/daemon restart, reconnecting to their live processes. You can also spawn a tab running an arbitrary command from the CLI with `af sessions tab-create`, and delete a single tab with `af sessions tab-delete` (see below).

Remote sessions are tab-driven too, with one limitation: the hook protocol can't run arbitrary commands on the remote host, so a remote session has an agent tab always and a single terminal tab **only when** its repo configures `remote_hooks.terminal_cmd` (`t`, `tab-create`, and `tab-delete` are rejected for remote sessions). See [docs/remote-hooks.md](docs/remote-hooks.md).

### Tasks

Tasks deliver a prompt to an agent automatically — on a cron schedule, or every time a long-running watch script emits a stdout line (e.g. a script polling for new GitHub issues). Each fire either creates a fresh session or sends the prompt into an existing one. In the TUI: `S` opens the task manager, `n` creates a task, `r` runs a cron task now.

Tasks are hosted by a background daemon that starts on demand; run `af daemon install` once to keep them firing across reboots. See [docs/tasks.md](docs/tasks.md) for the full trigger × delivery matrix and the watch-script contract.

### Usage limits

When a `claude` or `codex` session hits a plan usage-limit wall, af detects the banner, marks the row `[limit]` in the sidebar (with the reset time when the banner states one), and keeps the session parked rather than treating it as dead. Press **`c`** on the session to resume it immediately; a task-driven session re-sends its stored prompt so it picks up where it left off.

Opt into hands-off recovery with `limit_auto_resume = true` (global config): the daemon then resumes a parked `claude`/`codex` session on its own once its limit window elapses. Task runs that hit a limit at startup are **parked, not failed** — recorded as waiting for the window and resumed to completion, never counted as an errored run. `gemini`/`aider` are API-key-metered (transient 429s), so they are out of scope. See [docs/usage-limits.md](docs/usage-limits.md).

### JSON CLI

Everything the TUI does is scriptable — `af sessions` and `af tasks` print JSON, so other tools (or other agents) can orchestrate Agent Factory:

```bash
af sessions create --name fix-auth-bug --prompt "Fix the login redirect loop"
af sessions preview fix-auth-bug
af sessions tab-create fix-auth-bug --command "btop"   # spawn a process tab in the worktree
af sessions tab-delete fix-auth-bug --name btop        # delete that tab again (agent tab can't be deleted)
af tasks add --name "Daily triage" --prompt "Triage open issues" --cron "0 9 * * *"
```

See [docs/cli.md](docs/cli.md) for the complete command reference.

### HTTP API

The daemon also exposes the same session and task operations as a small JSON API over a **localhost-only Unix socket** (`$AGENT_FACTORY_HOME/daemon-http.sock`, `0600` owner-only — no TCP port, no token), so tools and agents can call Agent Factory without shelling out to `af`. Every response uses the same `{data, error}` envelope as `af --json`:

```bash
curl --unix-socket ~/.agent-factory/daemon-http.sock http://localhost/v1/health
# {"data":{"ok":true},"error":null}
```

Run `af api` to list every endpoint (with a ready-to-run curl example) and the resolved socket path; see [docs/http-api.md](docs/http-api.md) for the full reference.

### Remote sessions

With `remote_hooks` configured in a repo, `N` launches sessions on a remote backend (your own scripts: launch, list, attach, delete, terminal) and shows them alongside local ones with the same attach/kill/preview experience. See [docs/remote-hooks.md](docs/remote-hooks.md) for the script protocol.

### Root agent (always-ensured)

Opt a repository into an always-on **root agent** — a reserved session titled `root` that the daemon keeps alive, attached in-place at the repo root (no worktree; killing it never touches your working tree) and re-created automatically if its tmux dies:

```toml
[root_agents]
"/home/me/myrepo" = {}
```

Strictly opt-in via your global `~/.agent-factory/config.toml` (never from an in-repo config, so a cloned repo can't spawn one), and the default profile runs `claude --dangerously-skip-permissions` with auto-yes — see [docs/configuration.md](docs/configuration.md#root-agents-always-ensured) before opting in. The name `root` is reserved: normal session creation rejects it.

### Configuration

Config is [TOML](https://toml.io), for easy hand-editing. Global defaults live in `~/.agent-factory/config.toml`; a repo can check in its own `.agent-factory/config.toml` that overrides them (and is the only place repo-specific keys like `remote_hooks` and `post_worktree_commands` may live):

```toml
default_program = "claude"

[program_overrides]
claude = "/home/me/.local/bin/claude --dangerously-skip-permissions"
```

Read and write the global config from the CLI: `af config list` / `af config get <key>` show the effective values, and `af config set <key> <value>` writes a single settable key in place — preserving every comment and your ordering (it never regenerates the file), validated with the loader's own rules first. Structural keys (`root_agents`, `[keys]`) stay hand-edited. Upgrading from a `config.json`? The move to TOML is automatic — `af` converts it on first run and keeps a `config.json.bak`. See [docs/configuration.md](docs/configuration.md) for every key, the migration, the precedence rules, and where state is stored.

## Documentation

- [docs/cli.md](docs/cli.md) — full CLI reference (`af sessions`, `af tasks`, `af daemon`, maintenance commands)
- [docs/http-api.md](docs/http-api.md) — the daemon-hosted HTTP/JSON API: socket path, auth model, every endpoint (`af api` prints the same catalog)
- [docs/configuration.md](docs/configuration.md) — config keys, global vs. in-repo precedence, state locations
- [docs/tasks.md](docs/tasks.md) — task triggers, the watch-script contract, daemon lifecycle
- [docs/usage-limits.md](docs/usage-limits.md) — usage-limit detection, the `[limit]` badge, manual retry, opt-in auto-resume, and task park-don't-fail
- [docs/remote-hooks.md](docs/remote-hooks.md) — remote backend script protocol
- [docs/release-process.md](docs/release-process.md) — release channels: manual stable `1.x.y` (the auto-update default), opt-in auto preview `1.x.y-preview-z`
- [docs/release-testing-plan.md](docs/release-testing-plan.md) — release gate and smoke checks
- [docs/container-testing.md](docs/container-testing.md) — running the test suite and TUI play-tests safely in a container (`make test-container` / `make playtest-container`)
- [examples/](examples/) — runnable task watch-scripts and remote-hook skeletons

## Maintenance

This repo is autonomously maintained by Captain Claude, an AI maintainer running on Claude Code; the operating contract lives in [CLAUDE.md](CLAUDE.md). Every open issue gets a response — a fix, specific questions, or a reasoned close — and merged work reaches the preview channel (`1.x.y-preview-z`) from `master` every 3 hours; stable releases (`1.x.y`) are cut manually (see [docs/release-process.md](docs/release-process.md)).

When filing an issue, include: steps to reproduce, expected vs. actual behavior, `af version` output and your platform, and (when relevant) logs from the application log (see `docs/configuration.md` for path resolution). The fastest way to gather all of that is `af bug-report`, which bundles the log tail, versions, tasks, redacted session state, and daemon health into a single `~/af-bug-report-<ts>.txt` you can attach — redaction is best-effort, so review the file before sharing it.

## License

[GNU AGPL v3](LICENSE.md)
