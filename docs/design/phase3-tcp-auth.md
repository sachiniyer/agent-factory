# Phase 3 — TCP + bearer-token auth (#1592)

**Status:** PLAN (review artifact — do not merge). **Phase:** 3 of 5 in the
#1592 multi-backend epic. **Predecessor:** Phase 2 shipped the client API over
`daemon-http.sock` (HTTP/JSON REST mirror + WS PTY stream + WS events) with
**auth-ready seams built as no-ops**. **Author:** Captain Claude.

Auth model is **Sachin-LOCKED** and NOT re-opened here: **one bearer token =
full access, single-owner. No mTLS, no OIDC, no per-user identity.** Loopback +
SSH-tunnel stays the zero-config default (no token on the local unix socket).
The token is for **direct TCP** (and the future Phase-5 web client). **TLS** on
the TCP listener.

---

## 0. Where Phase 2 left the seams

Phase 3 is *small* because Phase 2 pre-built every hook. The wiring already in
`master`:

| Seam | Location | Phase-2 state | Phase-3 fill-in |
|---|---|---|---|
| Token extraction | `agentproto/auth.go` — `TokenFromRequest` (header → `?access_token=` fallback) | extracts, never validates | unchanged — the extractor is already correct |
| Auth middleware | `daemon/httpauth.go` — `withAuth(next)` wraps the whole mux; discards the extracted token | no-op pass-through | constant-time compare + 401/close, **gated per-listener** |
| CORS hook | `daemon/httpauth.go` — `applyCORSPolicy` | permissive `*` | config-driven allow-list (empty = off) |
| Transport | `daemon/httpserver.go` — `startHTTPServer` binds **only** the 0600 unix socket | unix only | **+ TLS TCP listener**, same mux, same handlers |
| Stream indirection | `daemon/ws_pty.go` — `streamInfoResponse{URL, Local}` | returns a relative local path | returns an absolute authed `wss://` for remote runtimes (Phase 4 consumes) |
| Client transport | `apiclient/client.go` — `New()`/`NewWithSocket()` dial the fixed unix socket | unix only | **+ remote constructor** dialing TCP+TLS with a token |

Because every REST route *and* the WS handshake already flow through
`withAuth(newHTTPMux(cs))`, and the WS query-param fallback already exists, the
token gate drops in **without reshaping a single route, handler, or wire
message.** No new endpoints. No protocol version bump.

**Key structural property we exploit:** `withAuth` wraps the mux *before* the WS
upgrade handler runs. A gated request with a bad token gets a plain HTTP `401`
written by the middleware and `next` is never called — so `websocket.Accept`
never happens and the client's `websocket.Dial` sees the 401 and fails the
handshake. **REST and WS auth are therefore the same code path** — no
WS-specific rejection logic.

---

## 1. Resolved decisions

### 1.1 Listener model — dual listener, one handler, per-listener gate

