# CLI guide

Everything the TUI does is also scriptable. The `af sessions` and `af tasks` command groups output JSON to stdout and errors to stderr, so they compose with `jq` and shell scripts. Pass `--json` to wrap output in a `{data, error}` envelope for structured error handling. The TUI and the CLI share the same state — you can mix them freely.

Run `af <command> --help` for the authoritative flag list of any command.

## `af` — the TUI

```bash
cd your-project    # must be a git repo
af                 # launch the TUI
```

| Flag | Description |
|------|-------------|
| `-p`, `--program` | Agent to run in new sessions, one of `claude`, `codex`, `aider`, `gemini`, `amp`. To pass custom paths or flags, use `program_overrides` in the config instead — see [configuration.md](configuration.md#choosing-the-agent). |
| `-y`, `--autoyes` | Experimental: automatically accept agent prompts in all sessions. |

## `af sessions`

All subcommands accept `--repo <path>` to target a repository other than the current directory, and `--json` to wrap output in the shared envelope.

Session titles are unique **within a project, not across projects** — the same name may exist in several repos at once. Every command that takes a `<title>` resolves it inside the repo named by `--repo`, falling back to the current directory's repo. With no repo context (no `--repo`, cwd outside a repo) a title held by exactly one session anywhere still resolves; a title held by sessions in several projects is ambiguous and reports an error naming them rather than picking one:

```
session "foo" exists in multiple projects: /repos/alpha, /repos/beta — pass --repo to pick one
```

Against a remote daemon (`--daemon-url`/`AF_DAEMON_URL`), the split follows the transport:

- **Reads served by the targeted daemon** — `list`, `get`, `watch`, `preview` — ignore the current directory rather than sending it as a scope, since it names a repo on *your* machine, not the daemon's. A bare title resolves across the remote's projects.
- **Everything else** — `kill`, `archive`, `restore`, `send-prompt`, tab create/delete — reaches the *local* daemon regardless of `--daemon-url`, so it stays scoped to the current directory.

Caveat for the reads: `--repo` becomes an id by hashing the path **as given on this machine**, so it only disambiguates when the daemon has that project checked out at the same absolute path. Prefer a bare title against a remote and let the ambiguity error tell you when to narrow it.

One exception to per-project titles: **remote hook** sessions share a global name namespace, because the slug reaches `launch_cmd`/`delete_cmd` verbatim and external provisioners key real sandboxes on it — see [remote-hooks.md](remote-hooks.md#session-names).

```bash
af sessions list                                          # list sessions in the repo
af sessions get <title>                                   # fetch one session
af sessions create --name <title> [--prompt "..."] [--program <agent>] [--here]
af sessions send-prompt <title> "..."                     # append a prompt to a session
af sessions send-prompt <title> "..." --create            # send-or-create
af sessions tab-create <title> --command "<cmd>"          # spawn a process tab in the session's worktree
af sessions tab-create <title> --kind web --port 5173     # web/iframe tab pointing at a localhost dev server
af sessions tab-delete <title> --name <tab>               # delete a single tab (the daemon won't respawn it)
af sessions tabs create <title> --command "<cmd>"         # alias for tab-create (hyphen verb still works)
af sessions tabs create <title> --kind web --url <url>    # alias for tab-create (hyphen verb still works)
af sessions tabs delete <title> --name <tab>              # alias for tab-delete
af sessions preview <title>                               # snapshot the session's pane
af sessions attach <title>                                # attach interactively (foreground)
af sessions whoami                                        # report the session this shell is inside
af sessions archive <title>                               # default done action: restorable later
af sessions archive --self                                # archive the current session (resolved via whoami)
af sessions kill <title> [--force]                        # permanently destroy + prune branch
af sessions restore <title>                               # restore an archived/lost/dead session
```

Flags:

- `create`: `--name` (required), `--prompt` (initial prompt to send), `--program` (agent enum, defaults to the configured `default_program`). `--here` (alias `--in-place`) attaches the session to the repo's **existing working tree at its current branch** instead of cutting a new worktree+branch: the agent runs in the repo root, no branch is created, and killing the session never removes the working tree or branch. Requires a git repository (the current directory, or `--repo`); incompatible with remote sessions. The title `root` (any casing) is reserved for the daemon-managed root agent — see the `root_agents` key in [configuration.md](configuration.md#root-agents-always-ensured).
- `send-prompt`: `--create` auto-creates the session if it doesn't exist; `--program` picks the agent when creating.
- `tab-create`: creates a new tab in the session's worktree. By default, `--command` is run in the worktree as a process tab; `--name` sets the tab's display name (defaults to the command's basename, auto-suffixed `-2`, `-3`, … on collision). With `--kind web`, creates an iframe tab targeting `--url` (or `--port` as a localhost:<port> convenience) instead — see [web.md](web.md). The resolved tab name is printed as `{"name": "..."}` so scripts/agents can address it. The tab persists and reconnects across a daemon/`af` restart like every other tab. Refused once a session already holds 9 tabs. **Local sessions only:** an off-box session (docker/ssh/hook) has no daemon-side worktree — its tabs are managed by the in-workspace `af agent-server` (see [remote-hooks.md](remote-hooks.md)).
- `tab-delete`: the counterpart of `tab-create` — `--name` (required) selects the tab to delete. The tab is removed from the daemon's session state and its tmux window is killed; the removal is persistent (the daemon won't respawn it, and it doesn't return on restart). The deleted tab's name is printed as `{"name": "..."}`. The agent tab can't be deleted — use `af sessions kill` to tear down the whole session. Targeting a missing tab or session is an error. Local sessions only (an off-box session's tabs live in its agent-server).
- `tabs {create,delete}`: additive noun-subcommand aliases — `af sessions tabs create` == `af sessions tab-create` and `af sessions tabs delete` == `af sessions tab-delete` (same flags and output). The hyphen verbs are kept for existing scripts; nothing is renamed. There is no `tabs list` — list a session's tabs via `af sessions get`.
- `archive`: the default way to finish with a session. It tears down the session's tmux and **moves its git worktree** out to the global archive directory (`<AGENT_FACTORY_HOME>/archived/<repoID>/<title>/`), preserving the branch and any uncommitted changes. The session is not deleted — it becomes a quiescent **archived** row that survives restarts and is never auto-restored. Prints `{"ok": true, "title": "...", "archived_path": "..."}`. An off-box session (docker/ssh/hook) archives differently — it **pushes its branch to `origin`** and tears the sandbox down (durability lives in GitHub, not the sandbox), and restore re-provisions from the pushed branch. Not available for in-place (`--here`) sessions (they don't own a relocatable worktree). Bring it back with `restore`. Pass `--self` instead of a `<title>` to archive the current session — it resolves the caller's own session the way `whoami` does, so an agent can archive itself once its work is done; `--self` and a `<title>` are mutually exclusive, and it errors with an actionable message when not run from inside a session.
- `kill`: permanently destroys a session: tmux is torn down, the worktree is removed, the stored row is deleted, and the branch is pruned when Agent Factory owns it. It always destroys the session, including any uncommitted or unmerged work on its branch — there is no undo. Use `af sessions archive <title>` to keep it restorable instead. `--force` is accepted but has no effect (kept for backward compatibility).
- `restore`: restores an **archived**, **Lost**, or **Dead** session. Archived sessions move their worktree back next to the repo, re-register it, re-spawn **only the agent** (shell/process tabs are not restored), and mark the session running. Lost/Dead sessions recover in place, rebuilding a missing worktree when possible and resuming the recorded agent conversation when required. Prints `{"ok": true, "title": "...", "worktree_path": "..."}`. Fails if the session is not restorable, or if its origin repo is gone (an archived worktree is left intact for manual recovery). Honors `--repo` like `kill`.

## `af tasks`

Tasks deliver a prompt to an agent automatically — on a cron schedule or whenever a long-running watch script emits a stdout line. Full semantics (trigger × delivery matrix, watch-script contract, status model) live in [tasks.md](tasks.md). All subcommands accept `--repo <path>` and `--json`.

```bash
af tasks list
af tasks add --name <n> --prompt <p> --cron "0 9 * * *" [--target-session <title>] [--program <agent>]
af tasks add --name <n> --watch-cmd <cmd> [--prompt "... {{line}} ..."] [--target-session <title>]
af tasks get <id>
af tasks update <id> [--cron ...|--watch-cmd ...] [--prompt ...] [--target-session ...] [--program <agent>] [--enabled true|false]
af tasks trigger <id>          # run a cron task immediately (cron tasks only)
af tasks remove <id>
```

Exactly one of `--cron` / `--watch-cmd` per task. On `update`, setting one trigger clears the other. `--target-session ""` explicitly reverts to create-a-session-per-run; omitting the flag leaves it untouched. `--program` accepts the same agent enum as `tasks add`; omitting it keeps the task's current program.

## `af daemon`

The background daemon hosts task cron schedules, watch-task scripts, and autoyes mode. It starts on demand whenever `af` runs and an enabled task exists; installing it as a user-level autostart unit (systemd user service on Linux, launchd agent on macOS) keeps scheduled tasks firing after reboots. See [tasks.md](tasks.md#daemon-lifecycle).

```bash
af daemon install      # register autostart at login
af daemon restart      # restart a running daemon and re-adopt live sessions
af daemon uninstall    # remove the autostart unit (the daemon still starts on demand)
af daemon status       # read-only health snapshot: running?, socket paths, pid, autostart (+ --json)
```

`af daemon status` uses the same no-spawn probe as `af doctor` — it reports whether the daemon answers on the control socket, the control/HTTP socket paths and file presence, the recorded (and verified) pid, whether the autostart unit is installed, and whether a running daemon is on a since-replaced binary. It never dials in a way that starts a daemon.

`af daemon restart` is safe to run after replacing the `af` binary. It asks a
running daemon to shut down cleanly, restarts the autostart unit when one is
installed, otherwise starts an ad-hoc daemon from the current binary. If no
daemon is running, it exits successfully without starting one.

## `af config`

Read and write the **global** config (`~/.agent-factory/config.toml`) from the CLI. Config is a hand-editable file read by the daemon and TUI at startup (not daemon-owned state). `--json` wraps output in the shared envelope (success and error).

```bash
af config list                     # every key and its effective value (defaults applied)
af config get <key>                # one key (scalars print bare; maps as JSON)
af config set <key> <value>        # write one settable key, preserving comments/ordering
```

`get`/`list` report effective global values (defaults applied). `set` edits only the target value's bytes — every comment, blank line, and key ordering is preserved (the file is not regenerated) — and validates the value with the loader's own rules before writing, so it can never produce a config that fails to load. Settable keys: `default_program`, `program_overrides.<agent>`, `auto_yes`, `auto_update`, `daemon_poll_interval`, `log_max_size_mb`, `log_max_backups`, `branch_prefix`, `worktree_root`, `detach_keys`, `update_channel`, `limit_patterns.<agent>`. Structural keys (`root_agents`, `[keys]`) stay hand-edited. A change applies on the next `af`/daemon start, exactly like a hand-edit (`set` prints this reminder). Full key reference: [configuration.md](configuration.md).

## Maintenance commands

```bash
af version             # print the version and the release URL
af debug               # print the resolved config and its path
af keys                # print the effective TUI key bindings (defaults + [keys] rebinds)
af upgrade             # self-upgrade to the latest GitHub release (Linux/macOS)
af doctor --setup      # verify first-run prerequisites and writable storage
af doctor              # diagnose setup, leaked resources, and daemon health
af doctor --fix        # also apply the safe remediations
af bug-report          # bundle logs + versions + tasks + redacted state into one file to attach
af bug-report --json   # emit the structured manifest to stdout instead of writing a file
af reset               # nuclear option — see below
```

`af bug-report` collects one shareable diagnostics file: the daemon log tail
(bounded to the last ~2MiB / 5000 lines), versions (af, Go, OS/arch, the daemon
snapshot), the configured tasks, the session state from `instances.json`, the
`af daemon status` health snapshot, and the global config. It writes a single
text file (default `~/af-bug-report-<ts>.txt`, mode 0600; override with
`-o/--output`) so you can read the whole thing in one scroll before attaching
it. Redaction is **best-effort**: free-text and secret-bearing fields (session
titles, session prompts, task prompts, tab commands, remote metadata) are dropped, `$HOME` and
your username are collapsed to `~` / `[user]`, and known credential shapes are
scrubbed everywhere — but perfect redaction is impossible, so **review the file
before sharing it publicly**. It is read-only and local (like `af doctor`): it
never dials the daemon or the network, and is not part of the HTTP `af api`
surface.

`af doctor --setup` is the first-run profile: it checks AF home/config/state/log
writability, git and git identity, tmux, configured agent commands, daemon
health, and remote-hook setup for the current repo when configured.

`af doctor` is read-only by default: it reports orphaned processes left behind
by dead sessions, processes pegging a CPU core inside live sessions, `af_` tmux
sessions with no backing record, abandoned temp agent-factory homes, and daemon
problems (stale socket, stale pid file, a daemon still running a replaced
binary). With `--fix` it kills orphans whose ancestry markers prove they came
from a dead Agent Factory session and removes stale temp homes, logging each
action; anything it cannot verify is reported, never touched. Exits 1 when
unresolved issues remain.

When the repository you run it in configures a [remote-hook backend](remote-hooks.md),
`af doctor` also validates that setup, so a misconfigured remote surfaces as a
diagnosable problem instead of a cryptic failure at session-launch time:

- **remote-config** — the required `remote_hooks` commands (`launch_cmd`,
  `delete_cmd`) are present, naming the missing field and the in-repo config file
  when one is not. It also rejects a config still carrying the removed pre-PR7
  keys (`list_cmd`/`attach_cmd`/`terminal_cmd`), naming the stale key and pointing
  at the migration recipe.
- **remote-hook-script** — every configured hook command resolves to something
  runnable: a path that exists and carries the execute bit (with the exact
  `chmod +x` fix otherwise), or a bare name found on `$PATH`.

There is no connectivity round-trip probe: the provision-and-expose contract has
no read-only verb (`launch_cmd` provisions real infrastructure and starts an
`af agent-server`; `delete_cmd` tears it down), so the live wire round-trip is
exercised by actually creating a session, not by a doctor dry-run.

These checks run only for a repo that configures `remote_hooks`. Run outside a
git repo, or in a repo with no remote backend — the common local-only case —
they collapse to a single `n/a — no remote backend configured` line and add no
findings, so local users see no new noise. The remote checks are validated
against the current working directory's repository; run `af doctor` from inside
the repo whose remote setup you want to check.

`af reset` attempts to stop the daemon (and reports honestly if it couldn't — e.g. a source-built `agent-factory --daemon` that left no PID file), kills **all** Agent Factory tmux sessions, removes **every linked git worktree (and its branch)** from each repo that has stored sessions — including worktrees you created by hand — and deletes all stored session records. Use it to recover from a corrupted state, not for day-to-day cleanup — `af sessions archive <title>` (or `a` in the TUI) finishes with one session while keeping it restorable.
