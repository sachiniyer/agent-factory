# Getting started

For a developer who has not run `af` before. After this page you will have it
installed, one agent working in its own git worktree, and the same session open
in your browser — in about five minutes.

## Prerequisites

- **tmux** and **git** on your `PATH`.
- At least one AI coding agent installed — e.g.
  [Claude Code](https://docs.anthropic.com/en/docs/claude-code), Codex, Aider,
  Gemini, Amp, opencode, or Devin.
- Linux or macOS. On Windows, run it inside WSL2.

No Go toolchain is required to install `af` — releases ship prebuilt binaries.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | sh
```

This installs the `af` binary (Linux/macOS, amd64/arm64) to `~/.local/bin` —
override with `AF_INSTALL_DIR`, or pin a release with `--version <tag>`. Make
sure `~/.local/bin` is on your `PATH`.

Building from source instead? Clone the repo and run `./dev-install.sh` (this
needs Go 1.25+).

You rarely need to update by hand: installed binaries auto-update on launch
along the **stable** channel by default, at most once every 6 hours, and
relaunch you into the new version on the spot. Set
`update_channel = "preview"` in your global config to track preview builds
instead, or `auto_update = false` to pin what you have (see the
[release process](dev/release-process.md)). To update on demand anyway, run
`af upgrade`.

## Check the setup

```bash
cd your-project    # a git repo
af doctor --setup
```

The setup profile checks exactly what creating a first session needs: AF home
writability, config materialization and parsing, git and this repo, git
identity, tmux, your configured agent commands, state and log storage, daemon
health, and remote-hook setup when the repo configures one. Anything it reports
is worth fixing before the next step — see [Troubleshooting](troubleshooting.md).

## Your first session

Sessions run in git worktrees, so most of the time you start inside a
repository — that repo becomes the default project:

```bash
af                 # launch the TUI, scoped to this project
```

You can also run `af` from anywhere: outside a git repository it opens on your
project registry so you can pick a known project (or add one). Being in a repo
just picks that repo for you. Press **`ctrl+p`** from TUI navigation to switch
projects without restarting; while you are attached to a pane, `ctrl+p` goes to
the running program, so press `ctrl+]` to detach first.

The TUI opens with an empty sidebar. From here:

1. Press **`n`** to create a new session. Give it a name and, optionally, choose
   the agent with **Tab**. `af` creates a fresh git worktree on a new branch and
   starts your agent in it.
2. The session appears in the sidebar with a live status. The **Agent tab** on
   the right shows a snapshot of the agent's terminal — you can watch its
   progress without attaching.
3. Press **`↵`** (Enter) to **interact** with the selected agent right in the
   pane, or **`o`** to **attach** full-screen. From an in-pane interaction,
   **`ctrl+]`** returns you to navigation mode; from a full-screen attach, the
   tmux detach key drops you back to the sidebar. Either way the agent keeps
   running. Use **`s`** to open a selected tab as a workspace pane; when a pane
   has focus, **`←`** / **`→`** move focus between open panes.
4. When you are done with a session, **`a`** archives it: tmux is torn down, the
   worktree is moved aside, and the session can be restored later. **`D`**
   permanently kills a session and removes its worktree and branch, including
   any uncommitted or unmerged work. If a session is marked Lost or Dead after a
   crash, reboot, or missing worktree, select it and press **`r`** (or run
   `af sessions restore <title>`) to recover it and resume its recorded agent
   conversation when possible.

Because each session is a real git branch, reviewing and merging an agent's work
is just your normal git/PR flow.

## Open it in a browser

The same daemon that runs your sessions serves a full web client. It is on by
default, on loopback, with no token and no login screen:

**<http://localhost:8443>**

You get the same session rail, live terminals, tabs, tasks, and config — plus
things a terminal cannot do, like a VS Code tab rooted at a session's worktree.
See [the web client](web.md) for the tour, and
[remote daemon access](remote-http-auth.md) before exposing it to anything
beyond your own machine.

## The same thing from the CLI

Everything the TUI and the web client do is also scriptable. The `af sessions`
and `af tasks` command groups print JSON to stdout, so they compose with `jq`:

```bash
af sessions create --name fix-auth-bug --prompt "Fix the login redirect loop"
af sessions list
af sessions preview fix-auth-bug          # snapshot its terminal
af sessions watch fix-auth-bug            # block until it goes idle
af sessions tab-create fix-auth-bug --command "npm run dev"
af sessions archive fix-auth-bug          # finish with it, restorable later
```

Schedule an agent to run on its own:

```bash
af tasks add --name "Daily triage" --prompt "Triage open issues" --cron "0 9 * * *"
```

Scheduled and event-driven tasks are run by the background **daemon**, which
starts on demand whenever there is work to host. To keep it — and your tasks —
running across logouts and reboots, install its autostart unit once:

```bash
af daemon install
```

## Next steps

- [Concepts](concepts.md) — the five terms the rest of the docs assume.
- [The web client](web.md) · [the TUI](tui.md) · [the CLI](cli.md) — pick the
  surface you want to drive it from.
- [Configuration](configuration.md) — choosing agents, global vs. in-repo
  config, and every key.
- [Troubleshooting](troubleshooting.md) — when something looks wrong.
