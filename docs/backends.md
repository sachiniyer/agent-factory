# Backends (runtimes)

A session's **backend** decides *where* its workspace and agent run. Every
backend exposes the same session surface — attach, preview, prompt delivery, the
live PTY stream — so the TUI, CLI, and daemon drive a containerised session much
like a local one. Tab admission follows what each kind needs: shell/process tabs
need a local PTY and process, and VS Code needs a daemon-side worktree/editor, so
those remain local-only. A metadata-only web tab spawns neither and can be added
to an off-box session (docker, ssh, sandbox, hook) when its target is an external
HTTPS URL. Loopback targets still need an agent-side relay and are refused for
now; plain HTTP cannot be framed by the HTTPS web UI. Admitted remote web tabs
can be closed, renamed, and reordered like local web tabs.

| Backend | Where the agent runs | Selected with |
|---------|----------------------|---------------|
| `local` (default) | a git worktree + tmux on the daemon's own machine | nothing (the default), or `backend = "local"` |
| `docker` | a container on the daemon's Docker host | `backend = "docker"` + `docker.image` |
| `ssh` | a remote host over SSH | `backend = "ssh"` + `ssh.host` |
| `sandbox` | whatever the operator's own `sandbox.ssh` command reaches (free-form ssh: jump hosts, `ProxyCommand`, bastions) | `backend = "sandbox"` + global `sandbox.ssh` |
| `hook` | wherever your provisioner scripts put it | `backend = "hook"` (see [Hook backend](#hook-backend-bring-your-own-provisioner)) |

The operator-owned backend settings use `[docker]`, `[ssh]`, and `[sandbox]` in
global TOML. Their former flat spellings (`docker_mount_agent_credentials`,
`ssh_host_key_verification`, and `sandbox_ssh`) remain permanent aliases for
existing TOML and JSON. When both spellings are present, the grouped value wins,
including explicit `false` and empty-string values.

Select a backend per-repo in `.agent-factory/config.json`, or per-session on any
surface — each overrides the repo config for that one session:

- **CLI** — `af sessions create --backend <name>`; `af sessions backends` first if
  you want to see which values this repo can actually use.
- **TUI** — press `ctrl+r` while naming the new session and pick from the list.
  Its rows come from the daemon, so each one says whether *this* repo can use that
  backend; picking one it cannot names the config key to fix. Leaving the field
  alone (or choosing **Repo default**) keeps the repo's own setting.
- **Web** — the backend select in the new-session modal, same list, same rule.

!!! note "`af agent-server` is a backend, not the web UI"
    The non-local backends work by running an **`af agent-server`** in the remote
    workspace — a headless, single-workspace process that a daemon dials and
    drives. It serves **no frontend**: opening its port in a browser gets you a
    404 telling you so. The **web UI is served by the daemon** — run `af daemon`
    and open <http://localhost:8443>. See [The web client](web.md).

---

## Docker backend

With `backend = "docker"`, a session runs entirely inside a container:

1. The daemon `docker run`s a container from your image, publishing an internal
   port on a random loopback host port.
2. It clones the repo (from the repo's `origin` remote) into `/workspace` in the
   container.
3. It copies the `af` binary into the container and starts an
   **`af agent-server`** there — a headless, single-workspace server over the
   same bearer-token HTTP/WS protocol the daemon speaks (plain HTTP, no TLS).
4. The daemon drives that in-container agent-server over `http://127.0.0.1:<port>` (a container-published loopback port).
   Attach, preview, prompts, and the live terminal stream all flow over that one
   authed connection.
5. On kill, the container is torn down (`docker rm -f`) — no leaked containers.

The container is **disposable**: durability lives in GitHub (the workspace is a
clone of `repo@origin`), not in the container filesystem — so archive pushes the
branch and reaps the container, and restore re-provisions a fresh container that
clones the branch back (see [Archive & restore](#archive-restore)).

### Configuration

```json
{
  "backend": "docker",
  "docker": {
    "image": "my-org/af-runtime:latest",
    "run_args": ["--memory", "4g", "-e", "MY_VAR=1"]
  }
}
```

| Key | Required | Description |
|-----|----------|-------------|
| `docker.image` | yes | The container image the session runs in (see requirements below). |
| `docker.run_args` | no | Extra arguments appended verbatim to `docker run` (mounts, env, resource limits). |

Letting a containerised agent authenticate is **not** a `docker` key — it is the
global-only, operator-owned `docker.mount_agent_credentials` grant. See
[Agent credentials in a container](#agent-credentials-in-a-container).

The Docker runtime does not copy the daemon's whole environment into the
container. Because repository config selects the image and its binaries, af
does not automatically grant that image the built-in agent, GitHub, proxy, or
CA variables used by local sessions. Only names explicitly listed in the
global `session_env_passthrough` setting cross this boundary. Docker receives
each as `-e NAME`, so the value does not appear in the docker command line.
Container-native `HOME` and `PATH` remain owned by the image. An environment
added through `docker.run_args` still has to be built in or named in
`session_env_passthrough` before the agent pane may inherit it.

Treat each Docker pass-through name as a trust grant to the configured image,
and prefer an image pinned by digest. For example, a private GitHub Codex
session using environment-backed credentials may explicitly list
`GH_TOKEN` and `OPENAI_API_KEY`; that keeps clone, `gh`, HTTPS push, and Codex
authentication working after the operator has made that trust decision. If you
use stored credentials instead, mount only the relevant config or
credential-helper resources deliberately with `docker.run_args`, or set the
global `docker.mount_agent_credentials` grant (below); host paths and native
keyrings are not implicitly mounted into a container.

### Agent credentials in a container

Most agents authenticate from a **file in the daemon user's home** (an OAuth
token or an API key on disk), not from an environment variable — so the
default-deny env boundary above, on its own, leaves a containerised agent
unauthenticated.

The **operator** (not a repo) opts in by setting the global
`docker.mount_agent_credentials`:

```bash
af config set docker.mount_agent_credentials true
```

When it is on, a docker session bind-mounts **read-only** the credential file
for **that session's own agent** — and only that agent. The mapping is:

| The session's agent | Host file(s) mounted read-only → container path under `/root` |
|---------------------|---------------------------------------------------------------|
| claude | `~/.claude/.credentials.json` |
| codex | `~/.codex/auth.json` |
| gemini | `~/.gemini/{oauth_creds,gemini-credentials,google_accounts}.json` (whichever exist) |
| amp | `~/.config/amp/settings.json` |
| opencode | `~/.local/share/opencode/auth.json` |
| aider | *(none — authenticates via API-key env vars; name it in `session_env_passthrough`)* |
| devin | `~/.config/devin/config.json` *(no effect unless your image also carries the devin CLI)* |

**Exactly one row applies per session** — the one for the session's resolved
agent. A codex session is never handed the Claude token, and vice versa (a docker
session's agent is fixed for its life). af resolves the file under the daemon
user's real home and mounts it only if it exists, so:

- **Minimum surface, per agent.** It mounts the credential **file, not the whole
  config directory**: an agent writes state and refreshes its token inside that
  directory at runtime, so a read-only whole-dir mount would break it, and some of
  those dirs reach **gigabytes** of history (`~/.codex`, `~/.local/share/opencode`).
  `~/.claude.json` is deliberately **not** mounted — it is the config/privacy blob
  (MCP server tokens, every project path and prompt history, account and machine
  ids), not the credential; `~/.claude/.credentials.json` is what authenticates.
- **Read-only.** The agent authenticates but cannot refresh or rewrite the host
  credential (a session-lifetime token still works; the disposable container
  discards its own writes on kill regardless).
- **SELinux-relabeled.** The mount is `:ro,z`, so the credential is readable
  inside the container on an SELinux-enforcing host — the Fedora, RHEL and CentOS
  default. Without the relabel the container still starts and only the *read* is
  denied, which surfaces as a session that is silently unauthenticated. `z` is the
  **shared** label rather than `Z`: every concurrent session running that agent
  mounts the same host file, and the private label would relabel it out from under
  the others. Docker ignores the flag where SELinux is disabled, so it costs
  nothing on other hosts; where SELinux is on, note that it does relabel the host
  file itself to `container_file_t`.

!!! warning "A global, operator-owned grant — and a deliberate partial hole"
    `docker.mount_agent_credentials` is **global-only**: a repository selects the
    docker image, but only the **operator** decides whether that image may see
    their credentials. This is enforced by *source scoping*, not a trust gate —
    the key simply is not a repo key, so a cloned `.agent-factory/config.json`
    that sets it hits the standard "global setting, cannot be set per-repo" error
    (the same model as `session_env_passthrough`). Were it repo-settable, a cloned
    third-party repo could grant **itself** the operator's credentials by shipping
    `backend=docker` + its own image + this flag — which is exactly what the
    default-deny boundary (#2329) exists to prevent.

    When on, it **re-exposes one credential file to the configured image** — the
    named exception to the boundary, not a relaxation of it: everything else stays
    blocked, so containment is **partial by design**. Turn it on only for images
    you trust with read access to that agent's credential.

### Image requirements (bring-your-own image)

There is **no `af`-published image**: you bring your own (Sachin-locked
decision). The `af` binary is copied in for you at session start, so your image
only needs the workspace tooling:

- **`git`** — to clone the workspace.
- **`tmux`** — the in-container agent-server drives the agent through tmux.
- **`sh`** and **`sleep`** — the container is kept alive with `sleep`, and setup
  runs through `sh`.
- **`dd`** — used to stream the live PTY (present in both busybox and coreutils).
- The **agent CLIs** you intend to run (claude, codex, aider, gemini, …).
- A libc/architecture compatible with the daemon's `af` binary. The copied-in
  binary is the **daemon's own executable**, and release builds are a plain
  `go build` (cgo on) on `ubuntu-latest` — i.e. **dynamically linked against
  glibc**. So your base must be **glibc, and its glibc must be ≥ the daemon
  build's** (`ubuntu-latest` is glibc 2.39 today): `debian:trixie`/`ubuntu:24.04`
  work, `debian:bookworm` (2.36) is too old, and a musl base (alpine) will not run
  the binary at all. If you rebuild the daemon on a newer-glibc distro, rebuild the
  image on a matching-or-newer base. (A **static** `af`, `CGO_ENABLED=0`, would run
  on any base including alpine — but that is not how release builds are produced;
  the integration test force-builds a static `af` precisely so it can use an alpine
  test image.)

The container's internal agent-server port is `8000`; avoid binding it in your
image.

#### The image this repo ships

This repo carries a starting-point image so a docker-backed session works out of
the box — build it with:

```bash
make session-image        # builds af-session:local from scripts/container/Dockerfile.session
```

It is based on `node:22-trixie-slim` (glibc 2.41; Node 22 for the codex CLI) and
**includes `claude` and `codex`** — the two agents this repo's sessions use. The
Dockerfile carries commented, one-uncomment install lines for the other agents
(`gemini`, `amp`, `opencode`, `aider`); `devin` is not installed because af
forwards it no per-agent credentials. Override the tag with
`make session-image SESSION_IMAGE=my-org/af-runtime:latest`, then set that in the
repo's `docker.image`. There is no registry step (bring-your-own, built locally).

A minimal hand-rolled example, if you would rather not use the shipped one:

```dockerfile
FROM node:22-trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends git tmux ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code @openai/codex
# a musl base (alpine) works only if the daemon's af is a static (CGO_ENABLED=0) build
```

### Operations

- **List managed containers:** `docker ps -a --filter label=af.session`. Each also
  carries `af.home=<AF home>`, the daemon (home) that created it.
- **Automatic orphan reaping:** on daemon startup, af removes any leaked session
  container **this home** created (`af.home` matches) that no live or
  mid-provisioning session owns — the cases where a previous daemon died without
  reaping, a session record was deleted, or a reap raced a create. It is scoped to
  this AF home and to the currently-targeted Docker engine, so it never touches
  another home's or another engine's containers, and a container with no `af.home`
  label (created by a pre-upgrade af) is left alone. Kill still reaps synchronously;
  this is only the backstop for the container that outlived its session.
- **Reap a leaked container by hand** (should never be needed):
  `docker rm -f <id>`

### Requirements on the daemon host

- The `docker` CLI on `PATH` and a reachable Docker daemon.
- The repo must have an `origin` remote the container can clone from (GitHub for
  a real repo; a `file://` path + a `run_args` bind-mount for a self-contained
  test).

### Testing

`make backend-docker-roundtrip` runs the real end-to-end container round-trips on
a host with Docker: it builds a slim git+tmux image, creates a session on the
docker backend, drives the in-container agent-server over `http://`/`ws://` (input →
stream echo → preview/snapshot/liveness), and asserts the container is reaped on
kill. A second case commits real work on the session branch, **archives** it
(asserting the branch is pushed to `origin` and the container reaped), then
**restores** it (asserting a fresh container clones the branch back, the commit
is present, and the session is drivable again). It skips cleanly where Docker is
unavailable.

These builds resolve nothing against a registry. The public base is fetched once
per test binary, with retry, and retagged to a local-only name the Dockerfiles
build `FROM`; a base that cannot be fetched at all skips, the same way a missing
Docker daemon does. `make registry-free-check` proves it, by building the real
Dockerfiles inside a second Docker daemon whose egress is dropped — and re-running
this bug's own reproduction each time, so it cannot pass by measuring a network
that was quietly reachable (#2521).

### Making docker the default backend for a repo

To run a repo's sessions in containers by default, set `backend = "docker"` in
its `.agent-factory/config.json`. This is an operator decision, not a default af
ships — the steps below must be done in order, because the flip does **not**
degrade gracefully on an unprepared box:

1. **Build the image:** `make session-image` (or point `docker.image` at your own
   — see [Image requirements](#image-requirements-bring-your-own-image)).
2. **Grant credentials (operator, global):**
   `af config set docker.mount_agent_credentials true`, so the containerised agent
   can authenticate (see [Agent credentials in a container](#agent-credentials-in-a-container)).
   This is global-only on purpose and cannot be set from the repo config.
3. **Set the repo default:** put `"backend": "docker"` and `"docker": {"image": …}`
   in the repo's `.agent-factory/config.json`.

Only after all three does a **new** session in that repo come up in a container. A
flip with no image built, no reachable Docker daemon, or the grant unset does not
fall back to local — it **fails session creation** with a backend error. So make
docker the default only once the box is ready.

**Existing sessions are unaffected by the flip.** A session's backend is recorded
on its own row (`InstanceData.BackendType`, written from `backend.Type()`), and
the daemon reconstructs each session from that **stored** discriminator on
restart/restore — it never re-reads `backend` from config after create (which is
consulted only when a session is first created). So a running local session comes
back local regardless of the repo's new default; only new creates read it. This is
pinned by `TestFromInstanceData_SandboxBackends`, so a future change that made
restore re-resolve from config would fail a test rather than silently reinterpret
someone's session.

---

## SSH backend

With `backend = "ssh"`, a session runs on a remote host you reach over SSH — the
built-in, opinionated version of what a `hook` `launch_cmd` did by hand:

1. The daemon runs `ssh` (OpenSSH 7.6 or newer must be on the daemon host's PATH;
   7.6 is what `ssh.host_key_verification = "accept-new"` needs, and the other
   postures need less),
   with every setting af owns passed explicitly and **no configuration file read
   at all** — reusing your keys and verifying the host key against `known_hosts`.
2. It creates a fresh per-session directory under `~/.af-sessions` on the remote
   and clones the repo (from the repo's `origin` remote) into `workspace/` there.
3. It streams the `af` binary onto the remote and starts an **`af agent-server`**
   bound to `127.0.0.1:0` (a random loopback port — never exposed on the remote's
   public interface), behind the same bearer-token protocol (plain HTTP).
4. It opens an **SSH local-forward tunnel** from a daemon-local loopback port to
   that remote port and drives the agent-server over `http://127.0.0.1:<localport>`.
   Attach, preview, prompts, and the live terminal stream all flow through the one
   tunneled, authed connection (the SSH tunnel encrypts it; the bearer token still applies end to end inside the
   tunnel — defense in depth).
5. On kill, the remote `af agent-server` is stopped, the session directory is
   removed, and the tunnel + SSH connection are closed — no leaked process, dir,
   or tunnel. The remote host itself is left running (it is your machine, not a
   disposable sandbox).

Like docker, the workspace is **disposable**: durability lives in GitHub (the
workspace is a clone of `repo@origin`) — archive pushes the branch and reaps the
remote session, and restore re-provisions a fresh remote that clones the branch
back (see [Archive & restore](#archive-restore)).

### Configuration

```json
{
  "backend": "ssh",
  "ssh": {
    "host": "build-box.example.com",
    "user": "af",
    "port": 22,
    "identity_file": "~/.ssh/id_ed25519",
    "known_hosts": "~/.ssh/known_hosts"
  }
}
```

| Key | Required | Description |
|-----|----------|-------------|
| `ssh.host` | yes | The remote host (`host` or `host:port`) the session runs on. |
| `ssh.user` | no | The SSH login user (default: the current OS user). |
| `ssh.port` | no | The SSH port (default: 22). Give the port **either** here or as `host:port` in `ssh.host`, not both with different values — a conflict is rejected with an error naming both, rather than one silently winning. Identical values are fine. |
| `ssh.identity_file` | no | Path to the private key for auth. Empty ⇒ `ssh-agent` (`SSH_AUTH_SOCK`) and the default `~/.ssh` keys are tried. `~` is expanded. |
| `ssh.known_hosts` | no | Path to the `known_hosts` file the remote's host key is verified against (default: `~/.ssh/known_hosts`). `~` is expanded. |

> **Your `~/.ssh/config` does not apply, and never has.** af runs `ssh` with
> `-F none`, so neither `~/.ssh/config` nor `/etc/ssh/ssh_config` is read. Only
> the `ssh.*` settings above, `ssh.host_key_verification`, and your keys decide
> how af connects. This is unchanged behaviour — the backend has always ignored
> `ssh_config` — and it is deliberate: this backend's contract is that af enforces
> the host-key posture in code, and a `Host` block could otherwise supply
> `ProxyJump`, `RemoteCommand`, `SendEnv` or `ForwardAgent` behaviours that no af
> setting can override.
>
> **If you need a bastion, a `ProxyJump`, a `ProxyCommand`, or any transport af
> does not model, use `backend = "sandbox"`** with a free-form `sandbox.ssh`
> command — that backend exists precisely so you can express the connection
> yourself, and there your `ssh_config` and `known_hosts` are the whole authority.
> See the sandbox section below.

> **A `ssh.host` with several addresses stays on one machine.** af runs a separate
> `ssh` for each step — the setup commands, the port-forward, and the cleanup — so
> a name answering with more than one address (a round-robin record, several A
> records, a dual-stack host) could once put the workspace on one machine while
> the agent-server or the cleanup landed on another. Nothing looked wrong: every
> step succeeded against a valid host, and the cleanup then removed the wrong
> machine's directory and reported success while the real one leaked.
>
> af now resolves `ssh.host` **once**, when the session is created, and every later
> step dials that one address. The address is recorded with the session, so the
> cleanup reaches the same machine even after the daemon restarts.
>
> **This fixes DNS multiplicity, and only DNS multiplicity.** Read the next
> paragraph before assuming it covers you.

> **A load-balancer VIP is pinned to one machine too.** If `ssh.host` reaches a
> **virtual IP in front of several machines** — an L4/TCP load balancer, an AWS
> NLB, a keepalived/IPVS pair, a Kubernetes service — then pinning the *address*
> would not have been enough, because the address is not a machine identity: the
> balancer picks a backend **per TCP connection**, and af opens one for every
> provision command, for the tunnel, and for the cleanup.
>
> So af asks the machine who it is. On the first connection it reads
> `SSH_CONNECTION`, which tells it the address that backend accepted the connection
> on, and pins every later step to that. The address is recorded with the session,
> so the cleanup reaches the same machine even after the daemon restarts.
>
> Two cases where af leaves the pin alone, both logged:
>
> - **The backend's own address is not reachable from the daemon** — a private
>   address in another VPC, or across a NAT boundary. Pinning it would turn a
>   working host into one where every step fails, so af checks first and keeps the
>   address pin instead.
> - **The balancer preserves the destination address** (direct server return), so
>   the backend reports the VIP itself. There is nothing to re-pin to.
>
> In both cases the session behaves as it did before, and a multi-machine target
> can still split it — **point `ssh.host` at one machine** if that matters.

> **Your host key configuration is unaffected**, which is the part worth knowing:
> `ssh` is still given the **name** as its destination, so `known_hosts` is matched
> and host **certificate** principals are checked exactly as before. The pin
> applies only to the TCP connection, through a `ProxyCommand` that runs `af`
> itself — nothing new to install. An earlier attempt pinned the address as ssh's
> destination and had to restore the name with `HostKeyAlias`; that rejected host
> certificates on every non-default port, and was reverted (#3086).
>
> One genuine, if small, reduction comes with it: **`CheckHostIP` is switched off**.
> OpenSSH disables it whenever a `ProxyCommand` is in use, and it defaulted to *on*
> for OpenSSH 7.6–8.4 (upstream turned the default off in 8.5). On those clients a
> pinned session no longer records or cross-checks the host key against the
> **address** in `known_hosts`, only against the **name**. Verification against the
> name — the guarantee that matters, and the one certificates rely on — is
> unchanged, and af resolving the address itself and reusing that one address for
> every step covers the drift `CheckHostIP` was watching for. It is a trade, not a
> free win: ssh's IP cross-check for af's single-resolution guarantee.
>
> **What this changes for an existing `ssh.host`, deliberately:** a session is tied
> to the machine it was created on for its whole life. Before, if that machine went
> away mid-session, the next step would resolve the name again and might reach a
> different one and appear to keep working — which is the bug, not a feature: the
> workspace it needed was on the first machine. Now that step fails against the
> machine holding the workspace, which is the honest answer. Choosing a machine
> still tries each address the name answers with, so a host with one dead address
> and one live one is picked correctly at create time.
>
> Nothing else about connecting changes: same `known_hosts` matching, same
> certificate principals, same keys, same `ssh_config`-is-not-read rule, and no new
> software to install. There is nothing to migrate — existing sessions created
> before this keep working and are cleaned up by name exactly as they were.
>
> If af cannot resolve the name at create time, or cannot run itself as the relay,
> it says so in the log and connects by name, exactly as it did before — a host af
> cannot look up still works. The pin is an improvement on connecting by name, so
> when it cannot be applied af falls back rather than failing the session.
>
> **Which resolver picks the address, and when it matters.** af resolves with Go's
> built-in resolver, which reads `/etc/hosts` and DNS. `ssh` resolves with
> `getaddrinfo`, which follows `nsswitch.conf` and so can also use LDAP, `sssd` or
> mDNS. Once af pins, `ssh` does no resolving at all — the connection is made for
> it — so every step of a session goes to the same place regardless.
>
> The one place this can bite is a `known_hosts` entry recorded **earlier**, by a
> plain `ssh` or an older session, against whichever machine `getaddrinfo` picked.
> If your resolvers select different machines *and* those machines have different
> host keys, the pinned one presents a key that entry does not match and the session
> fails to start, naming the host. Nothing runs on the wrong machine — verification
> refuses it before authenticating. Point `ssh.host` at one machine if you hit this.
>
> **`accept-new` pins only once it knows the host.** On the very first connection to
> a host there is no entry to check against, so `accept-new` would simply record
> whatever key the pinned machine presented — and if that were the wrong machine,
> the session would run there silently and the name would stay bound to it. af
> therefore does not pin that first connection, and pins normally afterwards.
> `insecure` verifies nothing, so it is never pinned. Both cases are logged, and
> both keep the multi-address exposure this section describes.

**Host-key verification is strict by default** (secure by default — an unverified
host could MITM the connection and capture the bearer token). The operator can
relax it with the global-only **`ssh.host_key_verification`** key:

| `ssh.host_key_verification` | Behavior |
|-----------------------------|----------|
| `strict` (default) | Verify against a `known_hosts` file; refuse an unknown or changed key. Existing behavior — no change. |
| `accept-new` | Trust-on-first-use: record an **unknown** host's key on first connect and proceed, but still refuse a **changed** key. This removes the pre-seed step for ephemeral hosts. |
| `insecure` | No verification. A man-in-the-middle can capture the session bearer token; use only on a trusted network. |

It is **global-only** (operator-owned), not part of the repo-settable `ssh` table:
a repo selects `ssh.host`, but only the operator decides whether to relax
verification — a repo-settable waiver combined with a repo-settable `ssh.host`
would be a one-commit man-in-the-middle. Set it with
`af config set ssh.host_key_verification accept-new`.

`accept-new` records learned keys in an **af-owned store** under the AF home
(`$AGENT_FACTORY_HOME/ssh_known_hosts`) by default — never your shared
`~/.ssh/known_hosts` — or in `ssh.known_hosts` if you set that explicitly.

If you keep the default `strict` and need to reach a freshly-provisioned host,
either switch that host to `accept-new` for the first connect, or add its key to
your `known_hosts` out of band (`ssh-keyscan -H host >> ~/.ssh/known_hosts`, or
point `ssh.known_hosts` at a dedicated file), then create the session.

**Legacy cleanup records.** The `ssh.host_key_verification` posture is stored alongside a
killed session's cleanup record, but records written before that field existed
carry no posture, so they are cleaned up strictly. If such a record's host is not
in the strict `known_hosts` file, no retry can ever complete it — af says so once
and stops retrying, naming the host to add, rather than backing off and retrying
forever. Restarting the daemon re-attempts it, so adding the key is enough to let
the cleanup finish. This applies only to those pre-existing records · every
session created since records its posture and is unaffected.

af's `ssh` invocation never forwards the daemon's environment. The remote
agent-server and pane use credentials already present for the remote login
account, filtered through the same built-in allowlist. Names in the daemon's
global `session_env_passthrough` list are sent to the remote as names only; if a
matching variable exists in the remote account's environment, the agent may
inherit it. A local token is never copied to the SSH host implicitly.

### Requirements on the remote host

- An SSH server you can log into with a key, permitting **TCP forwarding**
  (`AllowTcpForwarding yes` — the default on most distros; the runtime reaches the
  remote agent-server through an SSH local-forward tunnel).
- `git`, `tmux`, `sh`, and a libc/architecture compatible with the daemon's `af`
  binary (a **static** `af` build, `CGO_ENABLED=0`, runs on any base). The `af`
  binary is streamed onto the remote for you — always version-matched to the
  daemon, so there is nothing to pre-install.
- The agent CLIs you intend to run (claude, codex, aider, gemini, …).
- The repo must have an `origin` remote the **remote host** can clone from
  (GitHub for a real repo; a `file://` path for a self-contained test).

### Operations

- **Find a session's remote files:** they live under `~/.af-sessions/<title>.XXXXXX`
  on `ssh.host`.
- **Reap a leaked session** (should never be needed — kill reaps automatically):
  `ssh <host> 'pkill -f "agent-server --listen"; rm -rf ~/.af-sessions/<dir>'`.

### Testing

`make backend-ssh-roundtrip` runs the real end-to-end SSH round-trips on a host
with Docker: it stands up a throwaway `sshd`+git+tmux container as the ssh target
(no external host, no dependency on the box's own sshd), creates a session on the
ssh backend pointing at it, drives the remote agent-server over the ssh-tunneled
`http://`/`ws://` (input → stream echo → preview/snapshot/liveness), and asserts the remote
process is reaped + the session dir removed + the tunnel closed on kill. A second
case commits real work, **archives** it (branch pushed to `origin`, remote
sandbox reaped), then **restores** it (a fresh remote clones the branch back, the
commit is present, the session is drivable) — the identical push/pull-branch
flow, over ssh. It skips cleanly where Docker is unavailable.

---

## Sandbox backend

`backend = "sandbox"` reaches the target through **your own** ssh invocation. The
global `sandbox.ssh` key holds a free-form command line — whatever already works
in your terminal, including a jump host, a `ProxyCommand`, a bastion, or any flag
`backend = "ssh"` does not model — and af runs the same provision, tunnel, and
cleanup steps over it. Your `ssh_config`, `known_hosts`, and host-key posture are
the whole authority here; af adds none of its own. It is global-only and
operator-owned, because af **executes** it on the daemon host (see
[Configuration](configuration.md)).

> **Known limitation: af cannot pin a `sandbox.ssh` target to one machine.**
> af runs a separate invocation of your command for each step — the setup
> commands, the port-forward, and the cleanup. If it reaches a name with several
> addresses (a round-robin record, a load balancer, a dual-stack host), each
> invocation resolves independently, so the workspace can end up on one machine
> while the agent-server or the cleanup lands on another. Nothing looks wrong:
> every step succeeds against a valid host, and the cleanup then removes the wrong
> machine's directory and reports success while the real one leaks.
>
> `backend = "ssh"` no longer has this problem because af composes that command
> and knows which token is the host. `sandbox.ssh` is **your** command, and af
> cannot know which of its words is a hostname — an argument to `-o`, a jump-host
> spec, a wrapper's own flag, or the target — so there is nothing it can safely
> substitute. Guessing would silently rewrite the command you asked for.
>
> Two workarounds, both under your control:
>
> - **Point the command at a single machine** — a literal address, or a name with
>   one address. This is the simplest fix and needs no other change.
> - **Pin it yourself**, the same way af does for `backend = "ssh"`: keep the name
>   as ssh's destination so `known_hosts` and certificate principals still match,
>   and put the address in a `ProxyCommand` — for example
>   `ssh -o ProxyCommand='nc 198.51.100.8 22' build-box.example.com`. Do not pin by
>   making the address the destination: that forces a `HostKeyAlias`, and no alias
>   value satisfies both a plain `[name]:port` entry and a certificate principal.
>
> Tracked in #3086, which is deliberately left open for this half.

---

## Hook backend (bring-your-own-provisioner)

`backend = "hook"` is the escape hatch for infrastructure the built-in `docker`
and `ssh` runtimes don't model — Kubernetes, Modal, Daytona, a bastion with
exotic auth, a bespoke orchestrator. Instead of an opinionated built-in, **you**
provide the provisioning: two shell scripts that stand the workspace up and tear
it down.

Since **#1592 Phase 4 PR7** the hook backend follows the **same
provision-and-expose contract** as `docker`/`ssh`. Your `launch_cmd` clones the
repo on your infra, starts an **`af agent-server`** there, and echoes that
server's authed endpoint (`{url, token}`); the daemon then
drives the session over that `ws://` stream — so a hook session matches a local,
docker, or ssh one on attach, type, resize, preview, archive/restore, and kill.
Like the other off-box backends it admits only external HTTPS web tabs; shell,
process, and VS Code tabs need a daemon-side worktree.

```json
{
  "backend": "hook",
  "remote_hooks": {
    "launch_cmd": "./.agent-factory/hooks/launch.sh",
    "delete_cmd": "./.agent-factory/hooks/delete.sh"
  }
}
```

The mechanics of `launch_cmd` (its arguments, the JSON endpoint it must echo,
how to start an `af agent-server` on your infra), `delete_cmd`, session-name
slugging, and `af doctor` validation live in the dedicated guide — see
**[Remote hooks](remote-hooks.md)**. This is intentionally the built-in `ssh`
runtime done by hand: if plain SSH-to-a-host covers your case, prefer `ssh`
(zero scripting); reach for `hook` only when it doesn't.

### Migrating from the old `remote_hooks` contract

PR7 is a **breaking, clean-break change**. The old hook contract — `launch_cmd`
returning a session id, plus `list_cmd`/`attach_cmd`/`terminal_cmd` for
enumeration, terminal proxying, and preview capture — has been **removed**;
`launch_cmd` now returns an `af agent-server` endpoint and the only other script
is `delete_cmd`. A config that still sets a removed key is rejected with an error
pointing at the guide.

The migration is mechanical (your `launch_cmd` gains an `af agent-server` start
and echoes its URL). Rather than duplicate it here, follow the copy-pasteable
recipe in
**[Remote hooks → Migrating from the old contract](remote-hooks.md#migrating-from-the-old-contract)**.

---

## Archive & restore

For the disposable sandbox backends (`docker`, `ssh`, and `sandbox`), archive and restore
are **push/pull of the session branch** — the durable workspace is the branch on
GitHub (`origin`), not the sandbox:

- **Archive** (`af sessions archive`) pushes the session branch to `origin`, then
  tears the sandbox down (reaps the container / removes the remote session dir +
  closes the tunnel). The session record is preserved as an inert **Archived**
  row — restorable, but consuming no container or remote process.
- **Restore** (`af sessions restore`) re-provisions a **fresh** sandbox that
  clones the pushed branch back, restarts the `af agent-server`, and relaunches
  the agent. The session resumes from the pushed branch state.

This is the same flow for every off-box backend (it is written once against the runtime
seam), and it is why `docker`/`ssh`/`sandbox`/`hook` match `local` on the lifecycle
capabilities — `Archive` and `Recover` are both supported. All four are declared
off-box in one place (`backendProvisionsOffBox`), and all four share a single
capability declaration, so none of them can differ from the others here without
that being a deliberate change. Parity is not total: they declare
`TabManagement` and `Handoff` off, which is why an off-box session carries only
its agent tab plus any external HTTPS web tabs (the one metadata-only kind
admitted off-box). A **Lost** sandbox session (one whose
sandbox answered that its agent is gone) is reachable, so recovery pushes its
work to `origin` before replacing it (anything unpushed would be destroyed by
the re-clone) — and refuses to replace if that push fails. Unreachability alone
is not death — it does not mark a sandbox Lost, and restore refuses to replace a
sandbox it cannot reach.

### What survives, and what doesn't

Because the sandbox is thrown away, **only what reaches `origin` survives** an
archive:

- **Committed work** on the session branch is pushed, so it is fully preserved.
- **Uncommitted work** is snapshotted into a WIP commit
  (`af: pre-archive snapshot (uncommitted work)`) and pushed too, so the working
  tree is not lost — matching the `local` worktree-move archive's "nothing lost"
  guarantee as closely as the disposable model allows. If you would rather not
  carry that WIP commit, commit your work yourself before archiving.
- **The agent's conversation history** lived only in the disposed sandbox and is
  **not** restored — a fresh agent runs on the restored branch. (The `local`
  backend, which relocates the worktree in place, does resume the conversation;
  this is the one place the disposable model differs.)

### Requirements

The sandbox must be able to **push** to the repo's `origin` — the same remote it
cloned from. For a real repo that means the container/remote has git credentials
for `origin` (an HTTPS token or an SSH deploy key); for a self-contained test a
read-write `file://` remote works. If the push fails, the archive fails and the
session is left running (nothing is torn down), so no work is lost.
