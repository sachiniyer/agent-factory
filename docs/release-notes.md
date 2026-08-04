# Release Notes

## A healed root agent keeps its conversation

- When the root agent's tmux vanished, the daemon re-created it as a **brand-new
  agent with no history**. Every other session that loses its tmux is re-spawned
  with `--resume`, so the one always-on session — the target of every watch and
  monitor delivery — was the only one that silently started over. It now carries
  its recorded conversation across the re-create and comes back on it.
- Nothing about the always-ensured guarantee changed: if the recorded
  conversation cannot be resumed (the configured root program runs a different
  agent, it pins its own resume flag, or the provider no longer has that
  conversation), the root is created fresh rather than left down.
- The application log now distinguishes the outcomes — resumed its prior
  conversation, had none recorded, or did not come up on it — so a root that
  restarted without its history is legible instead of reading exactly like one
  that kept it.

## Config changes apply on save

- Saving a config change via `af config set` or the web config editor now applies
  it to a running daemon in place — no manual `af daemon restart`, and no session
  loss. Each surface says exactly when the change takes effect rather than telling
  you to run a command; only `root_agents`/`root_agent` and `branch_prefix` still
  take effect on the next daemon start.
- The network listener keys now apply live too. `require_token`,
  `require_loopback_token`, and `cors_allowed_origins` are read per request, so a
  change takes effect on the next request — and a security *tightening* applies even
  if a listener rebind fails, because the two are deliberately independent. A
  `listen_addr` or `preview_listen_addr` change rebinds the listener in place,
  binding the new address **before** closing the old one: if the new bind fails
  (port taken, unbindable address), the daemon keeps serving on the previous address
  and tells you why, rather than leaving itself unreachable. Making the control API
  reachable without a token (a non-loopback `listen_addr` with `require_token =
  false`) is warned about at save time and still binds — it is never refused.
- **Behavior change — `session_env_passthrough`:** this key's grants used to be
  re-read from disk on every session create, so a raw hand-edit of `config.toml`
  was picked up by the next create with no apply step — a liveness no other key
  had. It now applies the same way every other key does: on a save through
  `af config set`, the web editor, or the config assistant. A raw hand-edit of
  `config.toml` that bypasses those surfaces therefore needs one of them (or a
  daemon restart) to take effect. This trades one key's accidental liveness for a
  single uniform rule; a later change that watches `config.toml` will make
  hand-edits live for every key — strictly better than today for all of them.

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
carry `auto_yes` load successfully and ignore it, so an upgrade cannot strand
the daemon. `auto_yes = true` also logs migration guidance, because behavior
that config asked for stopped happening; `auto_yes = false` loads silently,
since the post-removal default is already "don't auto-approve". New config
writes, stale flags, and stale API fields fail with that guidance instead of
silently doing nothing.
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
