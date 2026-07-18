# Configuration

Agent Factory reads two config files, merged field by field:

1. **Global** — `~/.agent-factory/config.toml`: your personal defaults, applied everywhere.
2. **In-repo** — `<repo-root>/.agent-factory/config.toml`: checked into a repository, applied whenever `af` runs in that repo.

Precedence is **app defaults → global config → in-repo config**: an in-repo field overrides the global value only when it is set, and `program_overrides` merges per key (an in-repo entry wins for that agent; global entries for other agents still apply).

Config is [TOML](https://toml.io) — chosen so it is easy to hand-edit. If you are upgrading from a version that used `config.json`, see [Migrating from JSON](#migrating-from-json) below; the change is automatic.

You can also read and write the global config from the CLI: `af config get <key>` / `af config list` print the effective values, and `af config set <key> <value>` writes a single settable scalar key **in place**, preserving all comments and ordering (it never regenerates the file) and validating the value first. Settable keys are the scalar tunables — `default_program`, `program_overrides.<agent>`, `auto_yes`, `auto_update`, `listen_addr`, `require_token`, `require_loopback_token`, `daemon_poll_interval`, `log_max_size_mb`, `log_max_backups`, `branch_prefix`, `worktree_root`, `detach_keys`, `update_channel`, `vscode_server_binary`, `limit_auto_resume`, `limit_retry_interval`, `limit_patterns.<agent>`, `global_agent_skills`; the structural tables (`root_agents`, `[theme]`, `[keys]`) and the `cors_allowed_origins` list are hand-edited only. See [`af config`](reference/cli.md#af-config) in the CLI reference. Changes apply on the next `af`/daemon start, the same as a hand-edit.

## Global config

`~/.agent-factory/config.toml`:

```toml
default_program = "claude"
auto_yes = false
auto_update = true
daemon_poll_interval = 1000
branch_prefix = "username/"
worktree_root = "sibling"
detach_keys = "ctrl-w"
log_max_size_mb = 50
log_max_backups = 2
update_channel = "stable"
limit_auto_resume = false
global_agent_skills = false
limit_retry_interval = "30m"

[program_overrides]
claude = "/home/me/.local/bin/claude --dangerously-skip-permissions"

[theme]
foreground = "#DCDCCC"
foreground_strong = "#FFFFEF"
foreground_muted = "#989890"
foreground_dim = "#656555"
background = "#3F3F3F"
background_subtle = "#494949"
background_panel = "#4F4F4F"
accent = "#8CD0D3"
success = "#7F9F7F"
warning = "#F0DFAF"
error = "#CC9393"
info = "#93E0E3"
purple = "#DC8CC3"
selection_background = "#4F4F4F"
selection_foreground = "#FFFFEF"
pane_border_default = "#989890"
pane_border_selected = "#8CD0D3"
pane_border_interactive = "#7F9F7F"
pane_border_preview = "#DC8CC3"
```

| Field | Description |
|-------|-------------|
| `default_program` | Default agent enum. Must be one of `claude`, `codex`, `aider`, `gemini`, `amp`, `opencode`. |
| `program_overrides` | Optional map from agent enum to the full command string used when launching that agent. Use this to pin a path or pass flags (e.g. `--dangerously-skip-permissions`). Keys must be one of `claude`, `codex`, `aider`, `gemini`, `amp`, `opencode`. Agent-specific injection (claude's `--plugin-dir` flag, aider's `--read` flag, opencode's `OPENCODE_CONFIG` env var pointing at an af-owned config, and — when `global_agent_skills = true` — the af skill file dropped into codex/gemini/amp's own skills folder) and readiness detection follow the program the override actually runs, not the key: pointing an agent name at a different command (even a non-agent one like `bash`) launches it with no injected agent flags, and a command running no known agent counts as ready once its pane shows output. The agent is identified by command-token basename (`/opt/tools/claude --model opus` and `ionice -c 3 claude` are claude; `/opt/claude-wrapper/run` is not), so if you wrap an agent in a script, name the script after the agent to keep its flags and readiness behavior. |
| `auto_yes` | Auto-accept agent prompts (experimental). |
| `auto_update` | Startup self-update. Defaults to `true`: an interactive `af` checks the configured `update_channel` on launch — at most once every 6 hours, so a relaunch inside that window costs nothing and makes no network call — and when a newer release exists it installs it, restarts the daemon from it (sessions survive), and relaunches you into the new version straight away. It never downgrades, never interrupts an `af` that is already running, and skips silently when the check fails or you are offline. It is also skipped whenever stdout is not a terminal, so a script or CI job that calls `af` keeps the binary it installed. Set to `false`, or set `AGENT_FACTORY_AUTO_UPDATE=0`, to pin the installed version; `af upgrade` still works either way. |
| `daemon_poll_interval` | Daemon polling interval in ms. |
| `listen_addr` | Address the daemon serves the bundled **web UI** + HTTP/WS API on, over **plain HTTP** (no TLS). Defaults to `127.0.0.1:8443` (loopback), so a fresh install has a browser client at `http://127.0.0.1:8443` that connects with no token and no login screen. Set it to `""` to **disable** the web server entirely (pure-unix daemon); set it to a routable host:port like `0.0.0.0:8443` to expose it to the network — a network bind **requires `require_token = true`**, and the daemon **refuses to start** without it rather than serving the control API unauthenticated (see [Remote daemon access](remote-http-auth.md#the-tokenless-network-refusal)). af serves no TLS either way, so front a routable listener with a TLS-terminating proxy or a private network. A web-port bind conflict is logged and skipped — it never blocks the daemon. Global-only. See [The web client](web.md) and [Remote daemon access](remote-http-auth.md). |
| `require_token` | Whether the web/TCP listener requires the bearer token for **non-loopback** (network) peers (default **`false`** — the token is off and auth is **opt-in**, so the bundled web UI opens with no login). What keeps that safe is the loopback-only `listen_addr` default. A **network** bind under this default would serve an **unauthenticated** control plane to anyone who can route to it, so the daemon **refuses to start** on that combination — a non-loopback `listen_addr` must set `require_token = true`. Note `require_loopback_token` does **not** satisfy this: it is inert while `require_token` is `false`. Set `true` to require the token from network peers (loopback stays exempt on a loopback bind — see `require_loopback_token`). The listener is plain HTTP, so the token travels over the connection as-is — front it with a proxy/private network. Global-only. |
| `require_loopback_token` | Whether even **loopback** peers (`127.0.0.1`/`::1`) must present the bearer token on the web/TCP listener (default `false`). The default loopback web UI is reachable with no token, which grants **any local process or user** the same access as you — weaker than the unix control socket, whose `0600` perms restrict it to your account. On a **shared/multi-user machine** set **both** `require_token = true` **and** this `true` so a same-machine browser must present the token (`af token show`), or set `listen_addr = ""` to disable the web server. It only tightens the loopback path, so it is **inert on its own**: `require_token = false` (the default) disables the token for all peers, loopback included. Global-only. |
| `cors_allowed_origins` | Exact-match allow-list of browser origins permitted to call the API cross-origin, e.g. `["https://af.example.com"]`. Empty (the default) emits no `Access-Control-Allow-Origin`, so no cross-origin browser can reach the API; non-browser clients (TUI/CLI, `curl`) are unaffected. The bundled web UI is same-origin and needs no entry here — this is for a web client you host yourself. Global-only. See [Remote daemon access](remote-http-auth.md). |
| `vscode_server_binary` | Binary that backs a **VS Code tab** (`af sessions tab-create <title> --kind vscode`, and the web UI's `+` menu). Empty (the default) **detects** one on the daemon's `PATH`: `code-server` first, then `openvscode-server`. af never bundles or installs either — when neither is found the tab still creates and the pane renders an install hint. Set a full path (a leading `~` is expanded) when the editor lives outside `PATH` or under another name; a configured path that is not executable is an **error**, never a silent fall back to detection. **Global-only**, like `root_agents`/`listen_addr`: it names a binary the daemon executes, so a repo's checked-in config must never be able to choose what af runs on your machine. See [VS Code tabs](web.md#vs-code-tabs). |
| `branch_prefix` | Prefix for worktree branches (defaults to `username/`). |
| `worktree_root` | Where new worktrees are created: `sibling` (default, next to the repo as `<repo>-<session>`) or `subdirectory` (under `~/.agent-factory/worktrees/<branch>`). |
| `detach_keys` | Key combination that detaches from an attached session (defaults to `ctrl-w`). |
| `log_max_size_mb` | Size cap in MB for `agent-factory.log` and the per-task watch-script logs before they are rotated (defaults to 50). Must be positive. |
| `log_max_backups` | How many rotated logs (`agent-factory.log.1`, `.2`, ...) to keep per log file; older ones are deleted (defaults to 2). `0` keeps none. |
| `update_channel` | Release channel that auto-update and `af upgrade` follow: `stable` (default) tracks manual `1.x.y` releases only; `preview` opts into the automatic `1.x.y-preview-z` prereleases cut every 3 hours. Any other value falls back to `stable` with a warning. See [release-process.md](release-process.md). |
| `root_agents` | Opt-in table of repositories that get an always-ensured `root` agent (default: none). See [Root agents](#root-agents-always-ensured). |
| `limit_auto_resume` | Opt in to the daemon auto-resuming a session parked at a usage-limit wall once its limit window elapses (default: `false`). See [Usage-limit auto-resume](#usage-limit-auto-resume). |
| `limit_retry_interval` | Fallback retry cadence (Go duration, e.g. `30m`) used only when `limit_auto_resume` is on **and** the limit banner carried no parseable reset time (default: `30m`). Empty or `0` disables the fallback. |
| `global_agent_skills` | Opt in to af writing its `agent-factory` skill file into your **global** codex/gemini/amp config directories so those agents discover af's CLI guidance (default: `false`). See [Agent guidance and your global agent config](#agent-guidance-and-your-global-agent-config). |
| `limit_patterns` | Optional map from agent enum to a regex that overrides the built-in usage-limit **detection** banner for that agent (the built-in reset-time parser is kept). Default: none. See [Custom usage-limit detection](#custom-usage-limit-detection-limit_patterns). |
| `theme` | Optional TUI color table. Defaults to a Zenburn-derived palette and validates each value as `#RRGGBB`; invalid values fall back to the corresponding default with a warning. See [Theme colors](#theme-colors-theme). |
| `keys` | Optional keymap overrides for the TUI. See [Key bindings](#key-bindings-keys). |

### Theme colors (`theme`)

The TUI palette defaults to Zenburn: low-contrast foreground/background colors
with muted green, red, yellow, blue, cyan, and purple accents. Override any
slot in the global `[theme]` table; omitted slots keep their defaults.

```toml
[theme]
accent = "#8CD0D3"
success = "#7F9F7F"
warning = "#F0DFAF"
error = "#CC9393"
pane_border_preview = "#DC8CC3"
```

All values must be `#RRGGBB`. Invalid values are ignored with a warning and the
default for that slot is used, so a bad color cannot prevent the TUI from
starting. `[theme]` is global-only and TOML-only, like `[keys]`: in-repo configs
reject it so a cloned repository cannot recolor your terminal.

### Root agents (always-ensured)

`root_agents` opts a repository into a **root agent**: a reserved session titled `root` that the daemon guarantees is always running. It is created **in-place** at the repo root (the `af sessions create --here` shape — no worktree or branch is created; killing it never touches your working tree or branch), and if its tmux session dies or vanishes, the daemon re-creates it automatically.

```toml
[root_agents]
"/home/me/myrepo" = {}
"~/work/other" = { program = "claude --model opus", auto_yes = false }
```

Keys are repository paths (a leading `~` expands to your home directory). Per-repo profile fields:

| Field | Description |
|-------|-------------|
| `program` | Command the root session runs. Unlike `default_program` this may be a full command string; a bare agent enum name (e.g. `claude`) still resolves through `program_overrides`. Default: the repo's resolved `claude` command with `--dangerously-skip-permissions` ensured — the root agent is meant to operate autonomously. |
| `auto_yes` | Auto-accept the agent's prompts. Defaults to **true** for root agents (unlike the global `auto_yes`). |

Behavior and guarantees:

- **Strictly opt-in and global-only.** Nothing gets a root agent unless you add it here, in *your* `~/.agent-factory/config.toml`. The key is rejected in in-repo configs, so cloning a repository can never opt your machine into an always-on agent.
- **Adopt, never clobber.** If a session titled `root` already exists and is alive — however it was created — the daemon leaves it completely alone. Only a `root` whose tmux has died (status `Dead`) or that is missing entirely is (re-)created.
- **The name `root` is reserved.** Normal session creation (TUI, `af sessions create`, the API, task spawns) rejects the title `root` (case-insensitively); auto-derived titles skip it.
- **An explicit kill is respected.** If you kill the `root` session (TUI `D`, `af sessions kill root`), the daemon does not resurrect it until the next daemon restart, at which point the configured state is re-asserted.
- **Failures back off but never give up.** If ensuring a root repeatedly fails (e.g. the configured path is not a git repository, or the tmux server is temporarily unusable), the daemon retries with exponential backoff that settles at one attempt every 5 minutes, logging each outcome to the application log (with an escalation to ERROR after 6 consecutive failures). The first attempt after the cause clears heals the root — no daemon restart needed.
- Changes to `root_agents` are picked up on the next **daemon restart**.

Because the default profile skips permission prompts and auto-accepts, only opt in repositories where you are comfortable with a fully autonomous agent running at the repo root.

### Usage-limit auto-resume

> This section covers the two auto-resume config keys. For the whole usage-limit feature end to end — detection, the `[limit]` badge, manual retry, auto-resume, and task park-don't-fail — see [docs/usage-limits.md](usage-limits.md).

When a `claude` or `codex` session hits a plan usage-limit wall, af marks it with a `[limit]` badge in the sidebar and — when the banner states a reset time — shows when the limit resets. By default the row stays there until you resume it yourself (the `c` key on the session).

`limit_auto_resume = true` opts the **daemon** into resuming such a session on its own once the limit window has elapsed:

```toml
limit_auto_resume = true
limit_retry_interval = "30m"
```

- **Off by default.** With `limit_auto_resume = false` (the default), a limit is surface-only — the badge and the manual `c` retry — and the daemon does no scheduling.
- **When it resumes.** If the banner carried a parseable reset time, the daemon resumes shortly after that time (a small grace buffer is added because limit windows are rolling and approximate). A reset time already in the past resumes promptly.
- **No parseable reset time.** Some banners don't state a reset time. In that case the daemon falls back to retrying on the fixed `limit_retry_interval` cadence (a Go duration such as `30m` or `1h`). Setting `limit_retry_interval` to empty or `0` leaves such a session surface-only.
- **Re-limit backoff.** If a resumed session immediately hits the wall again, the daemon backs off exponentially (settling at one attempt every 5 minutes) rather than hammering a genuinely exhausted plan. Killing the session is always the off-ramp.
- **Global-only, daemon behavior.** Both keys are rejected in in-repo configs and take effect on the next daemon restart.

Resuming re-delivers the session's stored task prompt (task-driven sessions resume their work); an interactive session with no stored prompt is sent a bare `continue`, which loses the agent's prior in-context state.

- **Task runs park, don't fail.** When a cron/watch task fires while your plan is already exhausted, the task-driven session that hits the wall at startup is **parked** — kept, marked `[limit]`, and recorded with the run status `parked: usage limit` — instead of being torn down and recorded as a failed run. Once the window resets, the same resume machinery (auto-resume or your manual `c` retry) re-delivers the stored task prompt and the run proceeds to completion. See [docs/usage-limits.md](usage-limits.md#task-runs-park-dont-fail).

### Custom usage-limit detection (`limit_patterns`)

The built-in usage-limit detection recognizes the shipped `claude`/`codex`
banners. If an agent reworded its banner, override the **detection** regex per
agent with `limit_patterns`; the built-in reset-time parser is kept, so a custom
detect pattern still schedules auto-resume against the parsed reset time.

```toml
[limit_patterns]
claude = "Claude usage limit reached\\."
codex  = "You've hit your usage limit"
```

- Keys must be a supported agent enum (`claude`, `codex`, `aider`, `gemini`, `amp`, `opencode`).
- An override for an agent with no built-in matcher (`aider`/`gemini`/`amp`/`opencode` today — none of them expose a plan-reset banner to parse) is ignored with a warning.
- An uncompilable regex warns and falls back to the built-in default, so a typo can never disable detection.
- `limit_patterns` is a detection tweak, not a behavior switch: it is honored everywhere the built-in detector runs (the daemon status poll, and the task-run startup park path).

### Key bindings (`[keys]`)

The TUI's key bindings are rebindable from a `[keys]` table. Each entry maps an **action** to a key string or a list of key strings, replacing that action's default binding entirely; actions you don't list keep their defaults.

```toml
[keys]
quit = "Q"
new = "c"
up = ["u", "ctrl+p"]
tasks = "ctrl+t"
```

- Key strings are the forms the terminal reports: a single character (`Q`, `/`, `?`), a named key (`up`, `enter`, `f5`, `space`), or a `ctrl+`/`alt+`/`shift+` combination (`ctrl+t`, `shift+up`).
- **Rebindable actions:** `up`, `down`, `scroll_up`, `scroll_down`, `attach`, `new`, `kill`, `quit`, `help`, `new_remote`, `new_tab`, `close_tab`, `tasks`, `search`, `open_pr`, `copy_pr`, `hooks`, `config_agent`, `config_editor`, `open_pane`, `split_pane`, `hide_pane`, `pane_prev`, `pane_next`, `collapse`, `expand`, `next_section`, `prev_section`, `archive`, `restore`, `limit_retry`, `error_details`, `switch_project`. (Run `af keys` to print the full effective table.)
- `pane_prev` / `pane_next` are contextual: their default `left` / `right` bindings switch panes only while a workspace pane has focus. With tree focus, the same arrows keep the tree's collapse/expand behavior.
- **Reserved keys** are rejected: binding any action to `enter`, `tab`, `shift+tab`, `esc`, `ctrl+]`, or a digit `1`–`9` is a startup error naming the key and why it's reserved (they drive interaction, the focus ring, overlay cancel, the interactive-mode exit, and the 1–9 tab jump respectively).
- **`ctrl+c` is a fixed hard exit, not a reserved key.** Validation does *not* reject it — you can write `quit = "ctrl+c"` (or point any action at it) with no error — but `ctrl+c` always quits and is handled before the keymap ever sees the keypress, so binding an action to it has no effect: the hard exit wins. It is therefore not *effectively* rebindable, which is different from the reserved keys above that are outright rejected at load.
- Any problem — an unknown action, an unparseable or reserved key, or two user overrides bound to the same key — is a **hard error at startup** that names the file and the offending action, so a typo can't silently leave you with a dead key. A user override on a key *suppresses* any default binding for that key rather than erroring, so an upgrade that ships a new default binding never breaks an existing config — the user's binding wins and the new action is simply unbound by default. The bottom menu and the `?` help overlay both reflect your rebinds.
- **Global-only.** `keys` is rejected in in-repo configs — a cloned repository can never rebind your terminal.
- **TOML-only.** The keymap exists only in `config.toml`; a `keys` block in a legacy `config.json` is ignored with a warning.

Run `af keys` to see the effective bindings (defaults plus your rebinds).

The default TUI keys changed to ergonomic lower-case bindings in #1027:
archive is `a`, restore is `r` (#1605), the task manager is `m`, copy PR URL
is `y`, hooks is `e`, and scrolling is `ctrl+u` / `ctrl+d`. The previous
defaults are not built-in
aliases; restore any old binding you still want by pinning it here:

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

### Agent guidance and your global agent config

af teaches each agent how to drive `af` itself — `af sessions whoami`, `af sessions archive --self`, and the rest. How that guidance reaches the agent depends on what the agent supports.

For **claude**, **aider** and **opencode**, af owns the file and points the agent at it for that launch only (`--plugin-dir`, `--read`, and `OPENCODE_CONFIG` respectively). Everything lives under af's own config directory, so it disappears when you uninstall af and is invisible to an agent af did not launch.

**codex**, **gemini** and **amp** auto-discover skills from a directory in your home and offer no per-launch pointer to an extra one. The only way to reach them is to write a file into *your* config — `$CODEX_HOME/skills/agent-factory/`, `~/.gemini/skills/agent-factory/`, `~/.config/amp/skills/agent-factory/` — which outlives the session, survives uninstalling af, and applies when you run those agents by hand somewhere af has nothing to do with. Creating a session is not consent to that, so **af does not do it by default**:

```toml
global_agent_skills = true
```

With it on, af writes (and keeps up to date) a single `agent-factory/SKILL.md` under each of those agents' skills directories. With it off — the default — those three agents simply do not get af's guidance; everything else about the session is unchanged.

af only ever manages the file it wrote. Each one carries an af marker, and:

- a file at that path **without** the marker is yours and is never overwritten or removed;
- turning the key off (or leaving it off after an af version that wrote one) removes af's **own** marked file, so af's edit does not outlive the decision;
- the `agent-factory/` directory is removed only if it is empty, so anything you put beside af's file keeps the directory alive.

### Choosing the agent

Override the agent for new sessions with `-p`:

```bash
af -p aider
```

`-p` and the per-task `program` field both accept a bare agent enum only (`claude`, `codex`, `aider`, `gemini`, `amp`, `opencode`). To pass a custom path or flags for an agent, set `program_overrides.<agent>` in your config — every session that launches that agent will use the override.

## In-repo config

A repository can carry its own configuration in `<repo-root>/.agent-factory/config.toml`, so every clone gets the same setup:

```toml
default_program = "codex"
post_worktree_commands = ["npm install"]

[program_overrides]
codex = "/usr/local/bin/codex --profile work"

[remote_hooks]
launch_cmd = "./infra/launch.sh"
delete_cmd = "./infra/delete.sh"
```

> **TOML top-level ordering:** put plain keys and arrays (like `post_worktree_commands`) *above* any `[table]` header. Once a table is opened, every following bare key belongs to it — that is TOML, not an af rule.

| Field | Scope |
|-------|-------|
| `default_program`, `program_overrides` | Valid globally **and** in-repo (in-repo wins). |
| `post_worktree_commands`, `remote_hooks` | **In-repo only.** The legacy `~/.agent-factory/repos/<repoID>/config.json` location keeps working for one more release (a deprecation warning in the log points at the new file) and is shadowed whenever the in-repo file sets the same key — including by an explicit empty value like `post_worktree_commands = []`. |
| `backend`, `docker`, `ssh` | **In-repo only.** Select the runtime a repo's sessions run on. |
| `auto_yes`, `auto_update`, `require_token`, `require_loopback_token`, `listen_addr`, `cors_allowed_origins`, `daemon_poll_interval`, `branch_prefix`, `worktree_root`, `detach_keys`, `log_max_size_mb`, `log_max_backups`, `update_channel`, `keys`, `theme`, `root_agents`, `limit_auto_resume`, `limit_retry_interval`, `vscode_server_binary`, `global_agent_skills` | Global only. Setting them in-repo is rejected with an error naming the key. The daemon network-surface keys (`require_token`, `listen_addr`, `cors_allowed_origins`) are global-only so a cloned repo can never open a port, widen CORS, or disable auth. `vscode_server_binary` is global-only for the same reason: it names a binary the daemon executes. See [remote-http-auth.md](remote-http-auth.md). |

`post_worktree_commands` are shell commands run after each new worktree is created (e.g. `npm install`, `make build`) — they can also be edited from the TUI via the `e` (worktree hooks) key. `remote_hooks` configures a remote-machine backend; see [remote-hooks.md](remote-hooks.md) for the script protocol.

### Backend runtime (`backend`, `docker`, `ssh`)

`backend` selects the runtime a repo's sessions run on, and `--backend` overrides
it per `af sessions create`:

| Value | Runtime |
|-------|---------|
| `local` (default, or unset) | Today's in-process runtime: the agent runs as a tmux session in a git worktree on the machine running the daemon. |
| `hook` | The remote-hook backend — a bring-your-own provisioner driven by the `[remote_hooks]` scripts (equivalent to the TUI's "new remote session"). |
| `docker` | Run the workspace + agent in a container started from `[docker].image`. |
| `ssh` | Run the workspace + agent on `[ssh].host` over ssh. |

```toml
backend = "docker"

[docker]
image = "af-runtime:latest"
run_args = ["--memory", "2g"]

[ssh]
host = "build-box"
user = "ci"
port = 2222
identity_file = "~/.ssh/id_ed25519"
```

An unknown `backend` value (or `--backend`) is reported when the session's
runtime is resolved at create time, naming the valid options.

### In-repo file name: `config.toml` or `config.json`

Because the in-repo file is **checked into your repository**, both names are accepted indefinitely: `<repo-root>/.agent-factory/config.toml` **or** `<repo-root>/.agent-factory/config.json`. This is deliberate — a repo shared with collaborators still on an older `af` (which only understands `config.json`) must keep working, so `af` never renames a checked-in file out from under them.

- New in-repo files that `af` writes (e.g. saving worktree hooks from the TUI) are created as `config.toml`.
- An existing `config.json` is updated in place, still as JSON, so your collaborators' `af` keeps reading it.
- A repo carrying **both** `config.toml` **and** `config.json` is a hard error naming both files — `af` will not guess which is live. Keep exactly one.

If your whole team is on a current `af`, prefer `config.toml`. While versions are mixed, keep `config.json`.

### Relative hook paths

Relative `remote_hooks` paths (like `./infra/launch.sh` above) resolve against the repository root — the repo whose `.agent-factory/config.toml` was loaded; for sessions in linked worktrees that is the main repository root — so checked-in hook scripts work no matter what the working directory of `af` or its daemon is. Bare names without a path separator (e.g. `bash`) keep normal `$PATH` lookup. See [remote-hooks.md](remote-hooks.md#command-path-resolution) for the full rules.

### Trust

An in-repo config executes what it configures: `post_worktree_commands` run after each worktree is created, and `remote_hooks` and `program_overrides` values are invoked as shell commands. Cloning a repository and running `af` in it implies trusting that repo's in-repo config. The first time a config carrying such fields loads (and whenever its content changes), `af` records one log line naming the fields and the file's content hash.

## Migrating from JSON

Earlier versions stored config as `config.json`. The move to TOML is **automatic and one-time** — you don't run anything:

- The first time a current `af` starts and finds a `~/.agent-factory/config.json` but no `config.toml`, it reads your settings, writes an equivalent `config.toml`, and moves the original aside to `config.json.bak`. From then on `config.toml` is the file to edit; `config.json.bak` is kept as a backup you can delete once you're happy. An existing backup is never overwritten — if `config.json.bak` is already there (e.g. from an earlier convert-and-downgrade round trip), the new one lands as `config.json.bak.1`, `.bak.2`, and so on, so your oldest backup is always preserved.
- If both `config.toml` and `config.json` are ever present, `config.toml` wins and `config.json` is ignored (with a warning). Delete or rename the stray `config.json` to silence it.
- A `config.json` that can't be parsed is **left untouched** with an error telling you what's wrong — it is not converted until it's valid, so you never lose settings to a half-broken file.
- If you downgrade to an older `af` after converting, it won't see your `config.toml` and will regenerate a default `config.json`. Your settings are safe in `config.toml` (and `config.json.bak`); when you upgrade again, `config.toml` takes over. To restore the old file explicitly, `mv config.json.bak config.json` before downgrading.

The **in-repo** file is not auto-converted — see [In-repo file name](#in-repo-file-name-configtoml-or-configjson).

## Where state lives

All data (sessions, tasks) is scoped to the current git repository — the TUI shows only what's relevant to the active project. Press `Ctrl-p` to switch projects without restarting.

| Path | Contents |
|------|----------|
| `~/.agent-factory/config.toml` | Global config. |
| `~/.agent-factory/config.json.bak` | Backup of your pre-TOML config, left by the one-time migration. Safe to delete. |
| `~/.agent-factory/instances/<repoID>/instances.json` | Persisted sessions, per repo. |
| `~/.agent-factory/tasks.json` | Tasks (see [tasks.md](tasks.md)). |
| `~/.agent-factory/logs/task-<id>.log` | Per-task watch-script logs. Rotated with the same `log_max_size_mb`/`log_max_backups` policy as the application log (`task-<id>.log.1`, `.2`). |
| `~/.config/agent-factory/agent-factory.log` | Application log (`os.UserConfigDir` on other platforms). Rotated once it exceeds `log_max_size_mb` (default 50 MB); the most recent `log_max_backups` rotations (default 2) are kept as `agent-factory.log.1`, `.2`. |

Setting the `AGENT_FACTORY_HOME` environment variable relocates the `~/.agent-factory` state directory — useful for sandboxed or test setups. When it is set, the application log also moves into that directory (`$AGENT_FACTORY_HOME/agent-factory.log`) so a relocated home is fully self-contained.
