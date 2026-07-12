# Phase 4 — real backends to parity: docker container + first-class SSH remote (#1592)

**Status:** design / review-only. PLAN ONLY — no implementation in this branch.
**Depends on:** Phase 1 (Backend abstraction total), Phase 2 (`AgentServer` seam +
uniform REST/WS + TUI-as-thin-client), Phase 3 (TLS TCP listener + bearer token +
`?access_token=` + `stream-info` URL indirection). All shipped.
**Sets up:** Phase 5 (web frontend) — it dials the exact same authed `wss://` a
container/remote session exposes.

This is the biggest phase of the epic. It turns the two "seams" Phases 2–3 built
(`AgentServer` interface, `StreamEndpoint.URL` → `stream-info` returns a URL,
token+TLS on TCP) into two **real, first-class backends running the agent +
workspace in their own sandbox**, each exposing an `af agent-server` behind an
authed URL — the OpenHands "provision-and-expose" model, at full parity with
local.

---

## 0. Where Phases 1–3 left the seam (what Phase 4 plugs into)

Phase 4 does **not** touch the daemon's observation/delivery path, the TUI, the
protocol, or the auth — those are done. It plugs into three existing seams:

1. **`session.AgentServer`** (`session/agentserver.go`) — the uniform contract the
   daemon speaks to a session's runtime. Today `Instance.AgentServer()` always
   returns `localAgentServer` (a thin in-process facade over tmux). The daemon's
   liveness/AutoYes/preview/prompt/stream paths depend ONLY on this interface.
2. **`StreamEndpoint{Local, URL}` + `stream-info`** (`daemon/ws_pty.go`
   `streamInfoHandler`) — the daemon already answers "where is this session's PTY
   stream reachable?" with either the local relative path *or a remote `wss://`
   URL*. The remote branch (`ep.URL != ""`) exists and is wired; **nothing
   produces a non-local URL yet**. Phase 4 is what fills it.
3. **Phase-3 auth material** — self-signed TLS cert (TOFU fingerprint), 32-byte
   bearer token, `?access_token=` WS fallback, CORS allow-list. The in-sandbox
   `af agent-server` reuses this verbatim to authenticate the daemon.

The whole point of Phases 2–3 was to make Phase 4 a matter of **(a) adding an
`af agent-server` process that runs in a sandbox and (b) adding daemon-side
`AgentServer` implementations that are HTTP/WS clients to it** — with the daemon
code above the interface unchanged.

---

## 1. Q1 — Agent-server-in-sandbox model (THE architecture decision)

**Recommendation: run a real `af agent-server` process INSIDE each sandbox; the
orchestrator daemon talks to it over the authed URL — NOT daemon-side driving via
`docker exec` / `ssh <cmd>`.**

This is the OpenHands ideal and the epic's locked decision #3 + "the key insight."
It is also what every seam above was built for. The alternative (daemon drives the
sandbox remotely — `docker exec`/`ssh` per action, agent-server stays daemon-side)
is rejected because:

- It **re-leaks locality** the epic spent Phase 1 removing: every action becomes a
  bespoke `docker exec`/`ssh` shell-out, re-introducing the exact
  transport-in-the-backend coupling Phase 2 deleted. The `AgentServer` interface
  becomes a lie (it would carry a "remote exec" shape, not a uniform protocol).
- It cannot stream a **raw PTY over WebSocket** cleanly — you'd be capturing a
  remote tmux over ssh again (the papercut Phase 2 killed). The in-sandbox server
  owns a native PTY and fans bytes over WS; that is the reliability win.
- Phase 5 (web) **requires** an authed URL the browser's xterm.js can dial. Only
  the in-sandbox model produces one. Daemon-side driving gives the web client
  nothing to connect to.

### 1.1 What "af agent-server" is (and is NOT)

The in-sandbox process is a **stripped, single-workspace** server — the OpenHands
"action-execution server", not the "app server". It serves the uniform REST+WS
`AgentServer` protocol for **exactly one session's workspace** and nothing else:

- **IS:** the per-tab PTY broker + ring buffer, `Subscribe`/`Input`/`Resize`,
  `Snapshot`/`Preview`/`Alive`, `SendPrompt`/`TapEnter`, the trust-prompt
  dismissal, tab management — all over the same routes the daemon mux already
  serves, behind the Phase-3 token+TLS gate. It drives the agent via a **native
  PTY manager** (see §1.2), not tmux.
