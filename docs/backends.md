# Backends (runtimes)

A session's **backend** decides *where* its workspace and agent run. Every
backend exposes the exact same session surface — attach, preview, prompt
delivery, the live PTY stream, tabs — so the TUI, CLI, and daemon drive a
containerised session identically to a local one.

| Backend | Where the agent runs | Selected with |
|---------|----------------------|---------------|
| `local` (default) | a git worktree + tmux on the daemon's own machine | nothing (the default), or `backend = "local"` |
| `docker` | a container on the daemon's Docker host | `backend = "docker"` + `docker.image` |
| `ssh` | a remote host over SSH | `backend = "ssh"` + `ssh.host` |
| `hook` | wherever your provisioner scripts put it | `backend = "hook"` (see [Remote hooks](remote-hooks.md)) |

Select a backend per-repo in `.agent-factory/config.json`, or per-session with
`af sessions create --backend <name>` (the flag overrides the repo config).

---

## Docker backend

With `backend = "docker"`, a session runs entirely inside a container:

1. The daemon `docker run`s a container from your image, publishing an internal
   port on a random loopback host port.
2. It clones the repo (from the repo's `origin` remote) into `/workspace` in the
   container.
3. It copies the `af` binary into the container and starts an
   **`af agent-server`** there — a headless, single-workspace server over the
   same TLS + bearer-token HTTP/WS protocol the daemon speaks.
4. The daemon drives that in-container agent-server over `wss://127.0.0.1:<port>`.
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
docker backend, drives the in-container agent-server over `wss://` (input →
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
   public interface), behind the same TLS + bearer-token protocol.
4. It opens an **SSH local-forward tunnel** from a daemon-local loopback port to
   that remote port and drives the agent-server over `wss://127.0.0.1:<localport>`.
   Attach, preview, prompts, and the live terminal stream all flow through the one
   tunneled, authed connection (TLS + token still apply end to end inside the
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
`wss://` (input → stream echo → preview/snapshot/liveness), and asserts the remote
process is reaped + the session dir removed + the tunnel closed on kill. A second
case commits real work, **archives** it (branch pushed to `origin`, remote
sandbox reaped), then **restores** it (a fresh remote clones the branch back, the
commit is present, the session is drivable) — the identical push/pull-branch
flow, over ssh. It skips cleanly where Docker is unavailable.

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
