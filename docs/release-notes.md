# Release Notes

## A remote hook can return an ssh host instead of an endpoint

- **New `remote_hooks.provision_cmd`.** Your script makes a machine and prints
  `{"host","host_key"}`; af reaches it over the operator's own ssh and runs the
  `af agent-server` itself. The bearer token never enters your script, there is
  no tunnel to keep alive, and no agent-server lifecycle to manage — the three
  jobs that produced most of this backend's sharp edges.
- **`host_key` is required, and that is the point.** A machine created seconds
  ago has no `known_hosts` entry, and no existing host-key posture can help
  without either refusing the launch or trusting the first key it sees. Your
  script is the only party with an authentic channel to the key, so it returns
  it and af pins it per session. There is deliberately no opt-out.
- **`launch_cmd` is unchanged and not deprecated** — it remains the escape hatch
  for targets with no sshd. A repo sets one or the other; setting both is
  rejected rather than silently resolved.

## One session's terminal history no longer shows up in another session's editor

- **A confidentiality fix.** code-server is VS Code *Web*, and it keeps its integrated
  terminal history in the **browser's** storage, under a key that is global rather than
  per-workspace. Because every session's editor was framed on the same origin, they all
  shared one store — so *Terminal: Run Recent Command* in one session offered **another
  session's commands**, and *Go to Recent Directory* offered another session's checkout.
  Command lines routinely carry branch names, paths and occasionally secrets, and the
  entries look exactly like your own, so this was unlikely to ever be reported as a bug.
- Setting `preview_listen_addr` now gives **each session's editor its own origin**, which
  is what makes the browser keep those stores apart. The origin is **stable across daemon
  restarts** (an editor's layout and history live behind it, so a rotating name would
  wipe them), derived from a 0600 secret in the af home.
- **The shared user-data directory is untouched** — settings, extensions and themes still
  carry across sessions, which is the reason it is shared.
