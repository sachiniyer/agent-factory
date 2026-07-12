# Phase 5 — the browser web frontend (#1592)

**Status:** design / review-only. PLAN ONLY — no implementation in this branch.
**Depends on:** Phase 2 (`AgentServer` seam + uniform REST/WS + TUI-as-thin-client),
Phase 3 (TLS TCP listener + bearer token + `?access_token=` + CORS allow-list),
Phase 4 (every backend provision-and-expose over `af agent-server`, so a session's
terminal is already an authed `wss://` endpoint). All shipped.
**Sets up:** nothing — this is the last phase of the epic. It turns the REST+WS
contract Phases 2–4 hardened into a second thin client that happens to render in a
browser instead of a terminal.

The thesis of this phase is small on purpose: **the web client is the TUI, re-skinned
for the browser.** The daemon API is the contract; the TUI is one thin client of it;
the web app is a second thin client of the *same* contract. There is no new server
behavior, no new protocol, no new auth. Every hard problem (single-writer state,
authed remote transport, raw-PTY-over-WS, multi-writer resize, `?since` replay) was
solved in Phases 1–4. What is left is a rendering client — and per Sachin's directive,
**making it look nice**, which is a play-testing problem, not an engineering one.

> **Sachin's directive (locked, baked into this plan):** *"base the website pretty
> heavily on the TUI + do a bunch of playtesting to make sure it looks nice + is
> clean. 20% building, 80% playing/testing."* So this doc treats play-testing as the
> primary activity. Every build PR ships **with** a play-test protocol; a dedicated
> browser play-test harness lands **before** the second feature PR; and the
> FLAG-FOR-SACHIN section is dominated by *visual/UX gut-checks he must eyeball on a
> running deploy*, not architecture.

---

## 0. Where Phases 1–4 left the seam (what Phase 5 plugs into)

Phase 5 touches **no daemon behavior, no protocol, no auth**. It plugs into five
existing seams, all shipped:

1. **The daemon HTTP mux is already the whole API** (`daemon/httpserver.go:150-169`).
   Every session/task RPC is a POST route in the `af api` catalog
   (`docs/reference/api.md`); the WS data plane (`GET /v1/sessions/{id}/stream`,
   `…/stream-info`, `/v1/events`) rides the same mux. The mux has a catch-all `/`
   handler that today just 404s (`daemon/httpserver.go:167`) — **that is exactly the
   mount point for the SPA.**

2. **The TLS TCP listener + bearer token** (Phase 3, `docs/remote-tcp-auth.md`). The
   listener is off by default; when enabled it serves the *same mux* over TLS gated by
   a bearer token, with a `?access_token=` query fallback for browsers
   (`agentproto/auth.go:23`, `daemon/httpauth.go`) and an exact-match
   `cors_allowed_origins` allow-list. This is the browser transport, already built and
   documented as "what it's for."

3. **The WS PTY protocol** (`agentproto/frame.go`, `session/ptybroker.go`,
   `daemon/ws_pty.go`) — raw PTY bytes, opcode-framed binary, multi-writer INPUT/RESIZE
   (last-resize-wins), `?since=<seq>` replay, WS keepalive ping. This is precisely what
   xterm.js consumes. §4 covers the one real gap.

4. **The events plane** (`GET /v1/events`, `daemon/ws_events.go`, `agentproto`
   `EventType`) — a WS/JSON fan-out of `session.created/updated/killed/archived/restored`
   and `task.*`. The TUI uses it to replace Snapshot polling with push; the web sidebar
   uses it identically for live status dots.

5. **The session projection** (`session.InstanceData`, `session/storage.go:12`), returned
   by `Snapshot`/`SnapshotWithAlarms` (`daemon/snapshot.go:29`). It already carries every
   field the sidebar renders: `Liveness` (#1195), `InFlightOp`, `LimitResetAt` (the
   `[limit]` badge), `Tabs`, `PRInfo`, `Program`, `BackendType`. The web sidebar mirrors
   this projection exactly as the TUI's `ui/sidebar_*` does.

