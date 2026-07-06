# The TUI

The `af` terminal UI is the home base for running agents. It's built to keep you
in one screen: a list of everything running on the left, a live look at any
agent on the right, and a keystroke to dive into any of them full-screen.

Launch it from inside a git repository:

```bash
cd your-project
af
```

## Layout

- **Sidebar.** Every session, with a live status indicator (running, waiting on
  you, hit a usage limit, dead-and-being-recovered). Sessions are grouped and
  navigable with the arrow keys or `j`/`k`.
- **Agent tab.** A snapshot of the selected agent's terminal, updated as it
  works — so you can follow progress across several agents without attaching to
  any of them. Toggle it with `s` (open) and `x` (hide).

## Interacting with a session

There are two ways in, split deliberately:

- **`↵` (Enter) — interact in-pane.** Type to the selected agent directly inside
  the layout, without a full-screen takeover. `Ctrl-]` returns you to navigation
  mode.
- **`o` — attach full-screen.** Hand the whole terminal to the session's tmux.
  The tmux detach key returns you to the sidebar. The agent keeps running either
  way.

## Tabs

A session isn't limited to its agent. Each one can hold up to nine **tabs**, all
running in the *same* worktree:

- **`t`** spawns a new tab — a shell, or any command you name (a dev server,
  `btop`, a test watcher). It runs alongside the agent, sharing its files.
- **`w`** closes the focused tab (the agent's own tab can't be closed — kill the
  session instead).
- **`1`–`9`** jump straight to a tab by number.

Tabs persist across restarts, and each is a real process the daemon tracks.
(Remote sessions are more limited — see [Remote hooks](../remote-hooks.md).)

## Working with results

- **`p`** opens the session's pull request; **`y`** copies its URL.
- **`e`** runs the repo's worktree hooks.
- **`/`** searches; **`?`** shows the full, live help overlay.

## Sessions, tasks, and other surfaces

- **`n`** creates a new local session; **`N`** creates a remote one.
- **`D`** kills the selected session; **`a`** archives it (or restores an
  archived one).
- **`m`** opens the tasks view to manage [scheduled and event-driven
  automations](../tasks.md).
- **`c`** retries a session that's parked on a [usage limit](../usage-limits.md).
- **`Ctrl-u`** / **`Ctrl-d`** scroll the current tab up and down.

## Key bindings are yours

Every binding above is the default. You can rebind almost all of them in the
`[keys]` table of your config — a handful of structural keys (Enter, Tab,
`Ctrl-]`, the `1`–`9` tab jumps) are reserved. Run:

```bash
af keys
```

to print every action with its **effective** binding (default or your rebind).
See [Configuration → key bindings](../configuration.md#key-bindings-keys) for how
to set them, including how to restore pre-#1027 keys, and the
[CLI reference](../reference/cli.md#af-keys) for the command.
