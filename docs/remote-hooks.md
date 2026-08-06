# Remote Hooks (bring-your-own-provisioner backend)

Agent Factory ships two first-class off-box backends — [`docker` and `ssh`](backends.md) — that need zero scripting. The **remote-hook backend** is the escape hatch for infrastructure those don't model (Kubernetes, Modal, Daytona, a bastion with exotic auth, a bespoke orchestrator): you provide two shell scripts and `af` runs your session on whatever you provision.

Since **#1592 Phase 4 PR7** the hook backend follows the same **provision-and-expose** contract as `docker`/`ssh`: your `launch_cmd` starts an [`af agent-server`](backends.md) in the remote workspace and echoes its authed URL; the daemon then drives the session over that `ws://` stream. A remote-hook session matches a local one on attach, type, resize, preview, archive/restore, and kill — the one exception is tab management (see [Capabilities & the agent-server](#capabilities-the-agent-server)).

> **Transport:** the `af agent-server` serves **plain HTTP** (no TLS — af terminates none of its own). The URL must be `http://` (or `ws://`), and the bearer token travels over the connection, so your `launch_cmd` must make the agent-server reachable from the daemon over a private network or tunnel it controls (a container's published loopback port, an SSH forward, a tailnet address).

> **⚠️ Breaking change (#2845): `launch_cmd`'s stdout is now the endpoint's alone.** stdout must carry the `{"url","token"}` JSON and **nothing else** — no tunnel logs, no progress lines. Anything else on it fails the provision with an error quoting the offending output. Only scripts that let another writer share stdout are affected, and the fix is one redirect. See [Migrating to an endpoint-only stdout](#migrating-to-an-endpoint-only-stdout).

> **⚠️ Breaking change (#1592 Phase 4 PR7).** The old hook contract — `launch_cmd` returning a session id, plus `list_cmd`/`attach_cmd`/`terminal_cmd` scripts for enumeration, terminal proxying, and preview capture — has been **removed**. `launch_cmd` now returns an `af agent-server` endpoint, and the only other script is `delete_cmd`. A config that still sets `list_cmd`, `attach_cmd`, or `terminal_cmd` is **rejected** with an error pointing here. See [Migrating from the old contract](#migrating-from-the-old-contract) for a copy-pasteable recipe.

## Two contracts: `provision_cmd` or `launch_cmd`

A remote-hook repo picks **one** of these. They answer the same question — how does this session get a workspace — in opposite ways, and setting both is rejected rather than resolved by preference.

| | `provision_cmd` (#2847) | `launch_cmd` |
|---|---|---|
| Your script returns | an **ssh host** + its host key | an `af agent-server` **endpoint** (`{url,token}`) |
| Who starts `af agent-server` | **af** | your script |
| Who owns the bearer token | **af** — it never enters your script | your script: argv, stdout, logs, errors |
| Who keeps a tunnel alive | **af** | your script |
| Needs sshd on the target | yes | no |

**Prefer `provision_cmd`.** Your script collapses to *make a machine, print how to reach it*, and the parts that have historically gone wrong stop being your problem: no token to leak, no tunnel to background, no agent-server lifecycle. Because a host address and a host **public** key are not secrets, nothing on this path needs redacting.

**`launch_cmd` remains the escape hatch**, and is not deprecated. Some targets have no sshd — certain Kubernetes pods, serverless runners, WebSocket-only PaaS — and returning a URL is the only option there.

### `provision_cmd`

Same arguments as `launch_cmd` (`--name`, `--title`, `--repo`, and `--branch` on restore). **stdout carries one JSON object and nothing else:**

```json
{"host": "10.0.0.7", "user": "af", "port": 2222, "host_key": "ssh-ed25519 AAAAC3Nz…"}
```

- `host` (**required**) — the address af connects to. May carry the port as `host:port` instead of using `port`.
- `host_key` (**required**) — the machine's **public** host key, in `authorized_keys` form.
- `user`, `port` — optional.

Progress goes on **stderr**; anything else on stdout fails the provision with the offending line quoted, exactly as for `launch_cmd`.

#### Why `host_key` is required

A machine created seconds ago has no `known_hosts` entry, and the resulting prompt is precisely what an unattended provision cannot answer. None of af's host-key postures solves it: `strict` refuses an unknown host, `accept-new` is trust-on-first-use where **every** session is a first contact (and its store later refuses a legitimate VM once an address is recycled), and `insecure` invites the man-in-the-middle who would then see the bearer token.

Your script is the only party with an **authentic** channel to that key — it is talking to the provider's control plane, which af cannot reach. So it returns the key, and af writes a **per-session** `known_hosts` containing exactly it, then connects with `StrictHostKeyChecking=yes` and `GlobalKnownHostsFile=/dev/null`. That is a real verification rather than trust on sight, and it is stronger than what `backend = "ssh"` can do.

Two ways to get the key, both ordinary practice:

```bash
# read it back from the provider
aws ec2 get-console-output --instance-id "$ID" | sed -n 's/^ssh-ed25519 /ssh-ed25519 /p' | head -1

# or inject one you generated, before first boot (strictly better: nothing is
# ever trusted on sight)
ssh-keygen -q -t ed25519 -N "" -f hostkey
# …pass hostkey to cloud-init as /etc/ssh/ssh_host_ed25519_key…
printf '{"host":"%s","user":"af","host_key":"%s"}\n' "$IP" "$(cat hostkey.pub)"
```

There is **no opt-out**. A record without a key is refused, because an escape hatch here would restore exactly the trust-on-sight this design removes.

## Configuration

Add remote hooks to the repo's own config file at `<repo-root>/.agent-factory/config.toml` (check it into the repo so every clone gets the same backend), and select the backend:

```toml
backend = "hook"

[remote_hooks]
# One of provision_cmd (preferred) or launch_cmd — see above.
provision_cmd = "./.agent-factory/hooks/provision.sh"
delete_cmd = "./.agent-factory/hooks/delete.sh"
```

(The in-repo file may also be named `config.json` for compatibility with older `af` versions — see [configuration.md](configuration.md#in-repo-file-name-configtoml-or-configjson). The JSON block further down this page is `launch_cmd` **output**, not config.)

`delete_cmd` is **required**, and so is exactly one of `provision_cmd` / `launch_cmd`. An empty or missing value is rejected when the backend is resolved, with an error naming the missing field (e.g. `remote_hooks.provision_cmd or remote_hooks.launch_cmd is required`, `remote_hooks.delete_cmd is required`) rather than a cryptic `exec: no command` at operation time. Setting **both** provisioning keys is rejected too — they are alternatives, not layers.

`remote_hooks` is an in-repo-only setting — it describes the repository, so it is not accepted in the global `~/.agent-factory/config.toml`. Configuring `backend = "hook"` selects the backend for that repo; you can also create a one-off hook session with `af sessions create --backend hook` or, in the TUI, press `N` for a remote session.

### Command path resolution

Each command value is the path of one executable — it is run directly, not through a shell. Where that path may point:

- **Relative paths resolve against the repository root** — the root of the repository whose `.agent-factory/config.toml` was loaded. `./infra/launch.sh`, `infra/launch.sh`, and `../shared/launch.sh` all work no matter what the current working directory of `af` or its daemon is. For sessions created from a linked worktree, the base is still the **main** repository root (where the config file lives), never the worktree's own path.
- **Absolute paths** are used as-is.
- **Bare names without any path separator** (`coder-launch`, `bash`) are looked up on `$PATH`, exactly like `exec`. A separator is what opts a value into repo-root resolution — so a script at the repo root must be written `./launch.sh`, not `launch.sh`.

### Validating your setup

Run `af doctor` from inside the repository to check the remote-hook setup: it validates that `launch_cmd` + `delete_cmd` are configured (and that no removed key lingers), and that each script exists and is executable. There is no read-only connectivity probe — the provision-and-expose contract has no dry-run verb (running `launch_cmd` would provision real infrastructure), so the live wire round-trip is exercised by actually creating a session.

## Script protocol

Both scripts run directly (not through a shell) and return exit code 0 on success. Write progress and log lines to **stderr** — af reads it for diagnostics and never for endpoints, so a script may say as much there as it likes, and so may anything it backgrounds. `launch_cmd`'s **stdout is reserved for its endpoint JSON**: see [stdout is the endpoint's, exclusively](#stdout-is-the-endpoints-exclusively).

### Session names

The `<name>` passed to hooks via `--name` is a slug derived from the session title:

1. lowercase the title
2. replace spaces with `-`
3. drop every character that is not `[a-z0-9-]`
4. truncate to 200 bytes (so the slug stays a legal filesystem component for `mktemp`/`mkdir`)
5. trim leading/trailing `-`
6. reject the remote-hook title if the result is empty

Examples: `"Fix Auth Bug"` → `fix-auth-bug`, `"my_app"` → `myapp`, `"af-test"` stays `af-test`. Two titles that slugify to the same value are rejected at create time, since `delete_cmd` keys on the slug. There is no hidden hash suffix.

A remote-hook title must therefore retain at least one ASCII letter or digit in
the bounded result. Titles such as `日本語` and `!!!` are rejected before
`launch_cmd` runs instead of receiving the generic name `session`. The check is
applied after the 200-byte bound too: a title made of 200 hyphens followed by
`a` is rejected because truncation removes its only letter. Add an ASCII
component that survives the bound (for example, `日本語-2`) to derive a specific
hook name.

**Hook names are global across projects.** Session titles are otherwise unique only *within* a project — the same title may exist in several repos at once (see [cli.md](cli.md#af-sessions)). Remote hook names are the deliberate exception: the slug reaches your scripts verbatim, with no repo component, and they tag and reap real sandboxes by it. Two projects using one name would let a second `launch_cmd` clobber the first sandbox, and either `delete_cmd` reap the survivor. So a remote-hook session's title must not collide with a remote-hook session in *any* project; af refuses the create and names the project already using it. Local sessions are unaffected — only hook-backed remote sessions share this namespace.

### `launch_cmd`

Provisions the workspace on your infrastructure, starts an `af agent-server` there, and echoes that server's authed endpoint.

**Arguments:**

| Flag | Meaning |
|---|---|
| `--name <slug>` | Stable session slug (also passed to `delete_cmd`). |
| `--title <title>` | The session title — pass it to `af agent-server --title` so the daemon dials the right workspace. |
| `--repo <url>` | The repo's `origin` URL to clone the workspace from (GitHub is the durable store). |
| `--branch <branch>` | **Only on restore** — the archived branch to materialize (see [Archive & restore](#archive-restore)). Absent on a fresh create. |
| `--program <p>` | The agent program to run (optional; forward to `af agent-server --program`). |
| `--program-resolved` | Present with `--program`; forward it to `af agent-server` so the cloned repo cannot apply a second `program_overrides` lookup. |
| `--session-env <name>` | Repeated for each global `session_env_passthrough` name; forward each one to `af agent-server --session-env`. Values are never command arguments. |

Both hook scripts themselves run with af's filtered session environment: core
runtime/Git/GitHub variables, authentication for the selected agent, and the
explicit `session_env_passthrough` names. Unrelated daemon secrets are absent.
If `launch_cmd` provisions another machine or container, it is responsible for
delivering approved values through that provider's secret mechanism and for
forwarding the `--session-env` **names** to the remote agent-server. Do not put a
credential value in argv or endpoint JSON.

**stdout (the endpoint JSON, and nothing else):**

```json
{"url": "http://10.0.0.7:8443", "token": "…bearer…"}
```

- `url` (**required**) — the `af agent-server`'s base URL (`http://host:port` or `ws://host:port`), reachable from the daemon. It must be plain HTTP — a `wss://`/`https://` URL is rejected (af serves no TLS).
- `token` (**required**) — the bearer token the server printed on startup.

These values are the `af agent-server` startup banner (`addr`/`token`). A legacy `tls_fingerprint` field is accepted-and-ignored, so an old script keeps parsing, but it does nothing — drop it. If `launch_cmd` fails in any way after it has started, af runs `delete_cmd` to reap whatever it may have provisioned — see [`delete_cmd` runs on any failed launch](#delete_cmd-runs-on-any-failed-launch).

#### stdout is the endpoint's, exclusively

**`launch_cmd`'s stdout carries that JSON object and nothing else.** Not a progress line, not a tunnel's log, not a second JSON record — the whole stream, from the first byte to the last, is one endpoint object. Surrounding whitespace is fine (a trailing newline, an indent, a blank line); everything else is a contract violation and **fails the provision**:

```
launch_cmd (./.agent-factory/hooks/launch.sh) printed something other than its endpoint on stdout: [INFO] forwarding 127.0.0.1:9000 -> pod/af-7
stdout carries the {"url","token"} endpoint JSON and nothing else. Redirect every other writer off it — start a tunnel as `mytunnel >/dev/null 2>&1 &`, or send it to a file — and write progress to stderr instead. See docs/remote-hooks.md
```

The error quotes the first thing on stdout that was not the endpoint record, and the script's full output follows it, so you can see which line to move.

The object itself is checked against the schema above: a top-level object with `url` and `token` both non-empty, and **no field beyond** `url`, `token`, and the legacy `tls_fingerprint`. So a structured log that happens to carry `url` and `token` is never mistaken for an endpoint, and printing one instead of the record fails with the schema error rather than this one.

**Why the stream is reserved.** af used to read the endpoint out of a stdout it shared with a backgrounded tunnel, which meant deciding, per line, whether it was the record the script echoed or part of a log. That question has **no answer** — `[INVALID,` opening a log array matches `[INFO] opening {config` on every property a classifier can inspect, yet the two require opposite handling ([#2845](https://github.com/sachiniyer/agent-factory/issues/2845) has the proof, and the seven rules that each closed one counterexample and admitted the next). Guessing wrong in the dangerous direction means **dialing a URL and sending a bearer token both lifted from a log line**, and anything that can write to your hook's stdout chooses those bytes. Reserving the stream deletes the question instead of ranking answers to it: the endpoint is a parse now.

**stderr is unchanged** and still takes everything else — your own progress lines, and any output from a process you background. Nothing about the endpoint is read from it.

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
    './.agent-factory/hooks/delete.sh' --name 'fix-auth-bug'
delete_cmd error: …
```

That command is shell-quoted and safe to paste as-is, whatever your `delete_cmd` path contains.

The same warning is logged whenever `delete_cmd` fails on a kill or an archive, where the sandbox certainly did exist.

### Script timeouts

| Script | Budget |
|---|---|
| `launch_cmd` | 5 minutes |
| `delete_cmd` | 60 seconds |

A script that exceeds its budget is killed and the operation fails.

**A timed-out `launch_cmd` is reaped, not left running.** When the budget expires af kills the script, so it will never return an endpoint and af will never dial whatever it was building. Any sandbox it did create is already orphaned at that moment — leaving it alone would not preserve a resource you could still use, only one you would still pay for. So a timeout is treated as a failure that reaps.

**A reap stops the whole launch tree first, then runs `delete_cmd`.** If your `launch_cmd` shells out to a provisioner (`terraform`, `gcloud`, `kubectl`) and is killed at its budget while that provisioner is still working, af kills the entire process group it started *before* running `delete_cmd`. Without that, the surviving provisioner would finish creating infrastructure **after** `delete_cmd` had already reaped and reported success — a resource that bills with nothing pointing at it, since a failed provision leaves af no record of the session.

This applies only to a `launch_cmd` that **just failed**, where everything it started is garbage by definition. It never applies to one that succeeded — see [Backgrounding a tunnel](#backgrounding-a-tunnel-is-supported-redirect-its-stdout) — and it does not apply on kill or archive, where the launch ended long ago. Making `delete_cmd` idempotent is still worthwhile, and a `launch_cmd` that cleans up after itself on `EXIT`/`TERM` is still good practice.

### Backgrounding a tunnel is supported — redirect its stdout

Many `launch_cmd`s must leave a process running to make the agent-server reachable — an `ssh -L` forward, a `kubectl port-forward`, a tunnel client. That process is not a leak: it is the thing af then dials. af treats it as **yours** and never touches it.

- af bounds and kills **the script**, never anything a script that **succeeded** left running. (A launch that *failed* is torn down as a tree — see [Script timeouts](#script-timeouts). If you want a process to survive even that, start it with `setsid`.)
- The script's stdout and stderr go to two temporary **files**, not pipes. A background process may hold them open and keep writing as long as it likes: af has already stopped reading, and there is no pipe whose closure could disturb it.
- af stops reading when the **script** exits, and its **exit status** decides success.

**Give the tunnel somewhere else to write.** stdout belongs to the endpoint record ([above](#stdout-is-the-endpoints-exclusively)), so a backgrounded process must not inherit it:

```bash
# Pick one — all three keep stdout clean:
mytunnel --to "$HOST" >/dev/null 2>&1 &                 # discard the tunnel's output
mytunnel --to "$HOST" >>"$WORKDIR/tunnel.log" 2>&1 &    # or keep it, in a file
mytunnel --to "$HOST" >&2 &                             # or fold it into stderr
```

Any of the three is fine — af reads stderr for diagnostics only, never for an endpoint. What fails is inheriting **stdout**, and it fails loudly at provision time rather than silently later.

Two rules, and they are the whole contract:

1. **Echo the endpoint JSON from the script itself, before it exits** — not from the background process. af reads what the script had written by the time it exited.
2. **Nothing else writes to stdout**, for the life of the script.

> **Also fixed earlier.** A `launch_cmd` that backgrounded a process holding stdout once **hung the provision**: af waited for the output pipe to close and the tunnel held it open indefinitely, so the 5-minute budget never applied. That is long fixed — output goes to files now — so the redirect above is about the *content* of stdout, not about avoiding a hang.

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

NAME="" TITLE="" REPO="" BRANCH="" PROGRAM="" PROGRAM_RESOLVED=""
SESSION_ENV=()
while [ $# -gt 0 ]; do
  case "$1" in
    --name)    NAME="$2";    shift 2;;
    --title)   TITLE="$2";   shift 2;;
    --repo)    REPO="$2";    shift 2;;
    --branch)  BRANCH="$2";  shift 2;;
    --program) PROGRAM="$2"; shift 2;;
    --program-resolved) PROGRAM_RESOLVED="--program-resolved"; shift;;
    --session-env) SESSION_ENV+=("$2"); shift 2;;
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
# `nohup … &` detaches it: this script exits as soon as it has the banner, and the
# server must outlive it. Both redirects are required. `>"$BANNER"` keeps the
# server off THIS script's stdout, which carries the endpoint record and nothing
# else, and gives us the banner to read; `2>"$LOG"` is where its own startup
# errors land, for the failure branch below to surface.
BANNER="$WORKDIR/banner.json"
LOG="$WORKDIR/agent-server.log"
ARGS=(agent-server --listen 0.0.0.0:0 --repo "$WORKDIR/workspace" --title "$TITLE")
[ -n "$PROGRAM" ] && ARGS+=(--program "$PROGRAM")
[ -n "$PROGRAM_RESOLVED" ] && ARGS+=("$PROGRAM_RESOLVED")
for ENV_NAME in "${SESSION_ENV[@]}"; do ARGS+=(--session-env "$ENV_NAME"); done
nohup af "${ARGS[@]}" >"$BANNER" 2>"$LOG" &
echo $! > "$WORKDIR/pid"

# Wait for the banner, then re-emit it as the endpoint contract.
for _ in $(seq 1 200); do grep -q '"addr"' "$BANNER" && break; sleep 0.1; done
ADDR=$(sed -n 's/.*"addr":"\([^"]*\)".*/\1/p' "$BANNER")
TOKEN=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' "$BANNER")
# Always echo the server's own output on failure. af reports what this script
# prints, so anything you do not surface here reaches nobody.
[ -n "$ADDR" ] || { echo "af agent-server printed no banner; its output was:" >&2; cat "$LOG" >&2; exit 1; }
printf '{"url":"http://%s","token":"%s"}\n' "$ADDR" "$TOKEN"
```

> **Why `nohup` and not `setsid`?** `setsid` is part of util-linux and **does not
> exist on macOS**, so a Mac user copying an earlier version of this recipe got
> `setsid: command not found` (#1946). `nohup` is POSIX and present on both.
>
> What the detach actually needs here is for the server to outlive this script,
> and the `&` alone already delivers that: the script exits immediately, and the
> kernel reparents the server to `init`/`launchd`. `nohup` adds immunity to the
> `SIGHUP` you would get by running this script by hand from a terminal that then
> closes. `setsid` additionally made the server a session leader, which this
> recipe never relied on.
>
> If you replace this with your own launcher, keep the two properties that matter:
> the server **survives this script**, and it **does not inherit this script's
> stdout**. A process writing to stdout puts its output where only the endpoint
> record belongs, and the provision fails naming the line. Its stderr may be
> inherited freely.

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

For a real orchestrator, replace `WORKDIR=…` / `nohup af …` with your provisioning (create a pod, `ssh` to a host, spin up a Modal/Daytona sandbox) and run `af agent-server` there, then surface its banner however you reach it (e.g. read a published address). The daemon only needs the `url` and `token` back — over plain HTTP, so make sure the address you hand back is reachable from the daemon on a private network or tunnel.

## Migrating to an endpoint-only stdout

Since [#2845](https://github.com/sachiniyer/agent-factory/issues/2845), `launch_cmd`'s stdout carries the endpoint JSON and nothing else. **Most scripts need no change** — if yours only ever `echo`s the endpoint, and everything it backgrounds is already redirected, you are done. What breaks is a script that lets another writer inherit stdout, and the fix is one redirect per writer.

**Do I need to change anything?** Run your `launch_cmd` by hand with stderr discarded, so what you see is exactly what af parses. **This provisions real infrastructure**, so the reap is part of the check, not an afterthought — run both lines:

```bash
./.agent-factory/hooks/launch.sh --name migration-check --title 'migration check' \
  --repo "$(git remote get-url origin)" 2>/dev/null
./.agent-factory/hooks/delete.sh --name migration-check      # reap it, whatever the above printed
```

One JSON object and nothing else means you are already compliant. Any other line — a progress message, a tunnel's log, a second record — is what af now refuses.

**Before** — the tunnel inherits stdout, so its log lines land on the endpoint's stream:

```bash
# provision, then open the forward that makes the server reachable
kubectl port-forward "pod/$NAME" 8443:8443 &                    # ← prints "Forwarding from …" on stdout
echo "waiting for the agent-server"                             # ← and this is on stdout too
printf '{"url":"http://127.0.0.1:8443","token":"%s"}\n' "$TOKEN"
```

**After** — every other writer is given somewhere else to go:

```bash
kubectl port-forward "pod/$NAME" 8443:8443 >/dev/null 2>&1 &    # ← or >>"$WORKDIR/tunnel.log" 2>&1
echo "waiting for the agent-server" >&2                         # ← progress belongs on stderr
printf '{"url":"http://127.0.0.1:8443","token":"%s"}\n' "$TOKEN"
```

The endpoint `printf` is unchanged, and always is: it is the one thing stdout is for. The three substitutions around it cover essentially every script:

| On stdout today | Change to |
|---|---|
| `mytunnel &` | `mytunnel >/dev/null 2>&1 &` — or `mytunnel >>"$WORKDIR/tunnel.log" 2>&1 &` to keep the log |
| `echo "progress…"` | `echo "progress…" >&2` |
| `some-cli provision` (chatty, output unused) | `some-cli provision >&2` — or `some-cli provision >/dev/null 2>&1` to drop it |

Keep the endpoint `printf`/`echo` itself exactly as it is: it is the one thing stdout is for. If you miss a writer, af tells you which line it was — the error quotes it and names the redirect.

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
