# Release Notes

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

## Session environment isolation

- New and respawned agent panes now inherit a conservative allowlist instead of
  the launching user's entire environment. Runtime basics, Git/GitHub auth,
  proxies/custom CAs, and authentication for the selected supported agent keep
  working; unrelated variables are denied by default.
- If a workflow relied on another environment variable, add its exact name to
  the global `session_env_passthrough` list. This is an intentional behavior
  change. Existing panes keep their original environment until restarted.
- Docker forwards approved host variables by name only when the repo-selected
  image exactly matches a global immutable-digest grant; mutable/ungranted
  images fail before Docker is invoked. Host credential files and path-backed
  agent state are never copied, HTTP origins with embedded credentials are
  rejected without echoing them, unapproved Docker client-config proxies are
  explicitly cleared, and credential forwarding cannot be combined with
  repository-controlled `docker.run_args`. SSH uses matching variables from
  the remote account without copying local values, and hook scripts run under
  the same filter and receive repeated `--session-env <name>` arguments to pass
  to their remote `af agent-server`.
- Local Git worktree subprocesses and checked-in `post_worktree_commands` also
  use the filtered environment. Package/build credentials needed by those
  commands must be named explicitly in `session_env_passthrough`.
- Claude cloud-provider credentials selected by a command-local
  `CLAUDE_CODE_USE_*` assignment are admitted only for one literal Claude
  invocation. Compound commands, redirects, arbitrary wrappers, and dynamic
  words must use an exported selector or explicit pass-through names.

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
