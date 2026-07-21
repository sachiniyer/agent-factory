# Release Notes

## Breaking: `auto_yes` removed

The `auto_yes` config key, `af --autoyes` / `-y`, and
`af agent-server --auto-yes` have been removed. A stale config, flag, or API
field now fails with migration guidance instead of silently doing nothing.
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