- **IS NOT:** the orchestrator. No task scheduler, no watch supervisor, no
  multi-session `Manager`, no autostart unit, no disk state ownership. The
  orchestrator daemon stays the single writer (#960); the in-sandbox server is
  stateless-per-restart and owns only its one live PTY. Archive/restore durability
  is GitHub, not the sandbox (§5).

So the daemon-side view is symmetric: `localAgentServer` drives tmux in-process;
`remoteAgentServer` is an **HTTP/WS client** to an in-sandbox `af agent-server`.
Both satisfy `session.AgentServer`; the daemon above them does not change.

### 1.2 tmux vs native PTY inside the sandbox

The local runtime keeps tmux (it is on the daemon's own box; the clientless
capture works and is shipped). The **in-sandbox `af agent-server` uses a native Go
PTY** (`creack/pty`-style), not tmux — Phase 2 comment #1 already resolved this
("container/ssh own a native PTY"). Reasons: no tmux dependency to bake into every
image/host, one process to supervise, and the ring-buffer/fan-out broker is
transport-agnostic Go that carries straight over. tmux stays an internal detail of
the *local* runtime only.

> **Load-bearing / flag for Sachin:** the epic locks the OpenHands model, so this
> is a confirmation not a reversal — but it commits us to shipping/operating an
> `af agent-server` (+ its token+TLS) inside every container and on every remote
> host. Worth an explicit nod before we build 8 PRs on it.

---

## 2. Q2 — Backend extensibility & the fate of HookBackend

**Recommendation: add two first-class built-in Go runtimes (docker, ssh); keep
HookBackend as the generic "bring-your-own-provisioner" escape hatch, but migrate
it to provision-and-expose (its terminal/attach contract is deleted, clean-break).**

The epic already locked: "hook-scripts survive for provisioning; the terminal
contract they had is gone", and Phase 2 comment #2 locked "keep both — the
agent-server subsumes the *terminal* contract; hook-scripts stay the
*provisioning* extension point." Phase 4 executes that:

- **`docker` and `ssh` are the two first-class, opinionated built-ins** — the
  common cases (run in a container / ssh to a box) need zero user scripting.
- **HookBackend becomes the uncommon-case escape hatch** for infra we don't build
  in (k8s, Modal, Daytona, custom orchestration). But it is re-scoped to the same
  provision-and-expose contract: **`launch_cmd` must now return a
  `StreamEndpoint` (the authed `af agent-server` URL + token) instead of terminal
  metadata**, and `terminal_cmd` / `runHookAttachWithDetach` are **deleted**
  (Phase 2 already killed the terminal render-client; this removes the last hook
  attach proxy).

### 2.1 Migration for today's `Type()=="remote"` config users

This is a **breaking, clean-break change** (per the no-backcompat directive). A
`remote_hooks` user's `launch_cmd` today echoes a session id + preview/attach
metadata; after Phase 4 it must instead **start an `af agent-server` in the remote
workspace and echo its `{url, token, tls_fingerprint}`**. `terminal_cmd`,
`preview_cmd`, and the attach hook are removed from the config schema.

Because `af agent-server` (PR1) is a shipped subcommand, the migration for a hook
user is mechanical: their `launch_cmd` clones `repo@branch` and runs
`af agent-server --listen :PORT` (exactly what the docker/ssh runtimes do
internally), then echoes the URL. We ship a documented recipe.

> **Load-bearing / flag for Sachin:** (a) does HookBackend survive at all, or do
> docker+ssh cover enough that we delete it? (b) is the breaking config migration
> (`launch_cmd` return shape changes; `terminal_cmd` deleted) acceptable now?
> This removes behavior existing remote users depend on — squarely a Sachin call.
> Recommendation: **keep HookBackend, ship the breaking migration + recipe.**

---

## 3. Q3 — Docker container backend

A session's workspace + agent run in a container; the container exposes an
`af agent-server` on a published port; `stream-info` hands the daemon that URL.

### 3.1 Provisioning (docker CLI via `os/exec`, not the SDK)

Recommend driving docker through the CLI (`os/exec`, same idiom as HookBackend's
`exec.Command`) rather than the Docker Go SDK: no heavy dependency, trivially
testable/mockable (swap the `docker` path), and the surface we need is small
(`run`/`exec`/`stop`/`rm`/`cp`/`port`). Revisit the SDK only if CLI parsing gets
fragile.

Lifecycle → Runtime interface (§ shared with ssh, defined in PR3):

| Runtime step | Docker impl |
|---|---|
| `Provision(repo@branch)` | `docker run -d --label af.session=<id> -p 127.0.0.1::<agentport> <image>` → clone `repo@branch` from GitHub inside (git already in image), into `/workspace` |
| `StartAgentServer` | start `af agent-server --listen :<agentport> --token <t> --tls…` in the container (entrypoint or `docker exec`); read the published host port via `docker port`; return `wss://127.0.0.1:<hostport>` + token + TLS fingerprint |
| `Snapshot/Preview/Subscribe/Input/…` | (not the runtime's job) — the daemon-side `remoteAgentServer` HTTP/WS client hits the URL |
| `PushBranch` / archive | `docker exec … git push origin <branch>` then `docker stop`/`rm` |
| `Teardown` / kill | `docker rm -f` by `af.session` label |

### 3.2 Image

Ship (or document) an **`af-runtime` image**: `git` + the agent CLIs the user
needs (claude/codex/aider/gemini) + the `af` binary (for `af agent-server`). The
repo is cloned **per-session at `repo@branch`** (epic decision 4) into an
ephemeral container FS or a named volume — **workspace persistence is not
required** because GitHub is the durable store (archive = push branch, §5). So no
host-worktree bind-mount; the container is disposable.

> **Load-bearing / flag for Sachin:** image distribution is a product choice — do
> we build/publish an official `af-runtime` image (which registry, versioning
> tied to `af` version) vs bring-your-own image with the `af` binary
> mounted/`docker cp`'d in? Recommendation: **BYO-image + `docker cp` the running
> `af` binary in for v1** (zero registry/publishing burden, always version-matched
> to the daemon), document an example Dockerfile, revisit an official image later.

### 3.3 Config surface

New `backend: docker` selection (per-repo config, alongside today's
`remote_hooks`), with `docker.image`, optional `docker.run_args`. Selection flows
through the same `defaultBackendFactory` / `instance_data.go` factory that picks
local vs hook today. `ForceRemote` generalizes to a `--backend` create flag.

> **Load-bearing / flag for Sachin:** new canonical config keys
> (`backend`, `docker.*`, `ssh.*`) are a public contract → per CLAUDE.md
> ask-vs-ship this needs sign-off on the key names/shape before PR3 locks them.

---

## 4. Q4 — SSH remote-machine backend

First-class: ssh to a configured host, clone `repo@branch`, run `af agent-server`
there, expose its authed URL back to the daemon.

### 4.1 Provisioning (Go `x/crypto/ssh`)

Recommend the Go `golang.org/x/crypto/ssh` client (not shelling to the `ssh`
binary) so we own the tunnel and don't depend on the host's ssh config. Lifecycle
maps onto the SAME Runtime interface as docker:

| Runtime step | SSH impl |
|---|---|
| `Provision(repo@branch)` | dial host (key auth); `mkdir` a per-session dir; `git clone repo@branch` into it |
| `StartAgentServer` | ensure `af` on the host (BYO or `scp`/`sftp` the daemon's binary); start `af agent-server --listen 127.0.0.1:<port>`; open an **ssh local-forward tunnel** host:port → a daemon-local port; return `wss://127.0.0.1:<localport>` + token + fingerprint |
| `PushBranch` / archive | run `git push origin <branch>` over the ssh session, then stop the agent-server + close the tunnel |
| `Teardown` / kill | kill the `af agent-server` PID + `rm -rf` the session dir; close tunnel |

The SSH-tunnel local-forward reuses Phase 3's "loopback is the zero-config default"
posture: the daemon always dials `127.0.0.1:<localport>`, TLS+token still apply
end-to-end (defense in depth even inside the tunnel), and no port is exposed on the
remote host's public interface.

### 4.2 Relationship to HookBackend

The SSH runtime is **the built-in, opinionated version of what a hook `launch_cmd`
did by hand** (ssh in, clone, start a session). It covers the common case with
zero scripting. HookBackend remains for hosts/orchestration the built-in ssh
runtime can't model (bastions with exotic auth, k8s, serverless sandboxes).

---

## 5. Q5 — Capability parity & archive/restore

**Both new runtimes advertise full parity** — every `Capabilities` field true,
`Workspace: WorkspaceRemote`:

| Capability | docker | ssh | How |
|---|---|---|---|
| `Attach` / `InteractiveInput` | ✓ | ✓ | WS `Subscribe`/`Input`/`Resize` to the in-sandbox agent-server (multi-writer, no lease — matches local) |
| `TerminalTab` / `TabManagement` | ✓ | ✓ | the in-sandbox agent-server manages tabs natively (native PTY per tab); no per-config gating needed (unlike hook's `terminal_cmd`) |
| `Archive` | ✓ | ✓ | **push branch to GitHub, tear down sandbox** |
| `Recover` | ✓ | ✓ | **re-provision sandbox, clone `repo@branch`, restart `af agent-server`, resume agent** |

### 5.1 Archive/restore = push/pull the branch (epic decision 4)

Because the workspace is a GitHub clone of `repo@branch`, durability lives in
GitHub, not the sandbox:

- **Archive:** `git push origin <branch>` (from inside the sandbox), then
  `Teardown` the sandbox (docker `rm` / ssh session-dir cleanup). The instance
  record persists (branch name, backend, repo) — restorable.
- **Restore:** re-`Provision` a fresh sandbox, `git clone repo@branch` (now pulls
  the pushed state back), `StartAgentServer`, and resume the agent (the existing
  resume path — `claude --continue` / `codex resume`). No local worktree to
  relocate — this is *why* parity is achievable where the old HookBackend returned
  `ErrRecoverUnsupported`.

This makes `Archive`/`Recover` **identical logic across docker and ssh** (both go
through the Runtime's `PushBranch`/re-`Provision`), so it is written **once**
against the Runtime interface (PR6), not per-backend.

### 5.2 Parity/compliance test

Extend `session/backend_test.go`'s interface-compliance suite to run the full
capability matrix against the docker and ssh runtimes (gated on docker/ssh
availability). The end-state assertion from the epic: **every backend implements
every capability** — no `ErrRecoverUnsupported`, no locality special-case.

---

## 6. Q6 — Testing strategy (REAL round-trips)

Docker **is** available on this box (verified: Docker Engine 29.4 server running).
So both backends get real integration round-trips, not just mocks.

- **Unit / compliance (CI, host):** extend the `fake_backend` + compliance suite;
  a `fakeRuntime` exercises the Runtime interface without docker/ssh. Runs
  everywhere.
- **Docker round-trip (host, docker-available gate):** a `make backend-docker-roundtrip`
  target — create a session on the docker backend against a throwaway git repo,
  attach over the real WS stream, `SendPrompt`, `Preview`, add a tab, **archive
  (push branch) → restore (clone back) → kill**, asserting the container is gone.
  Uses the `af-runtime` image (or BYO with `docker cp`). Gated so CI without a
  docker daemon skips cleanly.
- **SSH round-trip (host, self-contained):** stand up a **throwaway sshd container
  as the ssh target** (`docker run` an image with sshd + git + `af`), point the
  ssh runtime at `127.0.0.1:<sshport>`, and run the identical round-trip. This
  gives a real ssh clone + tunnel + agent-server without any external host or the
  box's own sshd. A `make backend-ssh-roundtrip` target inside the existing
  container fence (see `docs/container-testing.md`).
- **Play-test (manual, agent-driven entropy):** create real docker/ssh sessions in
  the playtest sandbox, attach/type/resize/tab/archive/restore/kill with varied
  sequences — the standing manual play-test discipline, not automation.

Every PR keeps the shipped floor: build / gofmt / lint / deadcode / file-length +
`make test-container` + `tui-driver-selftest` 25/25, plus the real round-trip for
the runtime PRs.

---

## 7. PR sequence (least-risky-first, each shippable, clean-break)

The spine is deliberately front-loaded to **prove the wire protocol against a real
`af agent-server` over loopback before any container or ssh exists** — the single
biggest de-risk.

**PR1 — `af agent-server` headless mode *(dark; no runtime drives it yet)*.**
New subcommand serving the uniform REST+WS `AgentServer` protocol for exactly one
workspace (the daemon mux, minus orchestrator: no scheduler/task-store/Manager).
Native PTY manager (not tmux). Reuses Phase-3 token+TLS. Play-test: run
`af agent-server` by hand, curl REST + WS against it. **LOW–MED.**

**PR2 — daemon-side `remoteAgentServer` HTTP/WS client + per-runtime
`AgentServer` factory *(dark)*.** A full `session.AgentServer` implementation that
is an HTTP/WS client to an `af agent-server` URL (Snapshot/Preview/Subscribe/
Input/Resize/SendPrompt/… over REST+WS). Turn `Instance.AgentServer()` into a
factory (local-in-process vs remote-url). Unit-tested against **PR1's real
`af agent-server` over loopback**. **MED.**
> **Milestone (the de-risk):** PR1+PR2 = the daemon driving a *real out-of-process*
> `af agent-server` over loopback `wss://`, behaviorally indistinguishable from
> the local in-process path. The hard part — the protocol over the wire — is
> proven before docker/ssh add sandbox provisioning on top.

**PR3 — `Runtime` provision-and-expose interface + registry + config selection.**
Generalize the backend factory into a `Runtime` seam:
`Provision(repo@branch)` → `StartAgentServer` (→ authed URL) → `PushBranch` /
`Teardown`. Add `backend: local|docker|ssh|hook` config selection + `--backend`
create flag. Pure scaffolding + a `fakeRuntime`; no real sandbox yet. **LOW–MED.**

**PR4 — Docker runtime *(first-class, REAL container round-trip)*.** Implement
`Runtime` via the docker CLI: `run` → clone `repo@branch` → `StartAgentServer` →
publish port → `wss://` URL. Wire create/kill; capabilities parity (archive/restore
lands PR6). `make backend-docker-roundtrip`. The flagship. **MED–HIGH.**

**PR5 — SSH runtime *(first-class, REAL ssh round-trip via sshd container)*.**
Implement `Runtime` via `x/crypto/ssh`: dial → clone → `StartAgentServer` →
local-forward tunnel → `wss://` URL. `make backend-ssh-roundtrip` against a
throwaway sshd container. **MED–HIGH.**

**PR6 — Archive/restore = push/pull branch (parity for docker + ssh).** Wire
`Archive`/`Recover` once against the `Runtime` interface: archive pushes the
branch + tears down; restore re-provisions + clones + restarts agent-server +
resumes. Extend the compliance/parity matrix across all runtimes. **MED–HIGH.**

**PR7 — HookBackend → provision-and-expose migration *(breaking, clean-break)*.**
`launch_cmd` returns a `StreamEndpoint` (af agent-server URL+token); delete
`terminal_cmd` / `preview_cmd` / `runHookAttachWithDetach`. Migrate the
`Type()=="remote"` config + docs; ship the migration recipe. **MED.**
> Gated on the Sachin decisions in §2.1 (hook survival + breaking migration).

**PR8 — Parity compliance test + docs + capability audit.** Extend
`backend_test.go` to run the full compliance suite against docker + ssh
(availability-gated); assert every backend implements every capability (no
`ErrRecoverUnsupported`, no locality branch). Ship `docs/backends.md` (docker + ssh
setup, migration guide). **LOW–MED.**

Ordering rationale: PR1–PR2 prove the protocol with zero sandbox risk; PR3 is
scaffolding; PR4/PR5 add the two real runtimes independently (either can ship
first once PR3 lands); PR6 adds durability once both exist so it's written once;
PR7 is the one breaking/product-shaping change, isolated and last-but-one; PR8
locks parity + docs.

---

## 8. How Phase 4 sets up Phase 5 (web)

Phase 5's web client is a thin xterm.js + REST/WS app. Phase 4 hands it everything:

- **The authed `wss://` URL.** A container/remote session's `stream-info` returns
  its own `wss://…?access_token=…` URL — exactly what a browser's xterm.js dials.
  The `StreamEndpoint.URL` indirection means the web client dials whatever it's
  handed without knowing which runtime backs the session (same as the TUI).
- **The auth is already browser-shaped** (Phase 3): `?access_token=` WS fallback +
  `cors_allowed_origins` allow-list. No new auth work for web.
- **The `af agent-server` protocol is the whole contract** — web and TUI are both
  thin clients of the same REST+WS, so they can't diverge.

---

## 9. Load-bearing / irreversible decisions — flag for root → Sachin

| # | Decision | Recommendation | Why it needs sign-off |
|---|---|---|---|
| Q1 | agent-server INSIDE sandbox (real `af agent-server` process) vs daemon-side driving | **inside** (epic-locked; confirm) | commits us to running/operating an `af agent-server`+token+TLS in every container/host; 8 PRs ride on it |
| Q2 | HookBackend fate + breaking `remote_hooks` migration (`launch_cmd` returns a URL, `terminal_cmd` deleted) | **keep HookBackend as escape hatch; ship breaking migration + recipe** | removes behavior existing remote users depend on — a product/removal call |
| Q3 | docker image distribution (official published image vs BYO + `docker cp` the binary) | **BYO + `docker cp` for v1**, document a Dockerfile | registry/publishing/versioning is a product+distribution choice |
| Config | new canonical keys `backend`, `docker.*`, `ssh.*` | names/shape as sketched | adding to default config keys is a public contract (CLAUDE.md ask-vs-ship) |

Everything else (native-PTY-in-sandbox, CLI-vs-SDK for docker, `x/crypto/ssh` for
ssh, archive=push-branch, the PR ordering, the test strategy) is an internal
engineering call within the locked epic and does not need sign-off.