- Two things to expect: **existing editor state does not migrate** (it is the shared store
  being fixed, so layout and history start empty once), and this is **same-machine only** —
  a remote viewer keeps the shared-origin editor. See
  [Web UI → per-tab preview origins](web.md#per-tab-preview-origins).

## Breaking: a remote hook's `launch_cmd` owns stdout

- **`launch_cmd`'s stdout now carries its `{"url","token"}` endpoint JSON and
  nothing else.** Surrounding whitespace is fine; a progress line, a backgrounded
  tunnel's log, or a second JSON record is not, and fails the provision with an
  error quoting the offending output and naming the fix. Only the
  **remote-hook** backend is affected — `local`, `docker`, and `ssh` sessions do
  not run these scripts.
- **Migration, one redirect per writer:** give everything except the endpoint
  somewhere else to go — `mytunnel >/dev/null 2>&1 &` (or `>>tunnel.log 2>&1`)
  and `echo "progress…" >&2`. **stderr is unchanged** and still takes anything,
  including output from a process the script backgrounds. A script that already
  keeps stdout clean needs no change. See [Migrating to an endpoint-only
  stdout](remote-hooks.md#migrating-to-an-endpoint-only-stdout).
- **Why it had to break.** af previously read the endpoint out of a stdout the
  docs let a tunnel share, which meant deciding per line whether a line was the
  record or a log — a question with no answer, since `[INVALID,` opening a log
  array is identical on every inspectable property to `[INFO] opening {config`
  and the two need opposite handling. Guessing wrong in the dangerous direction
  meant **dialing a URL and sending a bearer token both taken from a log line**,
  which anything able to write to the hook's stdout could choose. Reserving the
  stream makes the endpoint a parse, and there is deliberately no opt-out: an
  escape hatch would restore exactly the ambiguity this removes.

## Copying out of the web terminal on a phone

- **Long-press the terminal to copy.** Touch had no copy gesture at all — not a
  hard one, none: the rendered rows are `user-select: none` so the browser could
  never build a selection to long-press on, xterm's own selection is driven from
  mouse events a finger never sends, and every clipboard shortcut needs a Ctrl or
  Cmd a phone does not have. Pressing and holding now selects the token under your
  finger and copies it.
- It takes the **whole whitespace-delimited token** — a path, a URL, a branch name,
  a container id — rather than stopping at punctuation, because half of a URL is
  not what anyone reached for. Pressing past the end of a line copies that line
  instead, which is how you take a whole command or error message.
- The selection stays highlighted, so you can see exactly what was copied. Scrolling
  and tapping are unchanged: a drag still scrolls history and a tap still reaches a
  mouse-aware TUI, since a press that moves is a scroll and never a copy.

## Absolute-path assets load in a web-tab preview

- Setting `preview_listen_addr` (for example `127.0.0.1:8444`) now opens a second
  plain-HTTP port that serves each web tab from **its own origin**, where the dev
  server owns the origin root. An app that hard-codes absolute asset paths
  (`/assets/app.js`, Vite/CRA/Next defaults) previews correctly with no base-path
  configuration — previously those requests escaped the tab's proxy prefix and 404'd
  honestly, and the only fix was to reconfigure the dev server.
- Each tab being a genuinely distinct origin is also what isolates previews: the
  preview port answers no cross-origin allow-origin header, so one preview's
  JavaScript cannot read another's response, and none of them can reach the web UI
  or its bearer token. A tab's origin carries an opaque per-tab credential the
  daemon mints in memory and rotates on every restart.
- **Same-machine viewing only, off by default, and checked rather than guessed.**
  `*.localhost` names are resolved by the browser to its own loopback, so before
  switching a frame the web UI loads a probe page from the preview port and waits for
  it to report. If it cannot reach the port the tab silently keeps the preview it has
  always had — the same-origin, sandboxed mirror under `/v1/webtab/…`. That covers
  viewing remotely (Tailscale, SSH), the `ssh -L 8443:…` case where the browser's own
  address looks local but the daemon is not, and Safari, which does not resolve
  `*.localhost` at all. Nothing opens unless you set the key. See
  [Web UI → per-tab preview origins](web.md#per-tab-preview-origins).

## Copying out of the web terminal

- **macOS `Cmd+C` now copies.** The web terminal claimed `Ctrl` chords only, so on
  macOS the copy gesture fell through to the browser — which had nothing to copy,
  because xterm paints its own selection rather than making a DOM one. It failed
  **silently**: no error, no hint, and the next paste produced whatever had been on
  the clipboard before. `Cmd+C` over a selection now copies it, through the same
  never-silent path as `Ctrl+C`. With nothing selected it stays untouched, so it
  never sends an interrupt — `Cmd+C` is not an interrupt on macOS. `Cmd+V` is
  unchanged; it already worked.
- **Behavior change — a plain drag now selects text, even while an agent owns the
  mouse.** Claude Code and Codex both enable mouse tracking, which handed every drag
  to the application: making a selection (and so copying anything) required holding
  Option on macOS or Shift elsewhere. That is now inverted. **Dragging selects with
  no modifier, and holding Option/Shift is what sends the drag to the application** —
  so clicking an agent's own mouse-driven UI now needs the modifier. Nothing was
  removed; only which gesture needs the modifier changed.
- The wheel is deliberately **not** inverted: a mouse-aware application still
  receives a plain scroll, and Option/Shift + wheel still scrolls terminal history.

## Scrolling the web terminal on a phone

- **A one-finger drag scrolls the terminal again while an agent owns the mouse.**
  Claude Code, Codex, and any full-screen TUI enable mouse tracking, and xterm
  answers by switching its own touch scrolling off — so on a phone the pane stopped
  at the last screen with no way back into history, which is most of the reason to
  open one. The drag now moves terminal history itself.
- A **tap** still reaches the application, so a mouse-driven TUI keeps its clicks.
  The change above it — a plain *drag* now selects text, with app clicks behind
  Option/Shift — deliberately stops at the mouse: a phone has no modifier to hold, so
  applying it to a finger would leave a mouse-driven TUI unreachable there. On touch
  the two gestures separate without a modifier: the drag scrolls, the tap clicks. A
  pinch is still the browser's.
- The escape hint no longer shows for a touch. It names a key to hold — Shift, or
  Option on macOS — that a phone has no way to press, and the gesture it was there
  to rescue now works on its own.

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
