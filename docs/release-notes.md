# Release Notes

## Watch-task restart

- `af tasks restart <id>` now reloads an edited watch script without manual
  process signals. It waits for the old process group and queue drainer to exit
  before starting one replacement, so restarts cannot double-emit events.
- Task write/reload and restart operations are serialized in the daemon. A
  disable returns only after its watcher is gone, and a project-scoped restart
  cannot race a concurrent task rebind.

## Breaking: `auto_yes` removed

The `auto_yes` config key, `af --autoyes` / `-y`, and
`af agent-server --auto-yes` have been removed. Existing config files that still
carry `auto_yes` load successfully, ignore it, and log migration guidance so an
upgrade cannot strand the daemon. New config writes, stale flags, and stale API
fields fail with that guidance instead of silently doing nothing.
Configure approval behavior in the agent itself; the exact recipe for every
supported agent is in [Agent approval
behavior](configuration.md#agent-approval-behavior). Existing persisted session
records carrying the old field still load, but the field is ignored and is not
written back.

## Keymap Changes

- Default TUI keys changed to ergonomic lower-case (`a/m/y/e`,
  `ctrl+u/ctrl+d`); restore any previous binding by pinning it in `[keys]` in
  `~/.agent-factory/config.toml`.

Previous default keys are not built-in aliases. To restore the old visible
keymap, add:

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
