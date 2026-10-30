# Backends (runtimes)

For anyone who wants a session's agent to run somewhere other than this machine.
After this page you will know what each backend needs, how to point a repository
at one, and what changes about tabs, archive, and restore when a session is
off-box.

A session's **backend** decides *where* its workspace and agent run. Every
backend exposes the same session surface — attach, preview, prompt delivery, the
live PTY stream — so the TUI, CLI, and daemon drive a containerised session much
like a local one. Tab admission follows what each kind needs: shell/process tabs
need a local PTY and process; an off-box session serves them too, in the
workspace, when its agent-server advertises a tab API; a VS Code tab on an
off-box session opens the editor running on the
session's host, rooted at the workspace and relayed through the agent-server
(code-server or openvscode-server must be installed there; af detects it and
never installs it). A metadata-only web tab spawns neither and can be added
to an off-box session (docker, ssh, sandbox, hook) when its target is an external
HTTPS URL, or a loopback address (including `--port`): the daemon's web-tab proxy
reaches the workspace's own `127.0.0.1:<port>` through the agent-server's port
relay over the session's existing connection, so the tab shows the dev server
running inside the sandbox, never a daemon-host port. Plain HTTP external URLs
cannot be framed by the HTTPS web UI. Admitted remote web tabs
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

A host from your global [`hosts` list](configuration.md) is picked the same way and
places the session on that host over ssh: `af sessions create --host <name>` (or
`af sessions send-prompt --create --host <name>`, or `--host` on `af tasks add`), a
`host: <name>` row in the TUI's `ctrl+r` list and the web's backend select, and a
**Host** field in the task forms. Every surface refuses a name that is not in the
list with the same message, and lists an unusable host with the reason.

