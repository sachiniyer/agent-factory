# Backends (runtimes)

A session's **backend** decides *where* its workspace and agent run. Every
backend exposes the exact same session surface — attach, preview, prompt
delivery, the live PTY stream, tabs — so the TUI, CLI, and daemon drive a
containerised session identically to a local one.

| Backend | Where the agent runs | Selected with |
|---------|----------------------|---------------|
| `local` (default) | a git worktree + tmux on the daemon's own machine | nothing (the default), or `backend = "local"` |
| `docker` | a container on the daemon's Docker host | `backend = "docker"` + `docker.image` |
| `ssh` | a remote host over SSH | `backend = "ssh"` *(not yet implemented — #1592 Phase 4 PR5)* |
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
clone of `repo@origin`), not in the container filesystem. Archive/restore
(push the branch, re-provision on restore) lands in a later PR.

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

`make backend-docker-roundtrip` runs the real end-to-end container round-trip on
a host with Docker: it builds a slim git+tmux image, creates a session on the
docker backend, drives the in-container agent-server over `wss://` (input →
stream echo → preview/snapshot/liveness), and asserts the container is reaped on
kill. It skips cleanly where Docker is unavailable.
