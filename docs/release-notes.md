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

## Session environment isolation

- New and respawned agent panes now inherit a conservative allowlist instead of
  the launching user's entire environment. Runtime basics, Git/GitHub auth,
  proxies/custom CAs, and authentication for the selected supported agent keep
  working; unrelated variables are denied by default.
- If a workflow relied on another environment variable, add its exact name to
  the global `session_env_passthrough` list. This is an intentional behavior
  change. Existing panes keep their original environment until restarted.
- Docker forwards only names explicitly granted through
  `session_env_passthrough`: repo config selects the image, so built-in agent,
  GitHub, proxy, and CA variables are not trusted across that boundary by
  default. SSH uses matching built-in variables from the remote account without
  copying local values, and hook scripts run under the same filter and receive
  repeated `--session-env <name>` arguments to pass to their remote
  `af agent-server`.
- Local Git worktree subprocesses and checked-in `post_worktree_commands` also
  use the filtered environment. Package/build credentials needed by those
  commands must be named explicitly in `session_env_passthrough`.
- Claude cloud-provider credentials selected by a command-local
  `CLAUDE_CODE_USE_*` assignment are admitted only for one literal Claude
  invocation. Compound commands, redirects, arbitrary wrappers, and dynamic
  words must use an exported selector or explicit pass-through names.
- **No agent inherits cloud-infrastructure credentials by default.** Which agent
  a session runs is repo-settable (`default_program`, `program_overrides`), and
  swapping the program is legitimate — so a swap must not also be a credential
  grant. Gemini's `GOOGLE_APPLICATION_CREDENTIALS`, `GOOGLE_CLOUD_PROJECT`, and
  `GOOGLE_CLOUD_LOCATION` now follow its own `GOOGLE_GENAI_USE_VERTEXAI` /
  `GOOGLE_GENAI_USE_GCA` selectors, on the same terms as Claude's. OpenCode no
  longer receives AWS credentials or Google application-default credentials at
  all: it has no environment variable that selects a cloud provider, so there is
  nothing to gate them behind.
- **Action required if you run OpenCode against Bedrock or Vertex**: list the
  exact credential names you need in the global `session_env_passthrough` (for
  example `AWS_PROFILE`, `AWS_REGION`, and whichever of `AWS_ACCESS_KEY_ID` /
  `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` or
  `AWS_SHARED_CREDENTIALS_FILE` your setup uses). Aider is unaffected — its
  Azure entries are Azure OpenAI service keys, not cloud credentials.

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
