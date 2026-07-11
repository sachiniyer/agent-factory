# RFC: #1592 Phase 2 — af agent-server + uniform REST/WS client API

Status: **Proposed — awaiting root greenlight** · Author: Captain Claude ·
Epic: [#1592](https://github.com/sachiniyer/agent-factory/issues/1592) ·
Follows: Phase 1 (shipped v1.0.173) · Sets up: Phase 3 (TCP+auth), Phase 4
(docker + first-class SSH backends), Phase 5 (xterm.js web frontend)

> **Plan only.** No implementation lands from this document. It resolves the
> five open sub-questions the epic left for Phase 2, defines the agent-server +
> protocol at a design level, and decomposes the work into an independently
> shippable PR sequence in the Phase-1 mould (least-risky-first, behavior-
> preserving with a play-test guard per PR, clean-break with no backcompat).

---

## 0. Summary

Phase 1 made the `Backend` abstraction **total**: a capability descriptor, no
`Type()=="remote"` / `IsRemote()` special-casing, a provision/launch split in
`Start`, and a uniform `PTYStream` attach primitive (`session/ptystream.go`).
Remote (HookBackend) attach already routes through `PTYStream`; **local
full-screen attach was deliberately left on the tmux-server-mediated driver**,
to be moved when tmux goes behind the agent-server — i.e. now.

Phase 2 builds the OpenHands-shaped spine:

1. A small **af agent-server runs inside each sandbox** and speaks ONE
   REST + WebSocket protocol. Its job on the daemon side shrinks to
   **provision-and-expose**; the terminal mechanism (tmux) becomes an internal
   detail of the *local* runtime, no longer part of any interface a client
   touches.
2. The #1029 HTTP/JSON socket is **elevated to THE client API**: REST for the
   control plane (session/task RPC mirror, already live) + **WebSocket for the
   raw-PTY byte stream and live events**.
3. The **TUI becomes a thin client** of that API — it reads state over the
   socket and renders the PTY stream through `ui/termpane`, retiring the
   `net/rpc` TUI hot path and, crucially, the **`tmux attach-session` render
   client** that today backs each embedded live pane.

The protocol is **auth-ready from day one** (a no-op auth middleware seam over
the Phase-2 unix socket) so Phase 3 adds TCP + tokens/mTLS without reshaping a
single message. The daemon stays the #960 single writer; nothing here federates
or weakens that.

---

## 1. What Phase 1 left us (the seams Phase 2 builds on)

| Seam | Where | Phase-2 role |
|------|-------|--------------|
| Capability descriptor | `session/backend.go` `Capabilities{}` | Backend advertises what its agent-server exposes; unchanged shape |
| Provision/launch split | `Backend.Start` two internal phases (`backend_local.go`, `backend_hook_backend.go`) | Becomes provision-sandbox → start-agent-server → expose |
| `PTYStream` | `session/ptystream.go` (`io.ReadWriteCloser` + `Resize`) | The exact primitive the WS PTY endpoint reads/writes; transport swaps stdio→WS, primitive unchanged |
| #960 single-writer daemon | `daemon/` `Manager` + `controlServer` | Central orchestrator; sole state owner; hosts the agent-server-facing side and the client API |
| #1029 HTTP/JSON mirror | `daemon/httpserver.go`, `httproutes.go`, `apiproto` envelope | Control plane of the client API; already 1:1 with the RPCs, already `{data,error}` |
| #1160 pause-poll lease | `PauseStatusPoll`/`ResumeStatusPoll` RPC, `app/live_termpane.go interactivePollPauseCmd` | Generalizes into the attach lease (single interactive controller) |
| Hook attach via PTYStream | `runHookAttachWithDetach` → `driveHookPTYStream` | The proof that a backend's attach is just a PTYStream; local now joins it |

### 1.1 The load-bearing fact for this phase — today's live pane is a *second tmux client*

Each embedded live pane calls `termpane.New(sessionName, w, h)`
(`app/live_termpane.go:211`), which spawns a PTY running
`tmux attach-session -t =<sessionName>` (`ui/termpane/termpane.go:106-136`).
That is a **real, second client in tmux's client table** for the session. This
one fact is the root of both the reliability papercut (§6) and the reason
Phase 1 could not fold local attach onto `PTYStream` without risking the core
flow. Phase 2 deletes it.

---

## 2. Target architecture

Three orthogonal layers, unchanged from the epic; Phase 2 fills in (2)↔(1) and
rebuilds (3):

```
┌──────────────────────────────────────────────────────────────────┐
│ (3) CLIENTS — thin, over ONE API                                   │
│     TUI (ui/termpane)     CLI (af)      [Phase 5] web (xterm.js)    │
└───────────────▲───────────────────────────────▲───────────────────┘
                │ REST /v1/* (control)           │ WS /v1/.../stream (PTY+events)
┌───────────────┴───────────────────────────────┴───────────────────┐
│ (2) DAEMON — central orchestrator, #960 single writer              │
│     controlServer (RPC mirror)  +  WS PTY broker  +  event bus      │
│     auth middleware seam (no-op on unix socket, enforced Phase 3)   │
└───────────────▲────────────────────────────────────────────────────┘
                │ uniform agent-server protocol (in-process → loopback)
┌───────────────┴────────────────────────────────────────────────────┐
│ (1) BACKEND / RUNTIME — provision-and-expose                        │
│     local: agent-server wraps ONE tmux control channel (no client)  │
│     [Phase 4] container / ssh: agent-server owns a native PTY       │
└─────────────────────────────────────────────────────────────────────┘
```

The daemon never speaks tmux to a client and never assumes locality. It speaks
the uniform protocol to a per-session **agent-server**; for the local runtime
that agent-server is (initially) an in-process Go object wrapping tmux, later
reachable over loopback identically to a container/ssh one.

---

## 3. Resolved open questions

### Q1 — agent-server terminal transport: wrap tmux vs native PTY → **BOTH, chosen by runtime; local keeps tmux, behind a clientless control channel**

**Recommendation.** The agent-server exposes a single uniform surface
(`Subscribe`/`Input`/`Resize`/`Snapshot`); *how* it obtains the PTY bytes is a
per-runtime internal detail:

- **Local runtime** wraps the existing tmux session, but **as a control channel,
  not a second interactive client**: `pipe-pane` (output byte stream) +
  `send-keys`/`paste-buffer` (input) + `resize-window` (size), or one
  agent-server-*owned* attach if control-mode proves cleaner. The invariant is
  **≤1 tmux client, and WS subscribers are never tmux clients**. tmux is kept
  for the local runtime because it is what gives us crash/restart persistence
  and matches everything already hardened (#464/#598/#975/#1157). Phase 1
  deliberately deferred exactly this move.
- **Container / SSH runtimes** (Phase 4) have their agent-server **own a native
  PTY** directly (`creack/pty`) — no tmux in the image. The `PTYStream` seam
  (`session/ptystream.go`) already models this; `ptyFileStream` is the native
  case today.

**Rationale.** The uniform contract is the byte stream, not the mechanism.
Forcing tmux into a container would drag a dependency and a locality assumption
into every sandbox; forcing a native PTY onto local would throw away persistence
and re-litigate the hardened tmux detach machinery. Letting each runtime pick,
behind one `Subscribe`/`Input`/`Resize` surface, is the whole point of the
OpenHands split.

**Migration path.** local agent-server ships first (PR4–PR7) wrapping tmux;
the native-PTY runtime is proven later by Phase 4's container backend against
the *same* protocol, so no client changes when it arrives.

**Rejected.** (a) *tmux everywhere* — locality leak into remote sandboxes,
rejected by the epic. (b) *native PTY everywhere now* — discards tmux
persistence and forces a risky rewrite of local attach with no Phase-2 payoff.
(c) *put the mechanism back in the Backend interface* — the exact anti-pattern
Phase 1 removed.

### Q2 — backend extensibility: built-in Go vs hook-scripts vs both → **BOTH; the agent-server subsumes the *terminal*, not *provisioning***

**Recommendation.** Keep both mechanisms with a clean division of labour:

- **Built-in Go runtimes** (local today; container + ssh in Phase 4) implement
  provision-and-expose in-tree and run the real Go agent-server.
- **Hook-scripts stay the user extension point for *provisioning*** — a
  hook-backed runtime is one whose provision/launch/expose steps shell out to
  user scripts. What the agent-server model *removes* from hook-scripts is the
  bespoke **terminal/attach** contract (`attach_cmd`, `terminal_cmd`,
  `runHookAttachWithDetach`): once a hook backend exposes an agent-server URL,
  its terminal is the uniform WS stream like everyone else. Long-term a remote
  hook script's job is "provision a box + start `af agent-server` on it + print
  its authed URL," not "implement a terminal."

**Rationale.** Provisioning is inherently open-ended (every user's infra
differs) — that is what scripts are good at and must stay. Terminal mechanics
are not open-ended; they are exactly what we are unifying. So the agent-server
subsumes the terminal contract but *not* the provisioning contract.

**Phase-2 scope note.** Phase 2 adds **no new backend**. It builds the
agent-server for the **local** runtime only. Hook backends keep their current
attach path until Phase 4 migrates them onto an exposed agent-server URL. This
keeps Phase 2's blast radius on local UX (where we can play-test hard) and off
the remote path we cannot exercise live.

**Rejected.** (a) *Go-only, drop hook-scripts* — removes the only user
extension point on a public tool; violates "optimize for external users." (b)
*hook-scripts-only* — can't give container/ssh the first-class parity the epic
wants.

### Q3 — attach lease (generalize #1160): single interactive controller, others read-only → **daemon-granted per-session lease, carried in snapshot, acquired on WS connect**

**Recommendation.** Promote the #1160 pause-poll lease into a first-class
**attach lease** owned by the daemon (the single writer, so it is the natural
arbiter):

- A WS PTY connection opens with `mode=interactive` or `mode=observe`.
- The daemon grants the **interactive** lease to at most one connection per
  session; a second interactive request is **downgraded to observer** (receives
  the byte stream, its `Input`/`Resize` frames are rejected) — never blocked,
  never queued. Simplest, matches the epic's "you mostly attach through one
  client" decision.
- The holder **renews via WS keepalive** (the ping/pong that already detects
  half-open sockets, §6); the lease **releases on disconnect or renewal
  timeout**. No manual release RPC needed, though `mode=observe` self-demotion
  is allowed.
- **Lease state is part of the session snapshot** (`holder`, `mode`, since) so
  *every* client — TUI and future web — renders "someone else is driving this
  session" identically. This is what makes multi-client coherent.

The #1586 "defer automated task delivery while a human is attached" guarantee
and the #1160 capture-poll pause both **fold into "this session has an active
interactive lease"** — one concept, one place, instead of a title-keyed pause
map plus an implicit "close the render client before full-screen attach."

**Rationale.** Confirmed by the Phase-1 map: **there is no cross-client attach
arbitration today.** The #1160 lease is per-instance, best-effort, and
title-keyed; the only single-attach guard is client-side re-entry flags inside
the *one* TUI process (`m.attached`/`m.attachTransitioning`), and tmux itself
happily accepts multiple attach clients — so a second TUI or `af sessions
attach` could already double-attach. With a web client added this becomes a real
collision. Arbitration must be explicit, daemon-owned, and observable, and the
single writer is the only correct owner. The #1160 lease also already does
double duty (silence the poll *and* signal #1586 "defer prompts while attached")
— folding both into one daemon-owned lease removes that implicit coupling.

**Rejected.** (a) *queue/block second controller* — worse UX, needs timeouts and
fairness for no real gain. (b) *client-side arbitration* — races across clients
that can't see each other; only the single writer can be authoritative. (c)
*keep it title-keyed and best-effort* — doesn't extend to observers or surface
in the UI.

### Q4 — protocol shape → **REST control plane (elevate #1029) + WS binary PTY stream + WS/JSON event & lease frames; auth-ready middleware seam**

**Recommendation.** See §4 for the message-level design. Shape:

- **Control plane = REST**, the existing `POST /v1/<Method>` + `{data,error}`
  envelope, extended with lease read and stream-discovery routes. No renames of
  existing routes (they are a shipped contract).
- **Data plane = WebSocket**: one PTY stream endpoint per session using **binary
  frames** (opcode-prefixed) for PTY bytes/input/resize and JSON text frames for
  control/lease/exit events; a separate **events** WS for state-change fan-out
  (so clients can stop 100 ms Snapshot-polling — adopted opportunistically, see
  §7.2).
- **Auth-ready for the locked Bearer+TLS single-owner model** (§4.4): every REST
  and WS request passes through an auth-context middleware that in Phase 2 is a
  **no-op over the unix socket** (filesystem perms remain the auth, #1029's
  locked decision) and Phase 3 fills with a constant-time token compare. Auth
  rides transport (`Authorization: Bearer` + a `?access_token=` WS query fallback
  for browsers), never payloads, and the server is CORS-ready — so Phase 3/5 add
  nothing to any message shape.

**Rationale.** REST-for-control + WS-for-stream is the OpenHands shape and maps
cleanly onto what #1029 already is. Binary PTY frames avoid base64 bloat on the
hot path. Putting auth in a middleware seam (not the messages) is what lets
Phase 3 be additive.

**Rejected.** (a) *everything over WS* — loses the cache/curl-ability and the
byte-identical CLI parity #1029 bought. (b) *SSE for PTY* — unidirectional; PTY
needs client→server input. (c) *base64 PTY over JSON WS* — measurable overhead
on the interactive hot path. (d) *gRPC* — new dependency, no browser story for
Phase 5, discards the #1029 investment.

### Q5 — local-first migration: front the local runtime with an agent-server without regressing local UX → **in-process agent-server first, then loopback; PTY cutover is the last, most-guarded PR**

**Recommendation.** Two-step seam, so the protocol is exercised before the
transport and the scary attach cutover is isolated:

1. **In-process first (PR4).** Introduce an `AgentServer` interface; the local
   implementation is an in-process Go object wrapping today's tmux calls. The
   daemon's observation/preview/liveness/input paths call the agent-server
   methods (uniform types) instead of the tmux-shaped `Backend` methods —
   **same tmux underneath, behavior-preserving.** This proves the protocol
   surface with zero transport risk.
2. **Loopback + WS PTY (PR5–PR7).** Add the WS PTY broker (PR5), cut the TUI's
   embedded live panes onto it and delete the tmux render client (PR6), then
   move local full-screen attach onto the same WS `PTYStream` and delete the
   Phase-1-deferred tmux-mediated attach driver (PR7). The daemon↔local-agent-
   server hop stays in-process for Phase 2 (loopback unix socket is a
   transport swap under the identical interface, taken when Phase 4 needs an
   out-of-process agent-server).

The **seam where the daemon talks to a local agent-server** is a Go interface in
Phase 2 (in-process), designed so a `net`-address-backed implementation drops in
for Phase 4 with no caller change — exactly the in-process→loopback path the
epic names.

**Rationale.** The local UX (embedded interactive panes, full-screen attach,
detach hardening) is the highest-value thing we cannot regress. Exercising the
protocol in-process first means any bug found in PR4 is a logic bug, not a wire
bug; isolating the PTY cutover to PR6/PR7 means the one high-risk change has the
heaviest play-test guard and a clean revert.

**Rejected.** (a) *loopback socket from the start* — pays transport complexity
and a serialization tax before the protocol is even proven. (b) *big-bang
cutover* — un-play-testable, un-revertable; violates the phased mandate.

---

## 4. The uniform protocol (design level)

The control plane stays the #1029 REST mirror. Phase 2 adds a small resource
surface for streaming and leases, plus the WS data plane. This is a design
sketch, not OpenAPI (per the #1029 "no OpenAPI in v1" decision).

### 4.1 Control plane (REST, `{data,error}` envelope, unix socket)

Existing `/v1/*` routes are unchanged. New routes:

| Verb | Path | Purpose |
|------|------|---------|
| GET  | `/v1/sessions/{id}/stream-info` | Discover the WS PTY URL + current lease holder/mode for a session (control-plane view; the WS also pushes lease changes) |
| POST | `/v1/sessions/{id}/lease` | Explicit demote-to-observer / release (the common case is implicit via WS lifecycle, §3) |
| GET  | `/v1/events` | (WS, §4.3) state-change stream — advertised here for discovery |

`stream-info` returns the per-session PTY endpoint (`/v1/sessions/{id}/stream`)
rather than hard-coding it, so Phase 3/4 can hand back a *remote* authed URL for
an off-box agent-server without the client learning anything new — the OpenHands
"expose an authed URL" move.

### 4.2 Data plane — WS PTY stream: `GET /v1/sessions/{id}/stream?mode=interactive|observe&since=<seq>`

Upgrades to WebSocket. **Binary frames** carry the hot path; **text frames**
(JSON) carry control. First byte of a binary frame is an opcode:

Server → client:
- `0x00 PTY_OUT` — raw PTY output bytes (verbatim; the client emulates/renders,
  never a server-rendered grid — matches `ui/termpane` and xterm.js).
- text `{"type":"lease","holder":"<client>","mode":"interactive|observe"}` — lease
  changes (also sent once on connect).
- text `{"type":"resize","rows":R,"cols":C}` — authoritative size echo (so
  observers size their emulator to the holder's PTY).
- text `{"type":"exit","code":N}` — the agent/PTY ended.

Client → server (accepted only from the interactive lease holder; ignored+NACK'd
from observers):
- `0x01 INPUT` — raw key bytes → agent-server `Input`.
- `0x02 RESIZE` — `rows,cols` (uint16 pair) → agent-server `Resize`.
- text `{"type":"detach"}` — clean release (also implicit on socket close).

**Reconnect/replay.** The agent-server keeps a bounded per-session **output ring
buffer** with a monotonic sequence number. `?since=<seq>` on (re)connect replays
the tail so a reconnecting client repaints without a visible gap. No `since` =
replay the full ring (a fresh attach repaints current screen state).

**Keepalive.** WS ping/pong on a fixed interval detects half-open sockets; a
subscriber that misses pongs is dropped **without touching the PTY/session** (it
keeps running server-side; other subscribers are unaffected). This is the
structural difference from a tmux client (§6).

### 4.3 Events plane — `GET /v1/events` (WS, JSON)

A fan-out of session/task state changes (`session.created|updated|killed|
archived|restored|lease-changed`, `task.*`) carrying the same `InstanceData`
projection the Snapshot RPC returns. Lets a client replace the current 750 ms
Snapshot poll (`app/sync.go` `snapshotRefreshInterval`) with push. Phase 2
**designs and ships the endpoint** but TUI adoption is opportunistic (§7.2) —
the web client (Phase 5) is its first hard consumer.

### 4.4 Auth-readiness (Sachin: locked Phase-3 model = **Bearer token + TLS, single-owner**)

Sachin fixed the Phase-3 auth model, so Phase 2 designs the seam to match it
exactly and add **zero** message reshaping later:

- **Model: one bearer token = full access.** Single-owner. **No mTLS, no OIDC,
  no per-user identity** — deliberately out of scope. The token is what a
  *direct TCP* client and the *web frontend* present; loopback + SSH-tunnel stays
  the zero-config default (no token needed on the local unix socket).
- **REST + WS both pass through a `withAuth(next)` middleware** and carry an
  **auth-context** on every request and every WS handshake. Phase-2 impl:
  local-trust — the unix-socket peer is always authorized (unchanged #1029
  model), the middleware exists and threads the context but the token check is a
  **no-op**.
- **Auth material rides transport, not payloads.** REST: `Authorization: Bearer
  <token>`. WS: the same header **plus a `?access_token=<token>` query-param
  fallback**, because **browsers cannot set WS request headers** — the query
  param is the browser path and must be part of the design now, not retrofitted.
- **CORS-ready.** The HTTP server threads a CORS policy hook (permissive/no-op in
  Phase 2 on the unix socket) so the Phase-5 browser origin is an allow-list
  entry, not a re-architecture.
- Phase 3 = flip the listener to TCP + TLS, fill `withAuth` with the constant-
  time token compare, turn on CORS. **No message shape, route, or frame changes.**
  That is the acceptance bar for "auth-ready."

---

## 5. The agent-server interface (design level)

Introduced in PR4 as a Go interface the daemon depends on; the local impl wraps
tmux, the Phase-4 impls own a native PTY. Shape (illustrative, not final):

```
type AgentServer interface {
    // provision-and-expose (already the two Start halves from Phase 1 PR4)
    Provision(inst *Instance, firstTime bool) error
    Launch(inst *Instance) error
    Expose() (StreamEndpoint, error)   // local: in-process handle; Phase 4: authed URL

    // observation (non-interactive) — feeds snapshot/preview/liveness
    Snapshot() (Observation, error)    // preview text, hasPrompt, liveness, dims
    Preview(full bool) (string, error)

    // data plane
    Subscribe(mode LeaseMode, since Seq) (PTYSubscription, error) // fan-out read
    Input(b []byte) error              // lease-holder only
    Resize(rows, cols uint16) error    // lease-holder only

    // input helpers (already on Backend) route through the same channel
    SendPrompt(s string) error
    Kill() error
}
```

`PTYSubscription` is a `PTYStream`-shaped read side plus a sequence cursor. The
daemon's WS handler is a thin adapter: WS frame ⇄ `AgentServer` call. The
`Backend`'s tmux-shaped methods (`SendKeys`, `TapEnter`, `Preview`, …) become
**internal to the local agent-server**, no longer on any client path — the
locality leak the epic set out to remove, gone at the interface.

---

## 6. Reliability — WS PTY stream vs today's tmux render-client (**explicit design goal**)

### 6.1 Today's failure class (being deleted, not patched)

The live embedded pane is a **second `tmux attach-session` client**
(`ui/termpane/termpane.go:106-136`, spawned by `app/live_termpane.go:211`).
Observed ~9× over 3 days: `termpane: live client for <k> died after <N>s (pane
falls back to capture; rebind retries every 5s)` — self-recovering, not a crash,
but a real papercut (the live pane drops to the coarser capture render for up to
5 s). Root causes, all inherent to being a tmux client
(`app/live_termpane.go:116-146`):

1. **Size fight.** tmux sizes a session to its **smallest** attached client. The
   render client + any other client (full-screen attach, a second pane on the
   same session, an external `tmux attach`) negotiate size against each other.
   #598 forced us to *close the render client before any full-screen attach* to
   avoid shrinking/garbling the interactive client — a workaround, not a fix.
2. **detach-client reaping.** An external or implicit `tmux detach-client`
   (resize storms, tmux server churn, client reaping under contention) detaches
   the render client; its `Done()` fires → capture fallback → 5 s rebind loop.
3. **Client-table presence.** Because it *is* a client, anything that manipulates
   tmux clients can reap it out from under the TUI.

Phase 2 **deletes this mechanism** (PR6). No tactical patch — the class is
designed out.

### 6.2 Why the WS PTY stream structurally eliminates the class

- **Observers are not tmux clients.** The local agent-server holds **≤1** tmux
  connection (a clientless `pipe-pane`/`send-keys`/`resize-window` control
  channel, or one agent-server-owned attach) and **fans raw bytes to N WS
  subscribers**. Subscribers never appear in tmux's client table → cannot be
  `detach-client`'d, do not participate in size negotiation. Failure causes (1)
  and (3) become **impossible by construction**, not merely rarer.
- **Size is lease-owned, not negotiated.** Only the interactive lease holder's
  `RESIZE` frame sets the PTY size (§3, §4.2); observers render into whatever
  size the holder dictates. There is no smallest-client tug-of-war, and no
  separate tmux client for a full-screen attach to fight — full-screen attach
  becomes just the interactive lease holder on the same stream (§7, PR7). The
  #598 concern is designed away.
- **The stream survives client disconnects.** A dropped or half-open WS is
  detected by ping/pong and dropped **without touching the PTY/session**; the
  ring buffer retains scrollback; the client reconnects with `?since=<seq>` and
  **replays the gap**, repainting within one frame. That is invisible, versus
  today's visible fall-to-capture. There is no degraded capture mode to fall
  into — it is deleted (PR6/PR7).

### 6.3 Acceptance criterion (attached to PR6)

> Over an extended exploratory play-test — multiple visible panes on distinct
> sessions, aggressive resize storms, focus churn across tree/panes, a
> concurrent full-screen attach, and a **deliberately dropped WS mid-stream** —
> there are **zero** occurrences of the current "live client died → capture
> fallback" class, and any WS reconnect repaints from `?since` replay within one
> frame with no user-visible flicker. A targeted test drops the WS mid-stream
> and asserts seamless resubscribe+replay. The capture-pane live-fallback path
> is **removed** (there is no silent degraded mode left to enter).

---

## 7. TUI as a thin client — what actually changes

### 7.1 Control plane (PR3)
`app/session_control.go` + `app/handle_actions.go` + the `SnapshotWithAlarms`
read (`daemon/snapshot.go` client side) switch from the `net/rpc` control client
(`daemon/control_client.go`) to a typed **HTTP client** over `daemon-http.sock`
(PR2). Same daemon core, same envelope, same structs → behavior-preserving. The
`net/rpc` control socket **stays** for the CLI and daemon-internal lifecycle
(Shutdown/Ping) — it is *not* an "old path" being replaced for all clients in
Phase 2, so no clean-break deletion of it here (that is a Phase 3+ item once the
CLI also goes over the network API).

### 7.2 State push (opportunistic)
The `/v1/events` WS (§4.3) can replace the 100 ms Snapshot poll in the TUI. This
is a smoothness/efficiency win, **not required** for Phase 2's correctness, so it
is scoped as an optional follow-on within PR3's area — adopt if it lands cleanly,
otherwise keep polling and let Phase 5's web client be the first hard consumer.

### 7.3 Data plane (PR6, PR7)
`ui/termpane` gains a WS-backed byte source (consume `PTY_OUT` frames, emit
`INPUT`/`RESIZE`) alongside/replacing its tmux-attach source; `app/live_termpane.go`
binds panes to WS subscriptions instead of `tmux attach-session` children. Local
full-screen attach (PR7) drives the same `PTYStream` over WS as the interactive
lease holder.

### 7.4 Preview moves onto the wire (consequence surfaced by the Phase-1 map)

Today, **preview content never crosses the daemon wire** even though #960 made
the TUI a read-only projection for *state*: each client independently
materializes a full backend and captures preview **locally** — `tmux
capture-pane` for local sessions (`session/tmux/io.go`), and a **separate
long-running hook preview process** per client for remote sessions
(`backend_hook_pty.go` `ensurePTY` feeding a ring buffer). The Snapshot RPC
carries only `[]InstanceData` (status/liveness), not preview text.

Making the TUI a *true* thin client means preview comes from the daemon's
**observation channel**, not client-local capture:

- **Focused/visible pane** → the live WS PTY stream (§4.2) — already the plan.
- **Non-focused sidebar preview** → an `Observation.Preview` snapshot the daemon
  serves from the agent-server (PR4 owns the observation; delivered via a
  lightweight per-session preview read or piggybacked on the events/snapshot
  channel). The TUI **stops running `tmux capture-pane` and stops spawning its
  own hook preview process** — the daemon (single owner of the agent-server) is
  the sole capturer.

This removes the last big chunk of client-local session I/O and is what lets a
browser (which cannot run `tmux capture-pane`) show previews at all. Scope:
observation/preview delivery lands with the agent-server (PR4) and its WS/read
surface (PR5); TUI consumption (drop local capture) lands with PR6 next to the
live-pane cutover, since both delete client-local capture paths together.

---

## 8. PR sequence

Seven PRs, least-risky-first, each independently shippable and behavior-
preserving except the two explicit cutovers (PR6/PR7), each with a play-test
guard. `make tui-driver-selftest` (25/25) + `make test-container` are the
regression floor on **every** PR; the visible ones add exploratory play-testing.

| PR | Title | Serialize / parallel |
|----|-------|----------------------|
| 1 | protocol foundation: `agentproto` wire types + WS lib + frame codec | ∥ with PR2 |
| 2 | Go client SDK for the #1029 HTTP API (`apiclient`) | ∥ with PR1 |
| 3 | TUI control+read path → `apiclient` (retire `net/rpc` from the TUI) | ∥ with PR4 |
| 4 | `AgentServer` interface + local in-process impl (observation/input via uniform types) | ∥ with PR3 |
| 5 | WS PTY broker endpoint + local agent-server clientless tmux fan-out | after PR4 |
| 6 | embedded live panes consume WS stream; **delete tmux render-client** (reliability) | after PR5 |
| 7 | local full-screen attach → WS `PTYStream`; delete Phase-1-deferred tmux attach driver + capture fallback | after PR6 |

Dependency graph: `(PR1 ∥ PR2) → (PR3 ∥ PR4) → PR5 → PR6 → PR7`. Two lanes at
the front, two in the middle, then a forced serial tail because PR5→PR7 all
converge on the PTY streaming spine and the `ui/termpane`/`app/live_termpane.go`
files.

### PR1 — Protocol foundation
- **Scope.** New `agentproto` package: PTY frame opcodes + codec, lease/event/
  resize JSON message types; control-action types **reuse** the existing RPC
  request/response structs (no re-declaration → cannot drift). Add the WebSocket
  library dependency (recommend `github.com/coder/websocket` — context-aware,
  minimal, actively maintained; single vendored dep). Additive only; **nothing
  wires it yet.**
- **Files.** new `agentproto/`, `go.mod`/`go.sum`.
- **Risk.** ~zero (no behavior; compiles + marshal unit tests + a frame
  round-trip test).
- **Play-test.** None needed; selftest 25/25 as the floor.

### PR2 — HTTP API Go client
- **Scope.** A typed client that dials `daemon-http.sock` and calls `/v1/*`,
  returning the same structs the `net/rpc` client returns. First (safest)
  consumer: the read-only `af sessions list`/`get` path switches to it, proving
  parity against the disk-fallback. Everything else still on `net/rpc`.
- **Files.** new `apiclient/` (or `daemon/httpclient.go`), `api/*.go` read paths.
- **Risk.** low (read-only surface, byte-identical envelope, disk fallback
  unchanged).
- **Play-test.** `af sessions list --json` byte-parity vs `net/rpc`; selftest
  25/25.

### PR3 — TUI control+read path → HTTP client
- **Scope.** Switch `app/session_control.go`, `app/handle_actions.go`, and the
  `SnapshotWithAlarms` projection read from `net/rpc` → `apiclient`. TUI stops
  using the `net/rpc` client entirely (control socket stays for CLI/internal).
  Optionally adopt `/v1/events` push (§7.2) if clean.
- **Files.** `app/session_control.go`, `app/handle_actions.go`, `daemon/snapshot.go`
  (client side / accessors), `app/sync.go`.
- **Risk.** medium — TUI hot path — but reversible and the wire shape is
  identical.
- **Play-test.** Full selftest + **heavy exploratory**: create/select/kill/
  archive/restore/send-prompt/new-tab/close-tab, sidebar liveness, alarms — all
  behavior-identical; no orphans.

### PR4 — AgentServer interface + local in-process impl
- **Scope.** Define `AgentServer` (§5); local impl wraps today's tmux calls
  in-process. Route the daemon's snapshot/preview/liveness/prompt/keys paths
  through the agent-server methods (uniform types) instead of the tmux-shaped
  `Backend` methods. **Same tmux underneath → behavior-preserving.** Does not
  touch `app/`.
- **Files.** new `session/agentserver.go` + `session/agentserver_local.go`,
  `daemon/snapshot.go`/`daemon/delivery.go` server side, `session/backend_local.go`.
- **Risk.** medium (re-routes the observation path) but no wire, no UI.
- **Play-test.** Selftest + exploratory: preview text, liveness transitions,
  cron/watch prompt delivery, AutoYes tap-enter — all unchanged.

### PR5 — WS PTY broker + local clientless tmux fan-out
- **Scope.** Add `GET /v1/sessions/{id}/stream` (WS upgrade) + `stream-info` +
  lease routes; broker fans the local agent-server's output ring to N
  subscribers; input/resize from the interactive lease holder → agent-server;
  keepalive + `?since` replay. WS handshake threads the `withAuth` seam (§4.4:
  header + `?access_token=` fallback, no-op in Phase 2). Local agent-server
  switches its tmux binding to a **clientless** control channel
  (pipe-pane/send-keys/resize-window), and exposes the non-live **observation
  preview** read (§7.4) so a client can source sidebar preview from the daemon.
  **Not yet consumed by the TUI** (live panes still on the tmux render client),
  so off the UX hot path.
- **Files.** `daemon/httproutes.go`, `daemon/httpserver.go` (auth/CORS middleware
  seam), new `daemon/ws_pty.go` + `daemon/ws_events.go`,
  `session/agentserver_local.go`, lease state in `daemon/manager*.go`.
- **Risk.** medium — new endpoint + the clientless-tmux switch — but dark until
  PR6.
- **Play-test.** A WS integration harness (mould of `remote-roundtrip-container`):
  connect interactive + observer, receive bytes, send input, resize, drop+`since`-
  replay, lease grant/downgrade. Selftest floor.

### PR6 — Live panes on the WS stream; delete the tmux render-client (**reliability PR**)
- **Scope.** `ui/termpane` consumes the WS `PTY_OUT` stream and emits
  `INPUT`/`RESIZE`; `app/live_termpane.go` binds panes to WS subscriptions.
  **Delete** the `tmux attach-session` render client (`termpane.New(sessionName)`
  tmux path) and the capture-pane live-fallback + 5 s rebind loop. Acquire the
  interactive lease on focus (generalized #1160/#1586). Switch the sidebar
  preview to the daemon's observation snapshot (§7.4) and **delete the TUI's
  local `tmux capture-pane` path + its own hook preview process** — the daemon
  becomes the sole capturer.
- **Files.** `ui/termpane/termpane.go`, `app/live_termpane.go`, `app/home_panes.go`
  (drop local capture), `ui/tab_pane.go`/`ui/tabbed_window.go`, capture-fallback
  call sites.
- **Risk.** **HIGH** — the core embedded-terminal UX.
- **Play-test.** The §6.3 acceptance criterion (zero capture-fallback class,
  seamless drop+replay) + full exploratory: multi-pane type-in, resize, focus
  churn, interactive-mode invariant, no orphaned children, selection intact.

### PR7 — Local full-screen attach → WS PTYStream; clean-break deletions
- **Scope.** Move local full-screen attach (`app/home_attach.go`) onto the WS
  `PTYStream` as the interactive lease holder — retiring the **Phase-1-deferred**
  tmux-server-mediated local attach driver. Preserve the hardened detach
  semantics (#464/#598/#975/#1157) *as WS-level behavior* (detach frame,
  bounded drain, no size fight — now structurally absent). Delete the now-dead
  tmux attach driver code.
- **Files.** `app/home_attach.go`, `session/tmux/*` (dead attach driver removal),
  `ui/tabbed_window.go`.
- **Risk.** medium-high (hardened path) but PR6 already proved the WS stream
  under load; full-screen attach is the same stream in interactive mode.
- **Play-test.** Full-screen attach/detach ×N, reattach, resize under attach,
  Ctrl-] escape, no orphaned clients, `#598` size-integrity check; selftest.

---

## 9. Load-bearing / irreversible decisions to flag to Sachin

Root should surface these to Sachin before the corresponding PR merges — they
touch a public contract or a flow a user can depend on:

1. **The WS PTY + events protocol is a NEW public client-API contract** (Q4,
   PR1/PR5). It is what the Phase-5 web client and any third-party client will
   bind to. Frame opcodes, the lease model, and the `stream-info`-returns-a-URL
   indirection are hard to change post-release. **Decision to confirm:** binary
   opcode-framed PTY + JSON control, lease = single-interactive/others-observe,
   ring-buffer `?since` replay.
2. **Elevating `daemon-http.sock` to THE TUI transport** (Q4, PR3). Today the
   socket is documented as a CLI/automation mirror; making the TUI depend on it
   raises it to load-bearing. **Confirm:** TUI over HTTP+WS unix socket; `net/rpc`
   control socket retained for CLI/internal only (not deleted in Phase 2).
   Auth-readiness shape is **locked by Sachin** (Bearer token + TLS, single-owner;
   header + `?access_token=` WS fallback; CORS-ready; no-op in Phase 2) — not an
   open question, but the protocol bakes the seam in now (§4.4).
3. **Deleting the tmux render-client and the capture-pane live fallback** (PR6).
   This removes a currently-shipping (if papercut-y) code path with no fallback
   mode left. Gated on the §6.3 acceptance criterion. **Confirm:** clean-break,
   no dual-mode.
4. **Retiring the Phase-1-deferred local tmux attach driver** (PR7) — the
   hardened #464/#598/#975/#1157 path. **Confirm:** re-express its guarantees as
   WS behavior; delete the tmux driver.
5. **New dependency: a WebSocket library** (`coder/websocket` recommended, PR1).
   First non-stdlib network dep on the daemon's serving path. **Confirm the
   choice** (vs `gorilla/websocket`, vs hand-rolled RFC6455 — not recommended).
6. **Backend extensibility direction** (Q2): hook-scripts stay for *provisioning*
   but lose their bespoke *terminal* contract at Phase 4. This changes what a
   remote-hook author writes long-term. Phase 2 doesn't touch it, but the
   direction should be acknowledged now so Phase 4 isn't a surprise.
7. **Preview moves onto the daemon observation channel** (§7.4, PR6): the TUI
   stops running local `tmux capture-pane` and its own hook preview process, so a
   session's preview now requires the daemon (already true for state — the TUI is
   a projection). A user could notice preview no longer renders with the daemon
   down. **Confirm:** acceptable, consistent with #960; it is what makes a
   browser client able to show previews at all.

Not-flagged (ship without asking, every interpretation collapses): PR1/PR2/PR4
are additive/behavior-preserving internal scaffolding.

---

## 10. How Phase 2 sets up Phases 3–5 (no reshaping required)

- **Phase 3 (TCP + auth = Bearer token + TLS, single-owner — Sachin-locked).**
  The `withAuth` middleware seam (§4.4) plus transport-carried auth
  (`Authorization: Bearer` for REST, header **or `?access_token=` query** for WS)
  mean Phase 3 flips the listener to TCP + TLS, fills the middleware with a
  constant-time single-token compare, and turns on the CORS allow-list —
  **no message shape, route, or frame changes.** No mTLS/OIDC/per-user work.
  Loopback + SSH-tunnel remains the zero-config default; the token is for direct
  TCP and the web frontend. The `stream-info`-returns-a-URL indirection already
  lets the daemon hand back a `wss://` URL.
- **Phase 4 (docker + first-class SSH).** The `AgentServer` interface (§5) with
  its in-process→`net`-address transport swap (Q5) means a container/ssh
  agent-server that **owns a native PTY** (`ptyFileStream` already exists) plugs
  in behind the *same* protocol. Backends implement provision-and-expose;
  nothing above changes. Workspace=GitHub-clone (epic decision 4) is where
  archive/restore becomes push/pull — orthogonal to this phase.
- **Phase 5 (xterm.js web — will heavily mirror the TUI's layout/UX, built
  ~80% play-testing / 20% coding — Sachin).** The web client is a second consumer
  of the exact REST + WS API the TUI uses after PR3/PR6: `PTY_OUT` bytes →
  xterm.js write, keystrokes → `INPUT`, `RESIZE` on element resize, lease frames
  → "read-only" banner, `/v1/events` → live list, sidebar preview from the same
  observation channel (§7.4). Because it mirrors the TUI, **the TUI's move to the
  API in Phase 2 is what de-risks Phase 5** — the web client re-skins a
  contract already proven by the TUI, rather than inventing one. Every choice
  here is picked for browser-consumability: **JSON not gob** on the web-facing
  surface (#1029 is already HTTP/JSON — good), **binary WS frames** of raw bytes
  (not a server-rendered grid) so xterm.js is a drop-in, **WS token-in-query**
  (§4.4) because browsers can't set WS headers, and a **CORS**-ready server. If
  any of these were deferred, Phase 5 would force a protocol reshape — so they
  are in the Phase-2 design, not Phase 5's.

---

## 11. Risks & mitigations (summary)

| Risk | Mitigation |
|------|------------|
| PTY cutover regresses core local UX | In-process protocol first (PR4); cutover isolated to PR6/PR7 with the §6.3 acceptance criterion + heavy play-test; clean revert per PR |
| WS reconnect gaps / flicker | Ring-buffer `?since` replay + keepalive; targeted drop-mid-stream test |
| tmux clientless control channel misses escapes/redraws | Prove in PR5's WS harness before PR6 consumes it; keep the agent-server free to use one owned attach if pipe-pane proves lossy — the invariant is "observers aren't clients," not a specific tmux verb |
| New WS dependency surface | Single vendored lib, daemon-only, unix-socket-scoped in Phase 2 (no network exposure until Phase 3 auth lands) |
| Protocol churn after release | Flag as load-bearing (§9.1) for explicit sign-off before PR5 merges |
| Remote/hook path can't be play-tested live | Phase 2 leaves hook attach untouched (Q2 scope note); local-only blast radius; hook migration is Phase 4 |

---

_Authored by Captain Claude for #1592 Phase 2. Plan only — no implementation._