**Recommendation:** Keep the unix socket **exactly as-is** (local trust, no
auth — filesystem 0600 perms *are* the auth, #1029) **and** add a **TLS TCP
listener** when configured. **Both serve the identical `newHTTPMux(cs)`
handler.** The only difference is the auth gate each listener wraps the mux in:

```
unix socket  →  withAuth(mux, gateOff)      // peer trusted, token ignored
TCP+TLS      →  withAuth(mux, gateToken)    // peer must present the bearer token
```

`withAuth` grows one parameter (the gate); `startHTTPServer` builds two
`http.Server`s over two listeners sharing one `controlServer`/mux.

**Config (global-only, like `daemon_poll_interval` — daemon behavior, never
in-repo):**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `listen_addr` | string | `""` (**off**) | e.g. `"0.0.0.0:8443"` or `":8443"`. Empty ⇒ no TCP listener (pure unix, today's behavior). |

**Rationale:** One key, off by default, is the whole opt-in. The unix socket is
untouched so *nothing local can regress*. Two `http.Server`s over one mux is the
standard Go idiom and keeps the handler graph single-sourced (REST/RPC parity is
preserved because both transports still dispatch the same `controlServer`).

**Rejected:** a single listener that sniffs transport (fragile); a `tcp_enable`
bool separate from the address (two keys for one concept).

### 1.2 TLS cert — self-signed by default (TOFU pin), user cert as override

**Recommendation:**
- **Default (zero-config):** on first TCP-enable the daemon **self-generates**
  an ECDSA P-256 self-signed cert, stored in the af home as `daemon-tls.crt` +
  `daemon-tls.key` (key 0600). SANs cover loopback + the machine hostname + any
  literal IP in `listen_addr`. The client **pins the cert's SHA-256
  fingerprint** (TOFU) — hostname/SAN mismatch is irrelevant under a pin, so
  connecting by IP or through a tunnel Just Works. The fingerprint is printed
  next to the token (`af token show`, daemon start log).
- **Override:** `tls_cert` / `tls_key` config keys point at user-provided PEM
  files (Let's Encrypt, corporate CA). When set, the daemon uses them verbatim
  and does **not** self-generate; the client verifies against system roots
  (no pin needed).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `tls_cert` | string | `""` | Path to a PEM cert. Empty ⇒ self-signed. |
| `tls_key` | string | `""` | Path to the matching PEM key. |

**Rationale:** Self-signed + fingerprint pin gives a genuinely zero-config
secure default (no CA, no DNS) that survives the common "connect by IP over a
tunnel" case, while the override is a clean escape hatch for anyone who owns a
domain. TLS is **mandatory on TCP** — there is no plaintext TCP mode (the token
would ride the wire in the clear). Loopback-only (`listen_addr=127.0.0.1:…`)
plus an `ssh -L` tunnel remains the recommended default for remote and needs no
token surface beyond the tunnel — but even loopback TCP is TLS'd for uniformity.

### 1.3 Token lifecycle — file-backed, auto-generated, `af token` command

**Recommendation:**
- **Generation:** 32 bytes from `crypto/rand`, base64url-encoded. Auto-generated
  by the daemon on first TCP-enable if `daemon-token` is absent; also generable
  ahead of time by `af token show` (generate-if-absent, idempotent). Same file,
  same format, whichever runs first.
- **Storage:** af home `daemon-token`, **0600**.
- **Surfacing:** printed on daemon start **only when `listen_addr` is set**
  (token + TLS fingerprint + bound address, one banner); `af token show` prints
  the token (and fingerprint) on demand.
- **Rotation:** `af token rotate` writes a fresh token and prints it. It only
  rewrites the file — no daemon RPC needed, because the gate re-reads the token
  file at each auth check (see §1.4), so rotation takes effect for **new**
  connections immediately; existing WS streams keep running until they
  reconnect. (Documented as expected behavior, not a bug.)
- **Compare:** `crypto/subtle.ConstantTimeCompare` in the gate; empty/absent
  expected token ⇒ deny all TCP (fail closed).

**CLI shape (user-facing contract — flag once):**
```
af token show      # print the token + TLS fingerprint (generates if absent)
af token rotate    # replace the token, print the new one
```

**Rationale:** File-backed + auto-gen matches the locked "daemon
auto-generates, shown on start" decision with zero mandatory steps to enable.
Re-reading the small file per *auth event* (not per byte — auth happens once per
REST call / once per WS handshake) keeps rotation live without a daemon
control-plane round-trip. Constant-time compare closes the timing-oracle.

### 1.4 `withAuth` fill-in — exact wiring

`withAuth` gains a gate; the enforcement body replaces the discarded extraction.

```go
// authGate decides whether a request on this listener must present the token.
// nil ⇒ trusted transport (unix socket): always authorized.
type authGate struct {
    expectedToken func() (string, error) // reads daemon-token per call (rotation-live)
}

func withAuth(next http.Handler, gate *authGate) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        applyCORSPolicy(w, r)                 // §1.5, config-driven
        if r.Method == http.MethodOptions {   // preflight carries no creds — answer before the gate
            w.WriteHeader(http.StatusNoContent)
            return
        }
        if gate != nil {                       // TCP: enforce
            want, err := gate.expectedToken()
            got := agentproto.TokenFromRequest(r) // header → ?access_token= (unchanged extractor)
            if err != nil || want == "" ||
                subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
                writeHTTPError(w, http.StatusUnauthorized, errUnauthorized) // envelope 401; aborts WS handshake too
                return
            }
        }
        next.ServeHTTP(w, r)
    })
}
```

- **Unix peer** (`gate == nil`): unchanged — always authorized, token ignored.
- **TCP peer** (`gate != nil`): valid bearer token required, or **401** (REST)
  / **failed handshake** (WS, because 401 pre-empts the upgrade). REST reads the
  `Authorization: Bearer` header; the browser WS path reads `?access_token=` —
  both already handled by the one `TokenFromRequest`.
- **OPTIONS preflight** is answered *before* the gate (browsers never attach
  credentials to preflight), so CORS discovery works for the web client without
  a token.

`startHTTPServer` passes `nil` for the unix server and `&authGate{…}` (holding a
closure that reads `daemon-token`) for the TCP server.

### 1.5 CORS — config allow-list, off by default

**Recommendation:** Replace the permissive `*` with an explicit allow-list.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `cors_allowed_origins` | `[]string` | `[]` (**off**) | Exact origins allowed to call the API from a browser, e.g. `["https://af.example.com"]`. |

`applyCORSPolicy` echoes the request `Origin` **only if it exactly matches** an
allow-list entry (and then sets `Access-Control-Allow-Methods/Headers`); an empty
list emits **no** `Access-Control-Allow-Origin`, so no cross-origin browser can
call the API. Same-origin and non-browser clients (TUI/CLI, curl) are unaffected
— they don't do CORS.

**Rationale:** `*` is incompatible with credentialed requests anyway (and the
API is credentialed via the token), so the permissive default was never usable
by a real web client. Explicit allow-list is the correct, minimal, off-by-default
policy; it's the *only* thing Phase 5's browser needs from Phase 3 besides the
query-param token.

### 1.6 Client side — remote target via flags/env, token threaded on every call

**Recommendation:** The TUI/CLI target a remote daemon with **flags** (falling
back to **env**), defaulting to the local unix socket when unset (today's
behavior, unchanged):

| Flag | Env | Meaning |
|---|---|---|
| `--daemon-url wss://host:port` | `AF_DAEMON_URL` | Remote daemon base URL (`https`/`wss`). Unset ⇒ local unix socket. |
| `--token <tok>` | `AF_DAEMON_TOKEN` | Bearer token for the remote daemon. |
| `--tls-fingerprint <sha256>` | `AF_DAEMON_TLS_FINGERPRINT` | Pin for a self-signed daemon cert. Omit when the daemon uses a CA cert. |

`apiclient` gains `NewRemote(url, token, pin)` — an `http.Client` whose
transport dials TCP+TLS (pinned or system-root verified) instead of the unix
socket. `Client.call` sets `Authorization: Bearer <token>` on every REST
request; `DialStream`/`AttachStream` set both the header **and**
`?access_token=` on the WS dial (header for the Go client, query for eventual
browser parity). Everything downstream — the envelope decode, the agentproto
codec, `ui/termpane`, the attach proxy — is byte-identical to the local path.

**stream-info already closes the loop:** when a session's runtime returns an
absolute `wss://` URL (Phase 4 remote/container backends), the client dials
*that* URL with its token instead of the local relative path. The indirection is
runtime-transparent: the client dials whatever it's handed. Phase 3 only needs
to teach the client to carry the token when the URL is absolute+remote.

**No client-side config file** for the MVP — flags/env are enough for a
single-owner operator, and the Phase-5 web client carries its own token in the
browser. (A `known_daemons` pin file is a trivial additive follow-up if managing
several remotes by hand becomes annoying — explicitly out of scope here.)

---

## 2. PR sequence (least-risky-first, each shippable, clean-break)

Five PRs. PR1–PR2 are **dark** (no network surface, local behavior provably
unchanged); PR3 lights up the listener behind an off-by-default key; PR4 teaches
the client to reach it; PR5 documents + hardens. Smaller than Phase 2's seven
because the seams already exist.

> Every PR floor (repo standing directive): `go build ./...`, `gofmt -l .`
> clean, `golangci-lint run --timeout=3m --fast`, `deadcode -test ./...` clean,
> `scripts/lint-file-length.sh`, `make test-container` green, `make
> tui-driver-selftest` 25/25. Clean-break / no-shim / no dual-write per the
> epic's no-backcompat mandate.

### PR1 — auth material: TLS cert + token library + `af token` command *(dark)*
- **Scope:** Self-signed cert generation + persistence + user-cert override
  resolution; token generation/load/persist/rotate + constant-time compare;
  the `af token show|rotate` command. **No listener, no middleware change, no
  wire change** — pure additive library + a CLI command that reads/writes files
  in the af home. Files are created only on demand.
- **Files:** `daemon/tlsmaterial.go` (new — gen self-signed ECDSA cert, SANs
  from host/IP, load-or-generate, resolve user `tls_cert`/`tls_key` override,
  fingerprint helper), `daemon/token.go` (new — gen/load/persist/rotate, 0600,
  `ConstantTimeEqual`), `commands/tokencmd.go` (new — `af token show|rotate`),
  `commands/root.go` (register), `config/config_types.go` +
  `config/config_schema.go` (add `listen_addr`, `tls_cert`, `tls_key`,
  `cors_allowed_origins` with defaults — declared now so the whole key set lands
  in one reviewable place).
- **Risk:** **LOW** — nothing binds a socket or changes a served byte. Worst
  case is a bad `af token` output.
- **Load-bearing (flag):** the **config key names** and the **`af token` CLI
  shape** are user-facing contracts — cheap now, painful to rename post-release.
- **Tested:** unit — cert round-trips (parse back, assert SANs/expiry/fingerprint
  stability), token gen uniqueness + 0600 + round-trip + rotate-changes-value,
  `ConstantTimeEqual` truth table; `af token show` golden output + generate-if-
  absent + idempotence; user-cert override picked over self-gen. No daemon
  needed. selftest 25/25 (untouched surface).

### PR2 — `withAuth` enforcement + CORS allow-list *(dark: unix gate off)*
- **Scope:** `withAuth(next)` → `withAuth(next, gate *authGate)`; fill in the
  constant-time compare + 401 path (§1.4); replace permissive-`*` CORS with the
  `cors_allowed_origins` allow-list (§1.5). `startHTTPServer` passes **`nil`
  gate** for the unix socket and an **empty** origin list resolves to no ACAO —
  so **local behavior is byte-for-byte unchanged**; the enforcement path is
  exercised only by a gated test server.
- **Files:** `daemon/httpauth.go` (signature + enforcement + CORS logic),
  `daemon/httpserver.go` (pass `nil` gate from `startHTTPServer`),
  `daemon/httpauth_test.go` (expand).
- **Risk:** **LOW–MED** — touches the shared middleware, but the unix path is
  provably gate-off; the diff is additive on the enforcement branch.
- **Tested:** unit matrix on a gated in-memory server — no token → 401, wrong
  token → 401, right token (header) → 200, right token (`?access_token=`) → 200,
  empty expected token → 401 (fail-closed), OPTIONS preflight → 204 without a
  token; **nil-gate (unix) → 200 regardless of token** (regression guard for
  local trust). CORS matrix: allowed origin echoed, disallowed origin → no ACAO,
  empty list → no ACAO. selftest 25/25 (local unaffected).

### PR3 — TLS TCP listener *(lights up the network surface, off by default)*
- **Scope:** `startHTTPServer` additionally binds a **TLS TCP listener on
  `listen_addr`** when non-empty, serving the same mux wrapped in
  `withAuth(mux, gateToken)`; cert resolved via PR1 (user-provided or
  self-generated); token auto-generated on first TCP-enable. Daemon logs the
  token + fingerprint + bound address on start **only when enabled**. Empty
  `listen_addr` ⇒ **no TCP listener bound — pure no-op**, identical to today.
- **Files:** `daemon/httpserver.go` (bind + serve the TLS listener alongside
  unix, wire cert/token/gate; likely a small `daemon/tcpserver.go` for the TLS
  `http.Server` construction), `daemon/daemon.go` (enable-banner log).
- **Risk:** **MED** — a real network listener + TLS handshake path. Contained by
  the default-off key: users who don't set `listen_addr` see zero change.
- **Load-bearing / irreversible (flag):** binding a **network port** is a real
  security surface; the **token is printed to the daemon log** (log-file
  readability consideration — documented). Both gated behind explicit opt-in.
  Auth *model* is locked, so not in question.
- **Tested:** integration — bind a real TLS TCP listener on `127.0.0.1:0`, then:
  REST call with correct token → 200, wrong/absent → 401; WS PTY stream with
  `?access_token=` → streams, wrong → handshake fails; cert fingerprint stable
  across restarts; user-cert override honored. Extend
  `make remote-roundtrip-container`. selftest 25/25 (default off ⇒ unaffected).
  Live: enable on loopback, `curl --cacert`/`-k` the health route with the token.

### PR4 — client remote target (`--daemon-url` / `--token` / TLS pin)
- **Scope:** `apiclient.NewRemote(url, token, pin)` dialing TCP+TLS; thread
  `Authorization: Bearer` on every REST `call`; thread the token (header +
  `?access_token=`) on WS `DialStream`/`AttachStream`; honor an absolute
  `wss://` from `stream-info` (dial it with the token instead of the local
  path). Resolve `--daemon-url`/`--token`/`--tls-fingerprint` (+ env) at the
  four client-construction sites (`api/sessions.go`, `app/session_control.go`,
  `app/live_stream.go`, `app/handle_actions.go`) through one shared resolver;
  **unset ⇒ local unix socket, unchanged.**
- **Files:** `apiclient/remote.go` (new — TCP+TLS transport, pin verifier),
  `apiclient/client.go` (token on `call`, constructor plumbing),
  `apiclient/stream.go` + `apiclient/attach.go` (token on WS dial, absolute-URL
  handling), a shared `clientTarget` resolver (flags/env → local|remote) +
  its four call-sites, `commands/root.go` (persistent flags).
- **Risk:** **MED** — touches the client-construction path shared by TUI + CLI;
  the default local path must not regress.
- **Load-bearing (flag):** the **client flag/env names** are a user-facing
  contract.
- **Tested:** point a local CLI **and** TUI at the PR3 loopback TLS listener with
  a token → full round-trip: `sessions list/get`, live pane stream, full-screen
  attach, events. Wrong/missing token → clean auth error surfaced to the user
  (not a crash). Fingerprint mismatch → refused with an actionable message.
  Default (no flags) → local unix socket, selftest 25/25. Live cross-"machine"
  loopback play-test with agent entropy (create/attach/detach/tab over TCP).

### PR5 — docs + hardening *(wrap-up)*
- **Scope:** user-facing docs for the whole flow — enabling TCP (`listen_addr`),
  `af token show|rotate`, TLS trust (self-signed pin vs CA cert), the
  **SSH-tunnel-vs-direct-TCP** guidance (tunnel stays the recommended default),
  and `cors_allowed_origins` for the future web client; `af --help` + README
  updates; delete any transitional remnants. Confirm the enable→connect→rotate
  loop end-to-end and write it down.
- **Files:** `docs/` (new `docs/remote-tcp-auth.md`), README, help text.
- **Risk:** **LOW** — docs + copy.
- **Tested:** execute the documented walkthrough end-to-end (enable TCP, grab
  token via `af token show`, connect a remote client, `af token rotate`,
  confirm old token now 401s on a new connection). Keep README/`af --help`
  honest per the repo's external-users mandate.

*(PR5 could fold into PR4 if we prefer four PRs; kept separate so the network
code and the user-facing documentation review independently and the docs land
against the final, exercised behavior.)*

---

## 3. How Phase 3 sets up Phase 4 & 5

**Phase 4 (remote/container backends to parity):** the agent-server-inside-each-
sandbox model exposes its runtime over an **authed `wss://` URL** — exactly the
TCP+TLS+token surface Phase 3 builds for the daemon itself. `stream-info`
already returns a per-runtime `URL`; Phase 4 backends populate it with their own
`wss://`, and the client dials it with **`apiclient.NewRemote` / the token
threading built in PR4** — no new client transport. Phase 3 is the *proof* that
the client can dial an authenticated network PTY stream; Phase 4 just points it
at more of them.

**Phase 5 (browser / xterm.js):** the two browser-specific seams are both filled
here — **`?access_token=`** (browsers can't set WS headers) is honored by the
gate, and **`cors_allowed_origins`** gates the web origin. The web client sets
its origin in `cors_allowed_origins`, passes the token in the WS query, and
consumes the identical raw-PTY WS stream the TUI does. No protocol reshape.

---

## 4. Non-goals / explicitly out of scope

- **mTLS, OIDC, per-user identity, RBAC** — locked out by the auth model.
- **Multiple tokens / scopes / expiry** — single token, manual rotation
  (trade accepted with the locked model: leaked token = full access).
- **A client-side `known_daemons` config file** — flags/env suffice for MVP;
  additive later.
- **Plaintext TCP** — TLS is mandatory on the TCP listener; no clear-text mode.
- **Changing the local unix-socket trust model** — filesystem perms stay the
  local auth; the token is a TCP-only concept.
</content>
</invoke>