The whole point of Phases 2–4 was to make Phase 5 a matter of **(a) serving a static
bundle from the catch-all route and (b) writing a browser client of the same REST+WS
the `apiclient` Go package already speaks** — with the daemon unchanged.

---

## 1. Q1 — Serving model: how does the browser get the app?

**Recommendation: the daemon serves a single self-contained SPA (xterm.js + app JS/CSS
bundled via `go:embed`) from the existing catch-all `/` route on the *already-existing*
TLS TCP listener, reusing the Phase-3 token+TLS auth verbatim. No new listener, no new
port, no external assets.**

### 1.1 Where it mounts and the URL

The SPA mounts on the same mux the API already serves (`daemon/httpserver.go`), replacing
the catch-all `/` 404 with a static file server over a `go:embed`'d asset FS. Result:

- **API + WS + web are one origin, one port.** `https://host:8443/v1/...` is the API;
  `https://host:8443/` is the app; `wss://host:8443/v1/sessions/{id}/stream` is the PTY.
  Same-origin means the browser needs **no** CORS for the common "serve the web client
  from the daemon" case — `cors_allowed_origins` only matters if someone hosts the SPA on
  a *different* origin (a deploy we don't recommend for v1).
- **The web app is only reachable when the operator enables the TCP listener**
  (`listen_addr`, off by default). This is correct: no web surface appears unless the
  operator opts in, and the same TLS+token gate that protects the API protects the app.
  The local Unix socket is untouched and never serves the SPA (no browser can reach it).
- **Route precedence is safe:** Go's `ServeMux` longest-prefix match means every real
  `/v1/...` route wins; only genuinely unknown paths fall to the SPA handler, which serves
  `index.html` for any non-asset path (SPA client-side routing) and the embedded asset for
  asset paths.

### 1.2 How the token reaches the browser (the one genuinely new UX)

The API is bearer-token auth; a browser starts with no token. So the **only** un-authed
surface is the static shell:

- **Serve `index.html` + the JS/CSS bundle WITHOUT a token** (static assets leak nothing —
  they're the same bytes for every user). **Gate every `/v1/...` route and the WS upgrade
  with the token exactly as today.** This is the standard "public shell, authed API"
  split and requires *no* change to the auth middleware — it already only guards `/v1/…`
  paths; we simply let `/` and the asset paths through.
- **Login = paste the token.** First load shows a login view: a single field for the
  bearer token (the operator gets it from `af token show` on the host — the exact channel
  Phase 3 documents). On submit, the app does a probe request (`GET /v1/health` is public;
  use `POST /v1/Snapshot` as the auth check) and, on 200, keeps the token.
- **Token storage: `sessionStorage`, not `localStorage`.** It survives reload within the
  tab but not tab-close — a deliberate "don't persist a full-access credential to disk"
  posture matching the "treat it like a password" security note in
  `docs/remote-tcp-auth.md`. (FLAG: persistence choice is a security-taste call — §Flags.)
- **The token flows two ways, both already supported:** the `Authorization: Bearer`
  header on `fetch` for REST, and the `?access_token=` query param on the WS URL
  (`agentproto/auth.go:23`) because the browser `WebSocket` constructor cannot set headers.
  Both paths already exist server-side (`daemon/httpauth.go`); the browser just uses them.

### 1.3 Self-contained bundle (CSP-safe, no CDN)

Everything ships inside the `af` binary via `go:embed`: xterm.js + its addons, the app JS,
the CSS, fonts. **No external CDN, no runtime network fetch to any third-party origin** —
required both for the air-gapped/remote-host deploys this epic targets and to keep a strict
`Content-Security-Policy` (`default-src 'self'`) honest. The build step (§3.3) produces a
`dist/` the Go embed pulls in; a committed pre-built bundle keeps `go build ./...` working
with no Node toolchain on the build box (§3.3, §Flags).

> **Load-bearing / flag for Sachin:** serving a web UI is a new user-facing product
> surface on the daemon. It's gated behind the opt-in TCP listener (no new default
> exposure), but "the daemon now serves a website" is a positioning decision worth an
> explicit nod. Recommendation: **ship it embedded, opt-in, same-origin, documented as
> `docs/web.md`.**

---

## 2. Q2 — UI surface: mirror the TUI

**Recommendation: a 1:1 structural mirror of the TUI, sliced so v1 is minimal-but-complete
(list → attach → type → create/kill) and everything else is additive.** The TUI is the
spec; each TUI element maps to a web element, and the same daemon projection drives both.

### 2.1 TUI → web element map

| TUI element (source) | Web element | Data source |
|---|---|---|
| Sidebar session list + status dots + liveness (`ui/sidebar_render.go`, `sidebar_model.go`; dot logic `ui/tree/render.go`) | Left rail: session rows with a status dot | `Snapshot` `InstanceData.{Liveness,InFlightOp}` + `/v1/events` push |
| `[limit]` badge / "resets <t>" (`InstanceData.LimitResetAt`) | Badge on the row | same projection |
| Attached terminal / preview pane (`ui/termpane`, `ui/preview*.go`) | xterm.js canvas | `wss://…/stream` (§4) |
| Tabbed window / ephemeral tabs (#930, `ui/tabbed_window.go`, `tab_pane.go`, `InstanceData.Tabs`) | Tab bar above the terminal | `Tabs` + `CreateTab`/`CloseTab`; per-tab stream by tab index |
| New-session overlay (`ui/overlay/textOverlay.go`, project picker `projectPickerOverlay.go`) | New-session modal | `CreateSession` |
| Prompt / send-prompt input | Prompt modal / inline input | `SendPrompt` / `DeliverPrompt` |
| Confirmation overlay (kill/archive) (`ui/overlay/confirmationOverlay.go`) | Confirm modal | `KillSession`/`ArchiveSession` |
| Projects pane (`ui/projects.go`) | Repo grouping / project switcher in the rail | `Snapshot` grouped by `RepoID` |
| Status bar / keybinding hints (`ui/statusbar.go`) | Bottom bar with the same hints | static + selection state |
| Help overlay (keys in `keys/`) | Help modal listing the same shortcuts | static |
| Tasks / automations pane (`ui/task_pane.go`, `automations.go`) | Tasks view | `ListTasks`/`AddTask`/`UpdateTask`/`RemoveTask`/`TriggerTask` |
| Delivery-alarm banner (`ui/alarm.go`, `SnapshotResponse.DeliveryAlarms`) | Top banner | `SnapshotWithAlarms` |

### 2.2 v1 slice vs deferred

The **v1 slice is the minimal complete loop**: *see your sessions, attach to one, type
into it, create a new one, kill/archive one.* Concretely:

- **v1 (minimal-but-complete):** sidebar (list + status dots + liveness, live via
  `/v1/events`), attach terminal (xterm.js over the WS PTY, multi-writer input + resize),
  new-session modal, send-prompt, kill + archive with confirm, the login view. This is a
  usable product on its own and exercises every hard seam once.
- **Additive, deferred to later PRs:** multi-tab UI (#930 tabs) beyond the agent tab,
  projects/repo grouping, tasks/automations pane, delivery-alarm banner, restore-archived
  flow, `[limit]` badge + auto-resume surfacing, help overlay, search/filter, PR-info
  display. Each is a small additive PR with its own play-test, none blocks v1.

The ordering discipline: **v1 proves the whole thin-client architecture end-to-end
(auth → list → attach → type → mutate) before we breadth-expand.** Everything after v1 is
"add one more TUI element," low-risk and independently shippable.

---

## 3. Q3 — Client architecture: framework or vanilla?

**Recommendation: the lightest thing that mirrors the TUI cleanly — vanilla TypeScript +
xterm.js + a tiny hand-rolled state store over the existing REST/WS API. NO heavy SPA
framework (React/Vue/Svelte) in v1.** The API is the contract; the web is a thin
projection of daemon state exactly like the read-only TUI (#960). A framework buys
component ergonomics we don't need for a sidebar + a terminal + a few modals, and costs
bundle size (which we `go:embed` into every `af` binary), a build-toolchain dependency,
and a second idiom to keep honest against the TUI.

### 3.1 Shape

- **TypeScript, bundled with a small zero-config bundler (esbuild).** esbuild is a single
  Go binary (fits our world), builds the whole app in milliseconds, and needs no framework
  runtime. TS gives us types for the wire shapes so the client can't silently drift from
  the API.
- **State = one observable store** mirroring `SnapshotResponse` — a plain object updated by
  (a) an initial `Snapshot` fetch and (b) `/v1/events` push, with a handful of subscriber
  callbacks that re-render the affected DOM. This is the browser analogue of the TUI's
  read-only projection: the daemon is the single writer; the store is a pure mirror; user
  actions are RPCs whose effects arrive back as events. No client-side source of truth.
- **Rendering = direct DOM (or lit-html-style tagged templates if we want ergonomics
  without a framework runtime).** The surface is small and mostly a list + a terminal.
- **xterm.js + `@xterm/addon-fit`** for the terminal and resize.

### 3.2 Reuse the wire shapes — do NOT fork the protocol

The client mirrors, but must not re-invent, the existing shapes:

- **Envelope:** every REST call returns `{data, error}` (`apiproto/envelope.go`); one
  `af<T>(method, body): Promise<T>` helper unwraps it and throws on `error !== null`,
  giving the whole client uniform error handling for free.
- **Frames:** the WS PTY opcodes (`agentproto/frame.go`) are re-declared as a tiny TS
  encoder/decoder that is a **line-for-line port** of `Frame.Encode`/`DecodeFrame` — same
  opcode bytes, same big-endian resize layout. This is ~40 lines and is the one place the
  browser reimplements Go, so it gets a golden-vector test (§5) shared with the Go
  `frame_test.go` fixtures to guarantee they can't diverge.
- **Session/event DTOs:** TS `interface`s generated from / kept in lockstep with
  `session.InstanceData` and the `EventType` set. (FLAG: hand-mirror vs codegen — §Flags;
  recommendation: hand-mirror a small `types.ts` in v1, revisit codegen if it drifts.)

The invariant, stated plainly: **web and TUI are both thin clients of the same REST+WS, so
they cannot diverge in behavior — only in pixels.** That is the whole reason this phase is
cheap.

### 3.3 The reference implementation already exists in Go

The web client is not designing anything new — it is **porting `app/live_stream.go` +
`ui/termpane` to TypeScript.** `apiStream` (`app/live_stream.go`) already translates
`OpPTYOut→data`, `OpRepaint→repaint`, `MsgResize→resize` and sends `InputFrame`/`ResizeFrame`;
`ui/termpane` already owns the reconnect-with-`?since`, the `OpRepaint`-doesn't-advance-the-
cursor rule, and the backoff loop. The browser's terminal layer is a line-by-line port of
these two files onto xterm.js — which is why Q4 has exactly one genuine unknown (§4.3) and
everything else is mechanical. Likewise `apiclient/` is the reference for the REST layer:
the TS `af<T>()` helper mirrors `apiclient.call`, and the method list mirrors
`apiclient/control.go`+`sessions.go` one-to-one.

---

## 4. Q4 — The WS PTY in the browser (xterm.js over the binary protocol)

**Recommendation: xterm.js writes `OpPTYOut`/`OpRepaint` payloads straight to the
terminal; `onData` → `OpInput` frames; `FitAddon` → `OpResize` frames; reconnect with
`?since=<cursor>`; the WS keepalive is transparent. One real gap exists (§4.3) and needs a
tiny server-or-client accommodation.**

### 4.1 The mapping (all already emitted by the broker)

| Direction | Broker frame (`agentproto/frame.go`) | Browser handling |
|---|---|---|
| server→client | `OpPTYOut` (0x00) raw PTY bytes | `term.write(payload)` |
| server→client | `OpRepaint` (0x03) one-shot fresh-subscriber repaint | `term.write(payload)` **but do NOT advance the replay cursor** (§4.3) |
| client→server | `OpInput` (0x01) raw key bytes | `term.onData(d => ws.send(InputFrame(d)))` |
| client→server | `OpResize` (0x02) rows,cols uint16 pair | `FitAddon.fit()` → `ws.send(ResizeFrame(rows,cols))` |
| server→client | `MsgResize` JSON text frame (authoritative echo) | reflow: `term.resize(cols, rows)` to the server's last-resize-wins size |
| server→client | `MsgExit` JSON text frame | show "agent exited (code N)"; stop the terminal |

The multi-writer model is a browser non-issue: the browser is just another equal
read-write subscriber. The only cross-client conflict — size — is resolved by the server's
`MsgResize` echo, which the browser obeys by resizing xterm.js to match (so two clients of
different sizes converge on last-resize-wins, exactly as two TUIs do).

### 4.2 Reconnect / replay

The stream is bounded-ring + `?since=<seq>` replay (`daemon/ws_pty.go`,
`session/ptybroker.go`). The client tracks its absolute cursor as
`startSeq + bytesReceivedFromPTYOut` and, on socket drop, reconnects to
`…/stream?since=<cursor>` to replay exactly the gap — no full re-render, no lost bytes.
`OpRepaint` bytes are rendered but **excluded** from the byte count (they are
per-subscriber, not part of the shared ring's monotonic seq — counting them desyncs the
`?since` arithmetic; the opcode doc in `frame.go` is explicit about this).

### 4.3 The one real gap: the browser can't read `X-Af-Stream-Seq`

The broker returns the subscription's **starting seq** in the `X-Af-Stream-Seq` handshake
response header (`daemon/ws_pty.go:48,102`) so a client can seed its absolute cursor. **The
browser `WebSocket` API does not expose 101-handshake response headers** — so a browser
client cannot read `startSeq` the way the Go `apiclient` does. Without it the first-connect
cursor is unknown and the first reconnect can't compute a correct `?since`.

Three ways to close it, in order of preference:

1. **(Recommended) Server also emits the start seq as the first WS control text frame** —
   a new `MsgHello {type:"hello", seq:<n>}` sent immediately on `Accept`, before any
   PTY_OUT. This is a tiny additive `agentproto` message (mirrors `MsgResize`), costs the
   TUI nothing (it already has the header; it can ignore the frame or adopt it), and gives
   the browser a first-class cursor seed. It is the clean fix and the only server-side
   change this whole phase needs. (FLAG: this is a protocol addition — §Flags.)
2. Client seeds `startSeq` from a `GET …/stream-info` companion call that returns the
   current head seq. Extra round-trip; racy against bytes arriving between the two calls.
3. Client accepts a bounded gap: on reconnect, `?since=0` for a full ring replay (correct
   output, some duplicate render) or `?since=<local byte count>` (approximate). Cheapest,
   least correct.

Recommendation: **PR-scope option 1** — one additive control frame — and note it as the
sole protocol touch in an otherwise zero-server-change phase. Everything else
(binary framing, resize, keepalive, replay) the browser consumes as-is.

### 4.4 Keepalive

The broker pings each subscriber every `wsKeepaliveInterval` (15s,
`daemon/ws_pty.go:43`) and drops on a missed pong. Browsers answer WS pings at the protocol
layer automatically — **no client code needed.** The client only needs its own reconnect
loop for when the socket actually drops (network blip, daemon restart), driven by §4.2.

---

## 5. Q5 — Play-test strategy (THE 80%)

Per the directive, testing is the main event. Two complementary tracks, both against a
**containerized throwaway daemon + mock repo** (never this repo, never a real AF home —
the play-test isolation discipline from the 2026-07-03 outage), plus the standard
CI floor.

### 5.1 Track A — headless-browser regression harness (Playwright)

A **Playwright** harness that drives a real Chromium against a throwaway daemon, mirroring
the `tui-driver-selftest` as the web's regression baseline. It is the browser analogue of
the 25/25 TUI selftest: a fixed, scripted sequence asserting the core loop end-to-end.

- **Boot:** inside the container fence (`docs/container-testing.md`), start a daemon with
  the TCP listener on loopback + a known token against a mock git repo, build the SPA,
  point Chromium at `https://127.0.0.1:<port>/` with the self-signed cert accepted (pin or
  `--ignore-certificate-errors` inside the sealed container only).
- **The scripted baseline (`web-driver-selftest`):** login with the token → sidebar lists
  the seeded session → attach → terminal shows agent output → type a command → assert the
  echoed bytes render → resize the window → assert reflow → create a session via the modal →
  assert it appears (via the `/v1/events` push, not a poll) → send a prompt → kill a session
  → assert it leaves the list. A fixed N-step count (like TUI's 25/25) that CI can gate on.
- **Golden frame-vector test:** the TS `frame.ts` encoder/decoder is tested against the
  **same fixtures** `agentproto/frame_test.go` uses, so the browser's protocol port cannot
  silently diverge from the Go source of truth.
- **CSP/self-containment assertion:** the harness fails if the page issues **any** network
  request to a non-`self` origin (no CDN, no font host) — enforcing §1.3 as a test, not a
  convention.
- **Target:** `make web-selftest-container`, inside the existing container fence, skipped
  cleanly where no browser is available (like the docker/ssh round-trips gate on
  availability).

### 5.2 Track B — manual, agent-driven, entropy play-test (the primary bug-finder)

Per the play-testing-is-agent-entropy precedent, the selftest is the **regression
baseline, not the bug-finder.** The real coverage is a human/agent driving a real browser
against a throwaway daemon+mock-repo with *entropy*: improvise, vary terminal sizes, resize
mid-stream, open the same session in two browser tabs at once (multi-writer + last-resize-
wins in anger), reconnect after killing the daemon (`?since` replay under a real drop),
paste huge output, type Unicode/emoji/wide chars, background the tab and return, drive it
on a phone-width viewport. A written protocol lists *starting* moves and explicitly invites
deviation — it is **not** automated into a CI gate.

This is where "does it look nice + is it clean" gets adjudicated. Track B produces the
screenshots and the visual-taste findings that go to Sachin (§Flags).

### 5.3 The CI floor stays

Every PR keeps the shipped floor: `go build ./...`, `gofmt -l`,
`golangci-lint run --fast`, `deadcode`, `scripts/lint-file-length.sh`,
`make test-container` + `tui-driver-selftest` 25/25 — **plus**, from the harness PR onward,
`make web-selftest-container` green. The embedded-bundle PRs additionally assert the
committed `dist/` matches a fresh build (no drift between source and embedded bytes).

> **Load-bearing / flag for Sachin:** adding a Node/esbuild + Playwright/Chromium
> toolchain to the repo is a new build/test dependency. Recommendation: **commit the
> pre-built `dist/` so `go build`/`go test` never need Node** (the Go side is
> self-sufficient), and gate the JS build + Playwright behind `make web-*` targets +
> availability checks (like the docker/ssh round-trips), so CI without a browser skips
> cleanly. Worth his nod because it's the first non-Go toolchain in the tree.

---

## 6. Q6 — Build vs test split per PR

Each PR is a **small build slice + its play-test gate.** This phase is almost purely
**additive** — it adds a web client atop a frozen contract — so there is very little to
delete. The one meaningful "simplify/generalize" is that a few remaining **TUI-only
assumptions get generalized to "a client,"** noted per-row. The one server touch is the
additive `MsgHello` control frame (§4.3).

### 6.1 PR sequence

The spine front-loads **the auth + serving shell + a single attached terminal** so the
hardest browser-specific unknowns (token handoff, embedded serving, xterm.js over the real
binary WS) are proven before any breadth.

| # | Title | Scope | Size | Deletes / simplifies | Play-test gate |
|---|---|---|---|---|---|
| **PR1** | **`MsgHello` start-seq frame + TS frame codec** | Additive `agentproto` control frame carrying start seq (§4.3); TS `frame.ts` port of `Encode`/`DecodeFrame` | **S** | none (TUI ignores the new frame) | Go `frame_test.go` still green; TS golden-vector test vs the same fixtures |
| **PR2** | **Embedded static-serving shell + login** | `go:embed` asset FS on the catch-all `/`; login view (paste token → probe `Snapshot` → store in `sessionStorage`); CSP `default-src 'self'`; esbuild build → committed `dist/` | **M** | replaces the `/` 404 catch-all with the SPA fallback | manual: enable TCP listener, load page, login with real token, get an authed empty app; harness asserts no non-`self` request |
| **PR3** | **Sidebar: live session list (v1 core)** | Left rail from `Snapshot`, status dot from `Liveness`/`InFlightOp`, live updates via `/v1/events` | **M** | none | manual: create/kill sessions in the TUI, watch the web rail update via push (no poll) |
| **PR4** | **Attach terminal (v1 core) — the flagship** | xterm.js + FitAddon over `wss://…/stream`; INPUT on `onData`, RESIZE via fit, `MsgResize` reflow, `?since` reconnect using PR1's hello seq | **M–L** | none (generalizes: the stream is now consumed by a second, non-Go client — no code change, but the "TUI is the only stream client" assumption is retired) | manual entropy: type, resize, two-tab multi-writer, drop-and-reconnect replay, wide/Unicode output |
| **PR5** | **Create / kill / archive (v1 completes the loop)** | New-session modal (`CreateSession`, project picker), send-prompt, kill + archive with confirm modals | **M** | none | manual: full create→attach→type→kill loop in the browser |
| **PR6** | **`web-driver-selftest` Playwright harness** | The scripted N-step regression baseline + `make web-selftest-container`, availability-gated; CSP/self-containment assertion wired in | **M** | none | the harness IS the gate; runs green in the container fence |
| **PR7** | **Tabs (#930) in the web** | Tab bar from `InstanceData.Tabs`; `CreateTab`/`CloseTab`; per-tab stream by index | **M** | none | manual: open a shell tab, switch tabs, close it; selftest gains a tab step |
| **PR8** | **Projects pane + tasks/automations view** | Repo grouping in the rail; tasks list from `ListTasks`/`AddTask`/`UpdateTask`/`RemoveTask`/`TriggerTask` | **M** | none | manual: group by repo, add/trigger a task from the browser |
| **PR9** | **Polish surfaces: alarm banner, `[limit]` badge, restore-archived, help overlay, search** | The remaining TUI elements (§2.1) as small additive views | **S–M** | none | manual entropy sweep across all surfaces; the visual-taste pass for Sachin |
| **PR10** | **`docs/web.md` + release/screenshot pass** | User docs (enable listener → open page → login), screenshots, README/`af --help` honesty | **S** | none | docs walked e2e on a fresh deploy before merge |

### 6.2 Ordering rationale

- **PR1 first** because the start-seq frame is the sole protocol change and everything's
  reconnect correctness (PR4) depends on it; landing it alone keeps the one server touch
  isolated and independently reviewable.
- **PR2 before any UI** because "serve the bytes + get a token into the browser" is the
  first browser-specific unknown; an authed-but-empty app is the milestone that proves the
  serving+auth model with zero UI risk.
- **PR3+PR4+PR5 = the v1 slice** (list → attach → type → create/kill), the minimal complete
  loop; PR4 (attach) is the flagship and the biggest de-risk — a real browser xterm.js over
  the real binary multi-writer WS. Once PR5 lands, the product is usable.
- **PR6 (the harness) lands right after v1** so the regression baseline exists before
  breadth-expansion can regress it — mirroring how the TUI selftest guards the TUI.
- **PR7–PR9 are pure additive breadth** (tabs, projects, tasks, polish), each a small
  independent PR gated on its own play-test; any can reorder or ship in parallel.
- **PR10 locks docs + the screenshot/visual pass**, the artifact Sachin reviews.

Serialized small PRs, root gates each. Little to delete (additive phase); the only
"simplification" is retiring the implicit "the TUI is the stream's only client" assumption
(PR4) — no code change, just a widened contract that Phases 2–4 already made total.

---

## 7. FLAG FOR SACHIN — product-shaping & visual-taste decisions

Two buckets: a short list of **architecture/product** calls (like prior phases), and — the
one this phase is really about — the **visual/UX gut-check** the root Captain will bring him
on a *running deploy*. Enumerated here so he knows exactly what to look at.

### 7.1 Architecture / product calls

| # | Decision | Recommendation | Why it needs sign-off |
|---|---|---|---|
| Q1 | The daemon now serves a browser web UI (embedded, opt-in behind the TCP listener, same-origin) | **ship it embedded + opt-in** | new user-facing product surface on the daemon; positioning call even though it adds no default exposure |
| Q1 | Token handoff UX: paste-token login, stored in `sessionStorage` (not persisted to disk) | **paste + `sessionStorage`** | security-taste on a full-access credential in a browser; alternatives (localStorage persist, short-lived web tokens, cookie) are product choices |
| Q3 | Vanilla TS + xterm.js, **no** SPA framework | **vanilla** | commits the web client's whole idiom + bundle weight; hard to reverse once built |
| Q4/PR1 | One additive protocol frame (`MsgHello` start-seq) — the sole server change | **add it** | it touches `agentproto`, the frozen wire contract every client speaks |
| Q5 | First non-Go toolchain (esbuild + Playwright/Chromium); commit pre-built `dist/` so Go stays Node-free | **commit `dist/`, gate JS behind `make web-*`** | a new build/test dependency in a Go repo |
| Config | Whether to add any new config keys (e.g. a `web_enabled` toggle distinct from `listen_addr`, or reuse the listener as-is) | **reuse `listen_addr`; no new key in v1** | new canonical config keys are a public contract (CLAUDE.md ask-vs-ship) |

### 7.2 The visual/UX gut-check — what to eyeball on the running deploy

The directive is *"make sure it looks nice + is clean."* That is a judgment only Sachin can
make, on a real deploy, not from this doc. The root Captain will stand up a throwaway
daemon + mock repo, open the web app, and walk him through — please look at and rule on:

1. **Overall fidelity to the TUI.** Does it *feel* like the TUI, or like a different
   product? Layout proportions (rail width vs terminal), density, the status-dot language.
2. **The terminal itself.** Font + size, color scheme / theme (does the web pick up an
   `af`-native palette like the TUI's `ui/theme.go`?), cursor, how wide/Unicode/emoji output
   renders, scrollback feel. This is 80% of the screen — it has to feel right.
3. **Status dots & liveness legibility.** Can you tell Working / Idle / Lost / Dead /
   `[limit]` apart at a glance the way the TUI's colors let you?
4. **The modals** (new-session, prompt, confirm). Do they feel native or bolted-on? Modal
   vs inline for send-prompt?
5. **Multi-writer in the browser.** Open the same session in the TUI and the web at once —
   does watching your own typing mirror across both feel right, and is last-resize-wins
   reflow jarring or smooth?
6. **Reconnect UX.** When the daemon restarts / network blips, what does the user see —
   a spinner, a stale-then-catch-up, a flash? Is the `?since` replay invisible or ugly?
7. **Mobile / narrow viewport.** Is a phone-width browser usable or explicitly out of
   scope for v1? (Recommendation: usable-but-not-optimized; his call.)
8. **Theme / dark-vs-light.** Does the app follow the system theme, ship a single look, or
   expose a toggle?

Items 1–8 are **taste, not spec** — the plan can't pre-decide them; they're what the 80%
play-testing surfaces and what the deploy walkthrough is *for*. Everything in §1–§6 is an
internal engineering call within the frozen Phase 1–4 contract and does not need sign-off
beyond the table in §7.1.
