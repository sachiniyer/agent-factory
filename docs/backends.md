# Backends (runtimes)

A session's **backend** decides *where* its workspace and agent run. Every
backend exposes the same session surface — attach, preview, prompt delivery, the
live PTY stream — so the TUI, CLI, and daemon drive a containerised session much
like a local one. The one difference is **tab management**: only a local session
has a daemon-side worktree to spawn tabs into, so adding/closing tabs (`t`/`w`,
`af sessions tab-create`) is local-only; docker, ssh, and hook sessions carry a
fixed single agent tab.

| Backend | Where the agent runs | Selected with |
|---------|----------------------|---------------|
| `local` (default) | a git worktree + tmux on the daemon's own machine | nothing (the default), or `backend = "local"` |
| `docker` | a container on the daemon's Docker host | `backend = "docker"` + `docker.image` |
| `ssh` | a remote host over SSH | `backend = "ssh"` + `ssh.host` |
| `hook` | wherever your provisioner scripts put it | `backend = "hook"` (see [Hook backend](#hook-backend-bring-your-own-provisioner)) |

Select a backend per-repo in `.agent-factory/config.json`, or per-session with
`af sessions create --backend <name>` (the flag overrides the repo config).

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
clones the branch back (see [Archive & restore](#archive--restore)).

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

The Docker runtime does not copy the daemon's whole environment into the
container. It forwards only present GitHub token/proxy/CA variables, the
selected agent's built-in authentication names, and names explicitly listed in
the global `session_env_passthrough` setting. Docker receives each as `-e NAME`,
so the value does not appear in the docker command line. Container-native
`HOME` and `PATH` remain owned by the image. An environment added through
`docker.run_args` still has to be built in or named in
`session_env_passthrough` before the agent pane may inherit it.

For private GitHub repositories, an environment-backed `GH_TOKEN` or
`GITHUB_TOKEN` is forwarded automatically and supports clone, `gh`, and HTTPS
push. If you use stored `gh` credentials instead, mount the relevant config or
credential-helper resources deliberately with `docker.run_args`; host paths and
native keyrings are not implicitly mounted into a container.

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
- A libc/architecture compatible with the daemon's `af` binary. A **static**
  `af` build (`CGO_ENABLED=0`) runs on any base (musl/alpine included); a
  dynamically-linked `af` needs a matching glibc base (e.g. `debian:slim`).

The container's internal agent-server port is `8000`; avoid binding it in your
image.

A minimal example Dockerfile:

```dockerfile
FROM alpine:3.20
RUN apk add --no-cache git tmux bash
# add the agent CLIs your sessions need, e.g. claude / codex / aider
```

### Operations

- **List managed containers:** `docker ps -a --filter label=af.session`
- **Reap a leaked container** (should never be needed — kill reaps automatically):
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

---

## SSH backend

With `backend = "ssh"`, a session runs on a remote host you reach over SSH — the
built-in, opinionated version of what a `hook` `launch_cmd` did by hand:

1. The daemon dials `ssh.host` with the Go SSH client (`golang.org/x/crypto/ssh`
   — it does **not** shell out to the `ssh` binary), reusing your keys and
   verifying the host key against `known_hosts`.
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
back (see [Archive & restore](#archive--restore)).

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
| `ssh.port` | no | The SSH port (default: 22; a port in `ssh.host` wins). |
| `ssh.identity_file` | no | Path to the private key for auth. Empty ⇒ `ssh-agent` (`SSH_AUTH_SOCK`) and the default `~/.ssh` keys are tried. `~` is expanded. |
| `ssh.known_hosts` | no | Path to the `known_hosts` file the remote's host key is verified against (default: `~/.ssh/known_hosts`). `~` is expanded. |

**Host-key verification is always on** (secure by default — an unverified host
could MITM the connection and capture the bearer token). There is no
"insecure/ignore" option: for an ephemeral or freshly-provisioned host, seed its
key first with `ssh-keyscan -H host >> ~/.ssh/known_hosts` (or point
`ssh.known_hosts` at a dedicated file), then create the session.

The Go SSH client never forwards the daemon's environment. The remote
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
Like the other off-box backends it does not support adding or closing tabs (no
daemon-side worktree).

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

For the disposable sandbox backends (`docker` and `ssh`), archive and restore
are **push/pull of the session branch** — the durable workspace is the branch on
GitHub (`origin`), not the sandbox:

- **Archive** (`af sessions archive`) pushes the session branch to `origin`, then
  tears the sandbox down (reaps the container / removes the remote session dir +
  closes the tunnel). The session record is preserved as an inert **Archived**
  row — restorable, but consuming no container or remote process.
- **Restore** (`af sessions restore`) re-provisions a **fresh** sandbox that
  clones the pushed branch back, restarts the `af agent-server`, and relaunches
  the agent. The session resumes from the pushed branch state.

This is the same flow for both backends (it is written once against the runtime
seam), and it is why `docker`/`ssh` reach full capability parity with `local` —
`Archive` and `Recover` are both supported. A **Lost** sandbox session (its
container/remote died) recovers the same way: re-provision + clone the branch
back.

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
