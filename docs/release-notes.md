# Release Notes

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
