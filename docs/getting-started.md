# Getting started

## Prerequisites

- **tmux** and **git** on your `PATH`.
- At least one AI coding agent installed — e.g.
  [Claude Code](https://docs.anthropic.com/en/docs/claude-code), Codex, Aider,
  or Gemini.
- Linux or macOS. On Windows, run it inside WSL2.

No Go toolchain is required to install `af` — releases ship prebuilt binaries.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | sh
```

This installs the `af` binary (Linux/macOS, amd64/arm64) to `~/.local/bin` —
override with `AF_INSTALL_DIR`, or pin a release with `--version`. Make sure
`~/.local/bin` is on your `PATH`.

Run `af doctor --setup` after install to verify tmux, git, your configured
agent command, git identity, config/state/log storage, and daemon health.

To update later, re-run the script or run `af upgrade`. Installed binaries also
auto-update along the **stable** channel; set `"update_channel": "preview"` in
your global config to track preview builds instead (see the
[release process](release-process.md)).

Building from source instead? Clone the repo and run `./dev-install.sh` (this
needs Go).

## Your first session

`af` operates on a git repository, so start inside one:

```bash
cd your-project    # must be a git repo
af doctor --setup  # optional but recommended on first run
af                 # launch the TUI
```

The TUI opens with an empty sidebar. From here:

1. Press **`n`** to create a new session. Give it a name and, optionally, an
   choose the agent with **Tab**. `af` creates a fresh git worktree on a new
   branch and starts your agent in it.
2. The session appears in the sidebar with a live status. The **Agent tab**
   on the right shows a snapshot of the agent's terminal — you can watch its
   progress without attaching.
3. Press **`↵`** (Enter) to **interact** with the selected agent right in the
   pane, or **`o`** to **attach** full-screen. From an in-pane interaction,
   **`Ctrl-]`** returns you to navigation mode; from a full-screen attach, the
   tmux detach key drops you back to the sidebar. Either way the agent keeps
   running.
4. When you're done with a session, **`D`** kills it and removes its worktree
   and branch, or **`a`** archives it to set it aside and restore it later.

Because each session is a real git branch, reviewing and merging an agent's work
is just your normal git/PR flow.

## Doing the same thing from the CLI

Everything the TUI does is scriptable. The `af sessions` and `af tasks` command
groups print JSON to stdout, so they compose with `jq` and shell:

```bash
af sessions create --name fix-auth-bug --prompt "Fix the login redirect loop"
af sessions list
af sessions preview fix-auth-bug          # snapshot its terminal
af sessions tab-create fix-auth-bug --command "npm run dev"   # a process tab in the worktree
af sessions attach fix-auth-bug           # attach interactively
af sessions kill fix-auth-bug             # tear it down
```

Schedule an agent to run on its own:

```bash
af tasks add --name "Daily triage" --prompt "Triage open issues" --cron "0 9 * * *"
```

See the [CLI reference](reference/cli.md) for every command and flag, and
[Tasks & automation](tasks.md) for schedules and watch scripts.

## Keeping automations running across reboots

Scheduled and event-driven tasks are run by a background **daemon**, which starts
on demand whenever there's work to host. To keep it (and your tasks) running
across logouts and reboots, install its autostart unit once:

```bash
af daemon install
```

See [The daemon](concepts/daemon.md) for what it owns and why.

## Next steps

- [Concepts: worktree-isolated agents](concepts/worktree-agents.md) — the core
  model.
- [The TUI](concepts/tui.md) — the sidebar, Agent tab, tabs, and key bindings.
- [Configuration](configuration.md) — choosing agents, global vs. in-repo
  config, and every key.