!!! note "`af agent-server` is a backend, not the web UI"
    The non-local backends work by running an **`af agent-server`** in the remote
    workspace — a headless, single-workspace process that a daemon dials and
    drives. It serves **no frontend**: opening its port in a browser gets you a
    404 telling you so. The **web UI is served by the daemon** — running `af`
    starts one on demand, then open <http://localhost:8443>. See
    [The web client](web.md).

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
| amp | `~/.local/share/amp/secrets.json` *(amp's stored API key; if you enabled amp's native keyring storage there is no file, so name `AMP_API_KEY` in `session_env_passthrough` instead)* |
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
- **SELinux-relabeled, on hosts that need it.** On a host running SELinux in
  **enforcing** mode — the Fedora, RHEL and CentOS default — the mount is
  `:ro,z` instead of plain `:ro`. Without the relabel the container still starts
  and only the *read* is denied, which surfaces as a session that is silently
  unauthenticated rather than as an error. `z` is the **shared** label rather
  than `Z`: every concurrent session running that agent mounts the same host
  file, and the private label would relabel it out from under the others. Note
  that on such a host this does relabel the credential file itself to
  `container_file_t`.

    af reads `/sys/fs/selinux/enforce` to decide. **Permissive** mode is treated
    as not needing it — SELinux logs the denial there rather than acting on it,
    so the read succeeds and your file is left untouched. A host switched to
    enforcing *while* sessions are running needs those sessions restarted to pick
    the label up.

    **The rule is: af relabels unless it can prove the relabel is unnecessary.**
    `z` is inert wherever SELinux is not enforcing, while skipping it on an
    enforcing host costs you a silently unauthenticated session — so the two
    directions are not worth treating alike, and every uncertain case resolves to
    relabeling. Concretely, af keeps the relabel when:

    - it cannot read the SELinux mode at all;
    - the Docker endpoint is **not local** (`DOCKER_HOST` / `DOCKER_CONTEXT`
      pointing off this machine). Docker resolves *and labels* a bind source on
      the daemon host, so this host's SELinux mode describes the wrong machine;
    - no enforce file is visible **but the kernel registers `selinuxfs`**. This is
      the af-in-a-container case: with the host's Docker socket mounted, af sees a
      local `unix://` endpoint, yet `selinuxfs` is usually not mounted inside a
      container — so an absent enforce file there means "af cannot see it", not
      "the machine has no SELinux". `/proc/filesystems` is kernel-global rather
      than namespaced, so it still describes the machine that will label the
      mount.

    One case af cannot see through: a `unix://` socket **forwarded to a different
    kernel** (`socat`, or a VM reached through a local path) is indistinguishable
    from a local daemon. Docker Desktop is the common instance and its VM does not
    enforce SELinux, so this is theoretical — but if you forward a socket to an
    enforcing host, set the mount mode yourself via `docker.run_args`.

    The **account** mount (below) takes the same decision, for the same reasons —
    af installs both mounts into the same container on the same engine, so they
    never disagree. One consequence to know about: an account path containing a
    `:` needs Docker's `--mount` form, which cannot carry a relabel, so af
    refuses that combination on a host that needs one. Move `AGENT_FACTORY_HOME`
    to a path without a colon.

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
- **`cat`** — reads the agent-server's startup banner, and, for an
  account-scoped session, the container's own `/proc/1/mountinfo` when af
  verifies the account boundary (#3598).
- **`stat`**, **`mkdir`**, **`chown`**, **`find`** and **`id`** — af probes the container's
  default user and prepares a writable HOME and `/workspace` for that uid, so a
  BYO image that sets a non-root `USER` directive (a common hardening practice)
  provisions the same way a root-default image does. An account-scoped session
  probes the **mounted account**'s owner; a session with no `--account` probes
  the **image**'s default `USER` (`id -u`/`id -g`), and af chowns HOME and
  `/workspace` to that uid as root before provisioning as that user. `stat` is
  account-only (it reads the mounted directory's owner); `id` runs on every
  non-account session, and `mkdir`/`chown` run on an account session or when
  the probe finds a non-root default user — a root-default image on a
  non-account session skips the chown entirely, so a host directory
  bind-mounted at `/root` or `/workspace` is never re-owned and a HOME set via
  `run_args` stays in effect. Before chowning, af reads the container's own mount table
  (`/proc/self/mountinfo`, via `cat`, `cd` and `pwd -P`) and never lets the
  recursive chown cross into **any** mount point at or under HOME — however it
  was created (`-v`, `--mount`, `--volumes-from`, `--tmpfs`, an image `VOLUME`,
  an agent-credential mount) — so it neither re-owns the operator's host files
  (a read-write bind) nor EROFS-aborts on a read-only one. `find` walks HOME with
  that prune list when there is at least one such mount; with none it is a plain
  `chown -R`. Only a directory whose containing mount is the container's own root filesystem is chowned: if HOME or `/workspace` is itself a mount, or lives beneath one (a host bind, a volume, a tmpfs, or a symlinked HOME that resolves into one), af skips that chown and logs why. If the
  mount table cannot be read or understood, provisioning fails with an error
  naming it rather than chowning blind. `chown` runs with `-h` in that form so a symlink the image ships under `/root`
  is re-owned itself, not its target outside HOME. These entries and `cat` were
  already required and simply missing from this list (#3602 review); busybox and
  coreutils each provide all of them.
- The **agent CLIs** you intend to run (claude, codex, aider, gemini, …).
- A libc/architecture compatible with the daemon's `af` binary. The copied-in
  binary is the **daemon's own executable**, and the release artifacts
  `install.sh` fetches are a plain `go build` (cgo on) on `ubuntu-24.04` —
  i.e. **dynamically linked against glibc**. So your base must be **glibc, and
  its glibc must be ≥ the daemon build's** (`ubuntu-24.04` is glibc 2.39
  today): `debian:trixie`/`ubuntu:24.04` work, `debian:bookworm` (2.36) is too
  old, and a musl base (alpine) will not run the binary at all. If you rebuild
  the daemon on a newer-glibc distro, rebuild the image on a matching-or-newer
  base. (Docker keeps this daemon-binary copy on purpose. The sandbox/ssh
  transports instead stream `agent-factory-linux-<arch>-static.tar.gz` — a
  CGO_ENABLED=0 release artifact that execs on ANY libc, alpine included —
  so only docker's image needs this glibc rule.)

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
- A **local** Docker engine — `DOCKER_HOST` / `DOCKER_CONTEXT` pointing at this
  machine. `docker run` publishes the in-container agent-server port on the engine
  host's loopback (`-p 127.0.0.1::8000`), and the daemon dials its own loopback to
  reach it, so a remote engine yields an endpoint this daemon can never connect to.
  af refuses a remote engine at create time (and reports it unavailable at choose
  time) rather than provisioning a session that fails later with an opaque
  `connection refused` to `127.0.0.1`.
- **At most one of `DOCKER_HOST` and `DOCKER_CONTEXT`**, unless both point at the
  same endpoint. When both are set, the docker CLI dials `DOCKER_HOST`, but its
  reference documents `DOCKER_CONTEXT` as the override. af cannot tell which one
  you meant, so it refuses the combination and names both variables in the
  error — including in the daemon-startup orphan sweep, which skips its
  destructive pass rather than guess an engine. Endpoint spellings that provably
  name one engine still pass: letter case, a scheme's default port, and trailing
  dots or slashes are normalized before af compares them. With only one of them
  set, af follows the CLI: `DOCKER_HOST` wins over the context selected with
  `docker context use`, and `DOCKER_CONTEXT` wins over it too.
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

To run a repo's sessions in containers by default, set `"backend": "docker"` in
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
   (or under the picked host's `workspace_root`, see [configuration.md](configuration.md))
   and clones the repo (from the repo's `origin` remote) into `workspace/` there.
3. It probes the remote's platform (`uname -s`/`uname -m`), picks an `af` binary
   the host can run — `sandbox.binary` if set, else the daemon's own binary on a
   matching platform, else the sha256-verified release artifact for the remote's
   platform — and installs it under `~/.af-bin/` (a per-host cache named by the
   binary's sha256, so installing one build never rewrites another that a
   running session links to; the next sessions reuse a matching file). An
   explicit `sandbox.binary` is used even on a platform af publishes no build
   for. The session directory stays first on the agent's `PATH`, so `af` in the
   pane is the staged one even when the login profile puts an older install
   ahead of it. It then runs
   `af version` **on the remote** and refuses to launch unless the staged binary
   reports the daemon's own version. Only then does it start an
   **`af agent-server`** bound to `127.0.0.1:0` (a random loopback port — never
   exposed on the remote's public interface), behind the same bearer-token
   protocol (plain HTTP).

   The bearer token is **per-session**: the daemon derives it from a 0600
   `sandbox-runtime-key` in its own AF home plus a per-session nonce, and writes
   it to `af-server-token` (0600) in the session directory, where the remote
   `af agent-server` reads it — beside its own binary, never through a flag or
   an environment variable, so nothing the agent-server spawns can inherit it.
   The session record persists the agent-server's address and version and the
   nonce reference (`token_ref`) — never the token — so the identity a reattach
   or a cleanup needs survives a daemon restart. The reference also rides in the
   agent-server's own environment as `AF_TOKEN_REF` so a host-side
   `/proc/<pid>/environ` probe can bind the live process to the record; it is
   the one variable that probe may read, and it never reaches agent or shell
   panes.
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
| `ssh.identity_file` | no | Path to the private key for auth. Empty ⇒ `ssh-agent` (`SSH_AUTH_SOCK`) and the default `~/.ssh` keys are tried. `~` is expanded. At session create af refuses a path it cannot hand to ssh — missing, unreadable, not a regular file, or containing `%` (ssh expands `%` tokens such as `%h`/`%d` when it loads the key, so af would verify one file while ssh reads another) — rather than silently authenticating with agent or default keys as the wrong identity. |
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
> **Which resolver picks the address, and when it matters.** af resolves the name
> the same way `ssh` does — through the system resolver (`getaddrinfo`, which
> follows `nsswitch.conf` and so uses LDAP, `sssd` or mDNS too), not with Go's own.
> In a `CGO_ENABLED=0` (static) build Go's resolver skips NSS, so where `getent`
> is available af reaches `getaddrinfo` through `getent ahosts`, dials the
> answers in order and pins the first that answers. `getent` is not on macOS, and
> the official Darwin builds are static, so there af cannot reach the system
> resolver and connects by name (resolving in `ssh` each step) as it did before.
> A cgo build dials through `getaddrinfo` directly — unless Go's pure resolver is
> forced on instead (the `netgo` build tag, or `GODEBUG=netdns=go`). af still
> believes it has the system resolver there (it keys that off the `cgo` build tag
> alone), so it does not fall back to `getent`: a name only NSS can answer fails
> the probe, and af connects by name as it did before. Either way, once af pins, `ssh` does no
> resolving at all — the connection is made for it — so every step of a session
> goes to the same place regardless.
>
> af and `ssh` use the same resolver, so the `known_hosts` entry an earlier `ssh`
> recorded against that name ordinarily matches the machine af pins — but the
> same resolver does not guarantee the same *address*, and that is still where this
> can bite. A name that hands back **different machines on different calls** — a
> rotating round-robin — can pin one the entry does not cover; and even with the
> same answers, af's probe pins whichever address actually answers the dial (it
> tries them in order, racing IPv6 and IPv4), so a multi-address name whose
> addresses hold different host keys can pin one the earlier `ssh` did not land
> on. The session then fails to start naming the host, nothing runs on the wrong
> machine, verification refuses it before authenticating, and the earlier advice
> to point `ssh.host` at one machine covers it.
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
- `git`, `tmux`, `sh`, and a platform af ships a binary for (Linux/Darwin on
  amd64/arm64; the linux release artifact streamed to a host is a separate
  static `CGO_ENABLED=0` build, so the host's libc is not a constraint there). The `af` binary is installed onto the remote for you — the
  daemon's own build on a same-platform host, the matching release artifact
  (`agent-factory-linux-<arch>-static` on linux) otherwise, always version-checked against the daemon — so there is nothing to
  pre-install. For an air-gapped or unbuilt-platform host set the global
  `sandbox.binary` key to a daemon-local `af` build for that host.
- The agent CLIs you intend to run (claude, codex, aider, gemini, …).
- The repo must have an `origin` remote the **remote host** can clone from
  (GitHub for a real repo; a `file://` path for a self-contained test).

### Adding and checking a listed host

`af hosts add <name> [user@]host[:port]` saves the host (the same entry
`af config set hosts` writes) and probes it in one step; `af hosts list`,
`af hosts check [name]` and `af hosts remove <name>` complete the group. You own
connectivity (a tailnet, a VPN, your ssh key): af opens no tunnel and edits no
ssh config, and a host that is unreachable or missing a tool is still saved, with
the fix printed and a non-zero exit so a script notices. These verbs act on this
machine's host list and daemon, so they refuse a remote `--daemon-url`.

With `--json` every outcome is exactly one envelope on **stdout** and nothing on
stderr. A failure is `{"data": null, "error": {...}}`. When the host was saved
and a later step did not finish, `data` carries the entry or the probe result
and `error.code` says which: `mutation_committed` (saved, but the probe or the
default pick could not run) or `host_not_ready` (saved and probed, not ready
yet). The exit code is non-zero in both. Concurrent `add`, `remove` and
`af config set` never lose each other's change: the edit is applied by the
writer as one read-modify-write under the exclusive config file lock, so
concurrent writers wait their turn instead of failing or retrying.

A host in the global `hosts` list is probed for `git`, `tmux` (3.2 or newer) and
`gh` without creating a session on it. `af doctor` shows one row per listed host
(ready, blocked with the missing tool, unreachable, or unknown), and the daemon repeats
the probe about every ten minutes, so the host pickers show a recent verdict
instead of "not probed yet". An unreachable host or an unreadable reply records
nothing: it can neither mark a host ready nor clear an earlier blocked verdict.
A create always re-checks the host live.

### Operations

- **Find a session's remote files:** they live under `~/.af-sessions/<title>.XXXXXX`
  on `ssh.host` (or under the picked host's `workspace_root` if set).
- **Reap a leaked session** (should never be needed — kill reaps automatically):
  `ssh <host> 'pkill -f "agent-server --listen"; rm -rf ~/.af-sessions/<dir>'`.

### Testing

CI runs `TestSandboxDaemonE2E` (`.github/workflows/sandbox-e2e.yml`, on every master
push and nightly): a real daemon on a default config drives a `backend = "sandbox"`
session against a containerized sshd host through its own RPCs (create, prompt,
preview, tab, archive, restore, kill, and a task-fired create).
`TestSandboxDisruptionsE2E` then hits one live session: a daemon restart (same
agent-server process, roster and conversation, no re-provision) and a killed ssh
connection (reconnects with no operator action). A third case archives the
session, restarts the host container and the daemon, restores, and compares a
stdout-only fingerprint (HEAD, a staged file, an unstaged edit, an untracked file
and an ignored one) taken before the archive with one taken after the restore. `make backend-sandbox-e2e` runs both tests by hand; it is not a routine
local gate.

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
[Configuration](configuration.md)). The remote `af` binary is staged exactly as
the `ssh` backend stages it — platform-probed, cached under
`~/.af-bin/`, sha256- and version-verified — and the same global-only
`sandbox.binary` override applies.

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
> The `sandbox.ssh` half was closed as won't-fix in #3086 (2026-08-11): af cannot
> know which word of your command is the host, so the two workarounds above are
> the supported answer.

### Operator config on the sandbox

The daemon resolves your policy once and writes it to `af-policy.json` beside the
session (docker: `/tmp/af-policy.json`); the sandbox's `af agent-server` reads that
file and nothing else. It never reads or creates `~/.agent-factory/config.toml` on
the host. The file carries `global_agent_skills`, `worktree_root`, `branch_prefix`,
`session_env_passthrough`, the program overrides you wrote, and the repo's
`post_worktree_commands` as the daemon sees them — uncommitted `.agent-factory`
edits and hooks saved in the TUI included. It never carries `sandbox.ssh`, `hosts`,
`move_cmd` or accounts.


### Inspect a sandbox session's work

The agent's work lives in the sandbox's workspace, not on the daemon host, so
`git status` in your own checkout shows nothing. You do not need ssh to the host
to look at it, or to hand the agent something: every client (TUI, web, phone,
`af` against a remote daemon) opens the same tabs a local session has, and they
run in the workspace.

1. **Status and diff.** Open a shell tab — `af sessions tab-create <title> --kind
   shell`, the TUI's new-tab key, or the web's **Terminal** button — and run `git status`
   and `git diff`. It is the agent's own workspace, so the branch and uncommitted
   changes are the agent's. The shell is listed once the sandbox's agent-server
   serves the tab API; against one that does not, the picker says so instead of
   creating a tab.
2. **Browse and edit files.** Open a VS Code tab — `af sessions tab-create <title>
   --kind vscode`. The editor runs on the host, rooted at the workspace, and the
   daemon relays it, so a remote client reaches it through the same authenticated
   origin as any other tab. The host needs `code-server` or `openvscode-server`;
   af detects one and does not install it, and a missing one shows the install
   hint in the tab. See [VS Code tabs](web.md#vs-code-tabs).
3. **Hand the agent a file.** Put the file in the workspace — drag it into the VS
   Code explorer, or paste its contents into a shell tab (`cat > notes.md`) — then
   tell the agent where it is with `af sessions send-prompt <title> "read
   notes.md"`. The agent sees what is on disk in the workspace; there is no
   separate upload channel.

An **archived** sandbox that was archived without pushing keeps its workspace on
the host (see [Archive & restore](#archive-restore)): there is no live agent to
ask, so read the files from the host, or restore the session to get the tabs
back.

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
Like the other off-box backends it admits web tabs (external HTTPS, or loopback via the port relay). Shell and process tabs are admitted when the `af agent-server` your `launch_cmd` starts advertises `tab-api` (a current build does): they then run on the session's host, with the daemon keeping a roster row for each; an older agent-server without `tab-api` keeps the refusal, since those kinds would need a daemon-side worktree. A VS Code tab opens the editor on the session's host.

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

## Moving a live session to a sandbox (move script)

> **Status:** the building blocks (worktree snapshot, move-script contract,
> all-or-nothing orchestration) live in the `move` package. The
> `af sessions move <title> --to <host>` command is not wired yet. The global-only
> `move_cmd` setting exists (a checked-in value is a load error) but nothing reads
> it yet, so nothing here is user-invokable today; this
> section is the contract the script will run under.

A move seeds the sandbox workspace from a snapshot of the local worktree — the
branch history plus every uncommitted and untracked file — rather than a fresh
clone. `.gitignore`d files are not carried; the operator's **move script** copies
them. The script runs on the daemon box, never from repository config.

- **stdin:** a JSON object with `session_id`, `title`, `branch`,
  `source_worktree`, `host`, `dest_workspace` and (when scoped) `account`.
- **env:** `AF_SSH` — the ssh command **without** the destination (options
  only); `AF_SSH_DEST` — the destination (`user@host` or an ssh alias);
  `AF_SSH_FULL` — the complete command including the destination. So
  `rsync -e "$AF_SSH" file "$AF_SSH_DEST:dir/"` and
  `sh -c "$AF_SSH \"\$@\"" af-ssh "$AF_SSH_DEST" 'cmd'` reach the same machine af does.
  The variables are shell command lines that may contain quoted options (a
  `ProxyCommand` with spaces), so never expand them unquoted: run them through
  `sh -c` with positional arguments, as `examples/move-script/move.sh` does. The same three
  values are in the JSON as `ssh`, `ssh_dest` and `ssh_full`. A free-form
  `sandbox.ssh` command already embeds its host and af cannot split it, so there
  `AF_SSH` and `AF_SSH_DEST` are empty and only `AF_SSH_FULL` is set: run
  `sh -c "$AF_SSH_FULL \"\$@\"" af-ssh-full 'cmd'` and, instead of `rsync -e`, pipe tar:
  `tar -C src -c . | sh -c "$AF_SSH_FULL \"\$@\"" af-ssh-full 'tar -C dir -x'`.
- **exit:** `0` continues; anything else aborts. Any failure before the session
  row switches leaves the local session running exactly as before and reaps the
  half-built sandbox workspace.

See `examples/move-script/move.sh` for a documented example.

## Session-scoped account sync (listed hosts)

> **Status:** `--account` and `default_accounts` are admitted on an ssh session
> placed on a listed host (W7.4). On any other target an explicit `--account` is
> refused at admission with the remedy, and a default account is not applied.

On a host in the global host list (`[hosts.<name>]`), af copies the session's
selected account into **that session's own scope** — `<session dir>/account` on the
host — and points the agent at it with the agent's own variable (`CLAUDE_CONFIG_DIR`,
`CODEX_HOME` or `GEMINI_CLI_HOME`). It never writes the remote user's global agent
config, so the host's own login is neither read nor replaced.

- **Only credential files travel** (claude `.credentials.json`, codex `auth.json`,
  gemini's OAuth files) — not history, settings or skills. They are sent over the
  ssh session's stdin as an archive, so no secret appears in a command line, a
  script or a log.
- **Only to a listed host.** The licence to send credentials is the host-list
  entry, which only the operator writes. A target a repository named (its `[ssh]`
  section) or a free-form `sandbox.ssh` command is refused before anything is sent.
- **Refreshes are written back — as a versioned compare-and-swap from a bounded
  stream.** af remembers the version (a content digest) of each credential file it
  synced into the session. A file the agent did not change is never written back, and
  a changed one replaces the registry copy only while the registry still holds the
  version this session synced in: if another session on the same account refreshed it
  first, or the account was removed, the write is skipped and logged, so a stale
  snapshot can never overwrite a fresher one. The host is untrusted, so its archive is
  read under a hard byte limit while it streams — the command is killed when the limit
  is crossed — and a truncated, oversized or malformed archive is refused whole. The registry is
  replaced ONLY from a fully validated archive: the host command's exit status is
  tar's own (a file that shrank while it was read fails the read), the archive must
  close with tar's end-of-archive marker, hold each allowlisted file exactly once with
  a size matching its header, and carry no empty or NUL-padded content; any failure
  keeps the existing credentials. The byte bound is tar's own arithmetic, so a
  credential at the size cap is accepted.
  Replacement is atomic, mode 0600, credential files only. Teardown writes back one
  last time, then removes the scope. A refresh that loses the race stays on the host
  and is discarded with the session; fan-out to live sessions is not built yet.
- **`handoff --account` and limit failover work on a listed host.** The new account is
  staged beside the live scope while the old agent still runs; the agent-server then
  stops the agent, moves the old scope aside, moves the new one into place and starts
  the agent again, so the old agent never writes into the new account's files. The old
  account is written back (the same compare-and-swap) and removed. The agent keeps its
  identity environment from launch, so the swap is within the **same agent** — a
  cross-agent `--to` with `--account` is refused. The replacement starts a **fresh
  conversation** (a transcript lives in the old account's store) and is told so, with
  the session's task. Every refusal (an unlisted host, an unattributed command, an
  account of another agent, an older sandbox without the route) happens before anything
  is stopped. A failure inside the host puts the old scope back.
- **Sessions on one host share its ssh user**, so they can read each other's
  workspaces and synced credentials, exactly as local worktrees share one user
  today. A Unix user per session is a later hardening, not part of this contract.

**Operator-selected agent config.** The global-only `sandbox_agent_config` key
(`agent:file` entries from a closed set: `claude:settings.json`, `claude:CLAUDE.md`,
`claude:.claude.json`, `codex:config.toml`, `codex:AGENTS.md`) lists agent
personalization af places in that same scope — read from the operator's own agent
home on the daemon machine, written beside the synced credentials, one way, and
removed with them. It applies only on a listed host and only to an account-scoped
session; a repository can never set it. A copied codex `config.toml` is rewritten to
the file credential store so it cannot override the session's account.

Not yet built: fanning a refresh out to every live session on the same account
(providers that rotate refresh tokens log the others out), write-back on a timer or
after a daemon restart.

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
`TabManagement` off (and `Handoff`, which an ssh or sandbox session's agent-server lifts when it advertises `agent-swap`). The static declaration is the floor: an
off-box session whose agent-server advertises `tab-api` lifts `TabManagement` and
serves shell and process tabs off-box too. Without that advertisement an off-box
session carries only its agent tab plus HTTPS web tabs (the one metadata-only kind
admitted off-box; a loopback target is admitted because off-box runtimes relay
loopback through the agent-server) and docs tabs. A **Lost** sandbox session (one whose
sandbox answered that its agent is gone) is reachable, so recovery **relaunches
the agent in place**: the sandbox's agent-server stops the agent (if it is still
running) and starts it again in its existing workspace with the provider's resume,
rebuilding the worktree if it was deleted: on the recorded branch when that
branch still exists (its tip is kept), or — when the branch is gone too — by
recreating it from the recorded base commit (falling back to the origin default
branch, then `HEAD`), which is only attempted when the agent's exact recorded
conversation can be resumed; otherwise the rebuild is refused and the session
stays Lost. A rebuilt worktree runs the repository's `post_worktree_commands`
again, so installs or builds with side effects repeat on that recovery. When the
worktree still exists, the branch, the tree and any uncommitted work are untouched
and no hooks re-run. Either way nothing is pushed,
reaped, re-cloned or re-provisioned. The same relaunch serves `retry-limit` on a
dead agent. A sandbox built before the relaunch route existed (it advertises the
`relaunch` feature on `/v1/health`) falls back to replacing the sandbox: recovery
then pushes its work to `origin` first (anything unpushed would be destroyed by
the re-clone) — and refuses to replace if that push fails. Unreachability alone
is not death — it does not mark a sandbox Lost, and restore refuses to replace a
sandbox it cannot reach.

### Host requirements, credentials and restarts

What the sandbox host needs, and what af does to it:

- **Tools.** `git`, `tmux` and a POSIX `sh` on the
  non-interactive `PATH` of the ssh login (af sends its provisioning scripts as
  one ssh argument that the remote login shell parses). The static `af` binary
  af copies over also has to run there: see the glibc rule under
  [Docker](#docker-backend) for the dynamic build.
- **Git identity is repo-local.** After the clone, provisioning writes the
  committer identity into the workspace's own `.git/config` (`git config
  --local`): the operator's `user.name`/`user.email` as git resolves them on
  the daemon host. Each field falls back separately to its placeholder
  ("Agent Factory" for `user.name`, `af@agent-factory.local` for
  `user.email`) only when that field is unset, so configuring just one keeps
  the configured value and substitutes the placeholder for the other. The
  host user's global git config is left untouched.
- **Credentials.** The clone uses value-free GitHub credentials: no token value is
  written into the clone's config. The per-session agent-server token goes
  through stdin into a `0600` file in the session directory, never through argv
  or the environment of a process the agent starts.
- **Callback credential.** For the agent inside the sandbox to call back into
  the daemon (`AF_DAEMON_URL`/`AF_DAEMON_TOKEN`), the daemon's dedicated
  sandbox callback endpoint must be running — `network.listen_addr` and
  `network.require_token` are not involved, so a default (loopback, tokenless)
  daemon already gives sandboxes a working callback.
  When that is not the case the create is **not** refused: the session is created
  without a callback credential and the reason is reported (`callback_unavailable_reason`
  in the backend picker). The credential admits a bounded `af` surface —
  `Snapshot` returns the caller's project (the sandbox-safe projection
  withholds the tmux name and the repository paths), and every other admitted
  route is bounded by its handler to the caller's own project.
- **Daemon restart.** The agent-server is started with `nohup`, so it
  outlives a daemon restart on the host, and on start the daemon tries to
  reattach to it in the background (`ReattachSandbox`): it re-dials the tunnel
  and rebinds the same agent-server, with no re-provision. What happens next
  depends on the row:
  - **Reattach succeeds.** The row is live again with the same agent-server.
    Nothing is pushed, replaced or restarted.
  - **Reattach is not attempted or fails** (no persisted runtime identity, the
    host does not answer, or the agent-server does not answer). The row stays
    Lost and holds *no* agent-server client: it carries the "not provisioned"
    sentinel (`ErrRemoteSandboxNotProvisioned`), which restore reads as
    *absent* (`probeAbsent` in `daemon/restore.go` and `daemon/lostrestore.go`),
    not as a sandbox that answered dead. Absent means replacement is
    unconditional: the old runtime is torn down and a fresh sandbox is
    provisioned from the session's branch on `origin`. **Nothing is pushed
    first, so work the sandbox never pushed is not preserved on this path**;
    only what already reached `origin` comes back. Provisioning needs the
    host, so a host that is still unreachable fails that attempt and the row
    stays Lost, but that is the provision failing, not a guard.
  - **A live client sees the agent die** (no restart involved, or the row was
    reattached first). If the agent-server answers that the agent is gone, the
    sandbox is still there, so recovery (and a manual restore) pushes its work
    to `origin` before replacing it (`probeAnsweredDead`), and refuses to
    replace anything if that push does not land. If it does not answer at all,
    the probe is unknown and replacement is refused: unreachable is not dead.
  - **`--force-reap`** replaces a sandbox that answered dead, or one that could
    not be probed, without pushing it; it still requires the session's branch
    to be known so the replacement never lands on the default branch.

Work that matters should therefore reach `origin` before a daemon restart can
leave a row un-reattached. For an `ssh`/`sandbox` session, the default archive
leaves the workspace on the host and restore reattaches it (W8.5/W8.6), so an
archived session does not depend on a push at all.

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
