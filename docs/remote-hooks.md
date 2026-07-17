# Remote Hooks (bring-your-own-provisioner backend)

Agent Factory ships two first-class off-box backends — [`docker` and `ssh`](backends.md) — that need zero scripting. The **remote-hook backend** is the escape hatch for infrastructure those don't model (Kubernetes, Modal, Daytona, a bastion with exotic auth, a bespoke orchestrator): you provide two shell scripts and `af` runs your session on whatever you provision.

Since **#1592 Phase 4 PR7** the hook backend follows the same **provision-and-expose** contract as `docker`/`ssh`: your `launch_cmd` starts an [`af agent-server`](backends.md) in the remote workspace and echoes its authed URL; the daemon then drives the session over that `ws://` stream. A remote-hook session matches a local one on attach, type, resize, preview, archive/restore, and kill — the one exception is tab management (see [Capabilities & the agent-server](#capabilities--the-agent-server)).

> **Transport:** the `af agent-server` serves **plain HTTP** (no TLS — af terminates none of its own). The URL must be `http://` (or `ws://`), and the bearer token travels over the connection, so your `launch_cmd` must make the agent-server reachable from the daemon over a private network or tunnel it controls (a container's published loopback port, an SSH forward, a tailnet address).

> **⚠️ Breaking change (#1592 Phase 4 PR7).** The old hook contract — `launch_cmd` returning a session id, plus `list_cmd`/`attach_cmd`/`terminal_cmd` scripts for enumeration, terminal proxying, and preview capture — has been **removed**. `launch_cmd` now returns an `af agent-server` endpoint, and the only other script is `delete_cmd`. A config that still sets `list_cmd`, `attach_cmd`, or `terminal_cmd` is **rejected** with an error pointing here. See [Migrating from the old contract](#migrating-from-the-old-contract) for a copy-pasteable recipe.

## Configuration

Add remote hooks to the repo's own config file at `<repo-root>/.agent-factory/config.toml` (check it into the repo so every clone gets the same backend), and select the backend:

```toml
backend = "hook"

[remote_hooks]
launch_cmd = "./.agent-factory/hooks/launch.sh"
delete_cmd = "./.agent-factory/hooks/delete.sh"
```

(The in-repo file may also be named `config.json` for compatibility with older `af` versions — see [configuration.md](configuration.md#in-repo-file-name-configtoml-or-configjson). The JSON block further down this page is `launch_cmd` **output**, not config.)

`launch_cmd` and `delete_cmd` are both **required** — an empty value is rejected when the backend is resolved, with an error naming the missing field (e.g. `remote_hooks.launch_cmd is required`) rather than a cryptic `exec: no command` at operation time.

`remote_hooks` is an in-repo-only setting — it describes the repository, so it is not accepted in the global `~/.agent-factory/config.toml`. Configuring `backend = "hook"` selects the backend for that repo; you can also create a one-off hook session with `af sessions create --backend hook` or, in the TUI, press `N` for a remote session.

### Command path resolution

Each command value is the path of one executable — it is run directly, not through a shell. Where that path may point:

- **Relative paths resolve against the repository root** — the root of the repository whose `.agent-factory/config.toml` was loaded. `./infra/launch.sh`, `infra/launch.sh`, and `../shared/launch.sh` all work no matter what the current working directory of `af` or its daemon is. For sessions created from a linked worktree, the base is still the **main** repository root (where the config file lives), never the worktree's own path.
- **Absolute paths** are used as-is.
- **Bare names without any path separator** (`coder-launch`, `bash`) are looked up on `$PATH`, exactly like `exec`. A separator is what opts a value into repo-root resolution — so a script at the repo root must be written `./launch.sh`, not `launch.sh`.

### Validating your setup

Run `af doctor` from inside the repository to check the remote-hook setup: it validates that `launch_cmd` + `delete_cmd` are configured (and that no removed key lingers), and that each script exists and is executable. There is no read-only connectivity probe — the provision-and-expose contract has no dry-run verb (running `launch_cmd` would provision real infrastructure), so the live wire round-trip is exercised by actually creating a session.

## Script protocol

Both scripts run directly (not through a shell), return exit code 0 on success, and may write progress/log lines to **stderr** (only `launch_cmd`'s JSON on stdout matters).

### Session names

The `<name>` passed to hooks via `--name` is a slug derived from the session title:

1. lowercase the title
2. replace spaces with `-`
3. drop every character that is not `[a-z0-9-]`
4. trim leading/trailing `-`
5. if empty, use `session`

Examples: `"Fix Auth Bug"` → `fix-auth-bug`, `"my_app"` → `myapp`, `"af-test"` stays `af-test`. Two titles that slugify to the same value are rejected at create time, since `delete_cmd` keys on the slug. There is no hidden hash suffix.

**Hook names are global across projects.** Session titles are otherwise unique only *within* a project — the same title may exist in several repos at once (see [cli.md](cli.md#af-sessions)). Remote hook names are the deliberate exception: the slug reaches your scripts verbatim, with no repo component, and they tag and reap real sandboxes by it. Two projects using one name would let a second `launch_cmd` clobber the first sandbox, and either `delete_cmd` reap the survivor. So a remote-hook session's title must not collide with a remote-hook session in *any* project; af refuses the create and names the project already using it. Local sessions are unaffected — only hook-backed remote sessions share this namespace.

### `launch_cmd`

Provisions the workspace on your infrastructure, starts an `af agent-server` there, and echoes that server's authed endpoint.

**Arguments:**

| Flag | Meaning |
|---|---|
| `--name <slug>` | Stable session slug (also passed to `delete_cmd`). |
| `--title <title>` | The session title — pass it to `af agent-server --title` so the daemon dials the right workspace. |
| `--repo <url>` | The repo's `origin` URL to clone the workspace from (GitHub is the durable store). |
| `--branch <branch>` | **Only on restore** — the archived branch to materialize (see [Archive & restore](#archive--restore)). Absent on a fresh create. |
| `--program <p>` | The agent program to run (optional; forward to `af agent-server --program`). |
| `--auto-yes` | Present when AutoYes is enabled (optional; forward `--auto-yes`). |

**stdout (one JSON object):**

```json
{"url": "http://10.0.0.7:8443", "token": "…bearer…"}
```

- `url` (**required**) — the `af agent-server`'s base URL (`http://host:port` or `ws://host:port`), reachable from the daemon. It must be plain HTTP — a `wss://`/`https://` URL is rejected (af serves no TLS).
- `token` (**required**) — the bearer token the server printed on startup.

These values are the `af agent-server` startup banner (`addr`/`token`). A legacy `tls_fingerprint` field is accepted-and-ignored, so an old script keeps parsing, but it does nothing — drop it. Keep non-JSON output on stderr. If `launch_cmd` fails in any way after it has started, af runs `delete_cmd` to reap whatever it may have provisioned — see [`delete_cmd` runs on any failed launch](#delete_cmd-runs-on-any-failed-launch).

### `delete_cmd`

Tears down whatever `launch_cmd` provisioned (the runtime teardown). Runs on kill, on archive, and on a failed provision.

**Arguments:** `--name <slug>`

**stdout:** anything (a `{"deleted": true}` acknowledgement is conventional but not required).

#### `delete_cmd` runs on any failed launch

Once `launch_cmd` has **started**, af treats the sandbox as possibly-existing and runs `delete_cmd --name <slug>` on every provisioning failure — including when `launch_cmd`:

- exited 0 but echoed no parseable endpoint JSON,
- exited **non-zero**, at any point,
- **timed out** and was killed (see [Script timeouts](#script-timeouts)).

A failed `launch_cmd` is not evidence that nothing was created. A script that creates a VM and then dies has left a VM billing to your account, and because the session never finished provisioning, af keeps **no record** of it — nothing else will ever clean it up. So af reaps on the weaker signal ("the script ran") rather than the stronger one ("the script succeeded").

This means your `delete_cmd` must **tolerate being called for a sandbox that was never fully built, or never built at all**: a slug it has never seen, a half-created VM, a resource still coming up. Make it slug-deterministic and idempotent, and treat a missing resource as success rather than an error — the [reference implementation](#migration-recipe) already does this (`|| true`, `2>/dev/null`). If yours would fail or exit non-zero on an unknown slug, fix that.

af does **not** run `delete_cmd` when `launch_cmd` could not start at all — a missing file, a file that is not executable, a bad shebang. Nothing ran, so nothing was provisioned.

If `delete_cmd` itself fails, af cannot clean up and does not retry. It reports the slug and the exact command to run by hand, both on the session-create error and at error level in the log:

```
A sandbox may still be running on your infrastructure — delete_cmd could not reap it, and af will not retry.
launch_cmd ran for session "fix auth bug" (hook name "fix-auth-bug"), so it may hold real resources: a VM, a pod, a cloud sandbox.
Reap it by hand, then check your provider for anything still running:
    ./.agent-factory/hooks/delete.sh --name fix-auth-bug
delete_cmd error: …
```

The same warning is logged whenever `delete_cmd` fails on a kill or an archive, where the sandbox certainly did exist.

### Script timeouts

| Script | Budget |
|---|---|
| `launch_cmd` | 5 minutes |
| `delete_cmd` | 60 seconds |

A script that exceeds its budget is killed and the operation fails.

**A timed-out `launch_cmd` is reaped, not left running.** When the budget expires af kills the script, so it will never return an endpoint and af will never dial whatever it was building. Any sandbox it did create is already orphaned at that moment — leaving it alone would not preserve a resource you could still use, only one you would still pay for. So a timeout is treated as a failure that reaps.

**af kills only the script itself, not the processes it spawned.** If your `launch_cmd` shells out to a provisioner that keeps running after the script is killed, that provisioner may still create infrastructure *after* `delete_cmd` has already run. af cannot close that race from the outside — it is another reason to make `delete_cmd` idempotent and safe to re-run, and to prefer a `launch_cmd` that cleans up after itself on `EXIT`/`TERM`.

If `launch_cmd` exits 0 but leaves a background process holding its stdout (a tunnel or port-forward, say), af stops reading output shortly after the script exits and parses what it captured. The **exit status** is what decides success, so a session like this still comes up normally — but echo the endpoint JSON before exiting, not from the background process.

## Capabilities & the agent-server

Because the session is driven through a real `af agent-server`, a remote-hook session matches local and docker/ssh on every capability **except tab management**:

- **Attach / input / resize** happen client-side over the agent-server's `ws://` PTY stream — there is no hook attach proxy or preview-capture loop anymore.
- **Preview / liveness / prompt delivery** go over the same REST surface.
- The agent-server drives the terminal surface, so a remote session gets its Agent tab with no per-config gating. **Adding or closing tabs is not supported** off-box: every `Add*Tab` path needs a daemon-side git worktree an off-box workspace does not have, so the tab list is fixed at the single agent tab.

## Archive & restore

Durability lives in **GitHub, not the sandbox** (the epic's push/pull-branch model), identical to docker/ssh:

- **Archive** pushes the session branch to `origin`, then runs `delete_cmd` to reap the sandbox. The instance record persists (branch, backend, repo) — restorable.
- **Restore / recover** re-runs `launch_cmd` (with `--branch <archived-branch>`) to re-provision a fresh sandbox that clones the pushed state back, then restarts the agent. Your `launch_cmd` must fetch/checkout `--branch` when it is set (see the recipe).

## Migration recipe

`af agent-server` is a shipped subcommand, so migrating an existing remote-hook setup is mechanical: your `launch_cmd` clones `repo@branch`, runs `af agent-server`, and echoes its banner. A minimal reference `launch.sh` (the remote already has `af`, `git`, and `tmux` on `PATH`):

```bash
#!/usr/bin/env bash
set -euo pipefail

NAME="" TITLE="" REPO="" BRANCH="" PROGRAM="" AUTOYES=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name)    NAME="$2";    shift 2;;
    --title)   TITLE="$2";   shift 2;;
    --repo)    REPO="$2";    shift 2;;
    --branch)  BRANCH="$2";  shift 2;;
    --program) PROGRAM="$2"; shift 2;;
    --auto-yes) AUTOYES="--auto-yes"; shift;;
    *) shift;;
  esac
done

WORKDIR="$HOME/.af-hook/$NAME"          # or provision a pod / VM / sandbox here
rm -rf "$WORKDIR"; mkdir -p "$WORKDIR"
git clone -q "$REPO" "$WORKDIR/workspace"
if [ -n "$BRANCH" ]; then               # restore: bring the archived branch back
  git -C "$WORKDIR/workspace" fetch -q origin "$BRANCH:$BRANCH"
fi

# Start the agent-server; capture its startup banner (one JSON line on stdout).
BANNER="$WORKDIR/banner.json"
ARGS=(agent-server --listen 0.0.0.0:0 --repo "$WORKDIR/workspace" --title "$TITLE")
[ -n "$PROGRAM" ] && ARGS+=(--program "$PROGRAM")
[ -n "$AUTOYES" ] && ARGS+=("$AUTOYES")
setsid af "${ARGS[@]}" >"$BANNER" 2>"$WORKDIR/agent-server.log" &
echo $! > "$WORKDIR/pid"

# Wait for the banner, then re-emit it as the endpoint contract.
for _ in $(seq 1 200); do grep -q '"addr"' "$BANNER" && break; sleep 0.1; done
ADDR=$(sed -n 's/.*"addr":"\([^"]*\)".*/\1/p' "$BANNER")
TOKEN=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' "$BANNER")
[ -n "$ADDR" ] || { echo "agent-server did not start" >&2; cat "$WORKDIR/agent-server.log" >&2; exit 1; }
printf '{"url":"http://%s","token":"%s"}\n' "$ADDR" "$TOKEN"
```

The matching `delete.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
NAME=""
while [ $# -gt 0 ]; do case "$1" in --name) NAME="$2"; shift 2;; *) shift;; esac; done
WORKDIR="$HOME/.af-hook/$NAME"
[ -f "$WORKDIR/pid" ] && kill "$(cat "$WORKDIR/pid")" 2>/dev/null || true
rm -rf "$WORKDIR"
```

For a real orchestrator, replace `WORKDIR=…` / `setsid af …` with your provisioning (create a pod, `ssh` to a host, spin up a Modal/Daytona sandbox) and run `af agent-server` there, then surface its banner however you reach it (e.g. read a published address). The daemon only needs the `url` and `token` back — over plain HTTP, so make sure the address you hand back is reachable from the daemon on a private network or tunnel.

## Migrating from the old contract

| Old | New |
|---|---|
| `launch_cmd` echoes `{"name","status"}` | `launch_cmd` echoes `{"url","token"}` (plain-HTTP URL) and starts an `af agent-server` |
| `list_cmd` enumerated remote sessions | **Removed.** The daemon owns sessions; restore re-runs `launch_cmd --branch`. |
| `attach_cmd` proxied a terminal / captured preview | **Removed.** Attach + preview go over the agent-server's `ws://` stream. |
| `terminal_cmd` powered the Terminal tab | **Removed.** The agent-server manages tabs natively. |
| `delete_cmd --name <slug> --json` | `delete_cmd --name <slug>` (unchanged in spirit) |

Delete the `list_cmd`, `attach_cmd`, and `terminal_cmd` keys from your config — they now cause an actionable error — and update `launch_cmd` per the recipe above.

## Example

See `examples/remote-hooks/` for skeleton `launch.sh` / `delete.sh` implementing this protocol.
