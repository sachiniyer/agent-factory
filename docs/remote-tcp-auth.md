# Remote daemon access: TCP, TLS, and tokens

By default the Agent Factory daemon is reachable **only** from the machine it
runs on, over a local Unix socket whose `0600` permissions are the entire auth
story (see the [HTTP API guide](http-api.md#authentication)). There is no open
port and no shared secret — anyone who can read the socket already runs as your
user.

Sometimes you want a client on **another machine** to drive that daemon: your
laptop's TUI pointed at a workstation, a script on a build box, or (in the
future) a browser web client. There are two ways to do that, and **they are not
equal on security** — pick the first one unless you have a specific reason not
to.

| | SSH (recommended) | Direct TCP + token |
|---|---|---|
| **New secrets to manage** | None — reuses your existing SSH keys | A bearer token you must generate, store, and rotate |
| **Network exposure** | Nothing new is listening; the daemon stays on its Unix socket | A TLS port is open (bind it to loopback, not `0.0.0.0`, if you can) |
| **Setup** | You already have it | Edit config, restart the daemon, distribute a token + TLS pin |
| **Works for a browser web client** | No | Yes — this is what it's for |
| **Use it when** | You can SSH to the host (almost always) | You genuinely can't tunnel, or you're serving the web client |

> **Rule of thumb:** if you can `ssh` to the box, use SSH. Reach for the TCP
> listener only when you can't — most commonly to serve the browser web client,
> which cannot open an SSH tunnel.

---

## Option 1 — SSH (recommended)

SSH already solves "authenticated access to a remote machine" with keys you
already manage. Lean on it instead of minting a new credential.

**Just run `af` on the host.** Open an SSH session and use `af` there — the TUI
renders over your terminal, and one-off commands work too:

```bash
ssh you@workstation                # then run `af` interactively
ssh you@workstation af sessions list
ssh you@workstation af sessions send-prompt my-session "run the tests"
```

Nothing new listens on the network, no token exists to leak, and the daemon's
local Unix socket (`0600`) is the only gate — which is exactly the single-user,
local-only model it was designed for.

**If you specifically want a *local* client driving the remote daemon** (for
example your laptop's TUI, over a low-latency link), keep the TCP listener bound
to **loopback** on the host and forward it through SSH rather than exposing it to
the network:

```toml
# ~/.agent-factory/config.toml on the HOST — loopback only, never 0.0.0.0
listen_addr = "127.0.0.1:8443"
```

```bash
# On your LAPTOP: forward local :8443 to the host's loopback listener over SSH
ssh -N -L 8443:127.0.0.1:8443 you@workstation
```

Now a client on your laptop can talk to `wss://127.0.0.1:8443` (see
[Option 2](#option-2-direct-tcp-token) for the flags). The token and TLS still
apply, but the port is never reachable from the network — SSH is the only way
in, and the encrypted SSH channel carries the traffic.

---

## Option 2 — Direct TCP + token

Enable this when SSH isn't an option — most importantly, to serve the browser
web client. It opens a **TLS-only** TCP listener on the daemon, gated by a
**bearer token**. TLS is mandatory (the token must never ride the wire in the
clear) and certificate verification is never skipped.

The local Unix socket is **unaffected** — it stays tokenless and keeps working
for local clients exactly as before. The TCP listener is purely additive and
**off by default**.

### 1. Enable the listener

`listen_addr` is a **global-only** key (a cloned repo must never be able to open
a network port), and it is not one of the scalar keys `af config set` writes, so
**hand-edit** your global config:

```toml
# ~/.agent-factory/config.toml
listen_addr = "0.0.0.0:8443"   # or "127.0.0.1:8443" for loopback-only (SSH-tunnelled)
```

Then restart the daemon so it binds the port:

```bash
af daemon restart   # live sessions keep running; the new daemon re-adopts them
```

On enable, the daemon logs a one-time banner with the bound address, the TLS
fingerprint, and the bearer token — the operator's channel to the freshly
generated credential:

```
daemon TLS TCP listener enabled on 0.0.0.0:8443 (self-signed=true)
  cert fingerprint: sha256:2f1c…
  bearer token: kZ9…-…q0
```

### 2. Read the token and TLS fingerprint

On the **host**, `af token show` prints the bearer token and the TLS
fingerprint a client pins. Both are generated on first access, so this is safe
to run even before the listener is enabled:

```console
$ af token show
token:           kZ9abc...-...q0
tls_fingerprint: sha256:2f1c9e...af
```

```bash
af token show --json    # same values wrapped in the {data,error} envelope
```

- **`token`** — the bearer credential. Under the single-owner auth model, one
  token grants **full access**; treat it like a password. It lives in
  `~/.agent-factory/daemon-token` with `0600` permissions.
- **`tls_fingerprint`** — the SHA-256 of the daemon's TLS certificate,
  formatted `sha256:<hex>`. A client pins this to trust a self-signed cert
  (see [TLS trust](#tls-trust)). It depends on the **certificate**, not the
  token, so rotating the token never changes it.

### 3. Connect a remote client

Point any `af` client at the daemon with three flags (each has an environment
fallback):

| Flag | Env var | Meaning |
|---|---|---|
| `--daemon-url` | `AF_DAEMON_URL` | The daemon's TLS URL: `wss://host:port` or `https://host:port` (the two are equivalent). Plaintext `ws://`/`http://` is rejected — the listener is TLS-only. |
| `--token` | `AF_DAEMON_TOKEN` | The bearer token from `af token show`. |
| `--tls-fingerprint` | `AF_DAEMON_TLS_FINGERPRINT` | The pinned `sha256:…` fingerprint for a self-signed cert. **Omit** when the daemon uses a CA-signed cert. |

Flags take precedence over the environment. When `--daemon-url` (or
`AF_DAEMON_URL`) is unset, `af` uses the local Unix socket exactly as before —
the remote path is entirely opt-in.

```bash
# One-off command against a remote daemon (self-signed cert, pinned)
af sessions list \
  --daemon-url wss://workstation:8443 \
  --token "$(ssh you@workstation af token show --json | jq -r .data.token)" \
  --tls-fingerprint sha256:2f1c9e...af

# Or export the environment once and drop the flags
export AF_DAEMON_URL=wss://workstation:8443
export AF_DAEMON_TOKEN=kZ9abc...-...q0
export AF_DAEMON_TLS_FINGERPRINT=sha256:2f1c9e...af
af sessions list          # now talks to the remote daemon
af                        # the TUI, too — the flags are global
```

Every downstream layer — the `{data,error}` envelope, the WebSocket PTY stream,
live panes, full-screen attach — is byte-identical to the local path. Only the
transport differs, so once connected the client behaves exactly like a local
one.

An invalid or missing token is rejected with a **401** on every request and on
the WebSocket handshake; a remote read surfaces that error rather than silently
falling back to a local disk scan (there is no local disk on the other end).

---

## TLS trust

The listener is always TLS. You choose how the client trusts the certificate.

### Self-signed (default, zero-config)

With no `tls_cert`/`tls_key` configured, the daemon generates a long-lived
self-signed ECDSA certificate once and stores it in the af home. The client
trusts it by **pinning its SHA-256 fingerprint** (trust-on-first-use):

- Get the fingerprint from `af token show` on the host and pass it as
  `--tls-fingerprint sha256:…`.
- The pin **replaces** the usual CA-chain and hostname checks — it is not
  skipped. An exact fingerprint match is required on every handshake, so
  connecting by IP or through an SSH tunnel Just Works despite the cert's
  hostname, while a substituted or regenerated cert **fails** the handshake with
  a clear mismatch error.
- The fingerprint accepts the `sha256:<hex>` form `af token show` prints, plain
  hex, or colon/space-separated hex.

### CA-signed certificate

If you have a real certificate (Let's Encrypt, a corporate CA), point the daemon
at it and skip the pin — the client verifies it against the system trust store
like any HTTPS server:

```toml
# ~/.agent-factory/config.toml (global-only, both keys required together)
listen_addr = "0.0.0.0:8443"
tls_cert = "/etc/af/tls/fullchain.pem"
tls_key  = "/etc/af/tls/privkey.pem"
```

Then connect **without** `--tls-fingerprint`:

```bash
af sessions list --daemon-url wss://af.example.com:8443 --token "$TOKEN"
```

Setting only one of `tls_cert`/`tls_key` is a configuration error — a cert
without its key (or vice versa) cannot serve TLS.

---

## Rotating the token

`af token rotate` replaces the bearer token with a fresh one and prints it:

```console
$ af token rotate
token: nW4new...-...t8
```

Rotation takes effect **immediately for new connections** — the auth gate
re-reads the token file on every request, so no daemon restart is needed. Any
in-flight streams keep running until they reconnect. The TLS fingerprint is
**unchanged** (it depends on the certificate, not the token), so clients keep
their existing `--tls-fingerprint` pin.

After rotating, the **old token is dead**: a new connection presenting it gets a
`401`. Re-distribute the new token to your clients.

---

## CORS (for the browser web client)

Browsers enforce CORS, so a web client served from a different origin than the
daemon needs that origin explicitly allow-listed. `cors_allowed_origins` is an
**exact-match** allow-list (no wildcards, no suffix matching):

```toml
# ~/.agent-factory/config.toml (global-only)
listen_addr = "0.0.0.0:8443"
cors_allowed_origins = ["https://af.example.com"]
```

- Empty (the default) emits no `Access-Control-Allow-Origin`, so **no
  cross-origin browser** can reach the API.
- Non-browser clients (the TUI, the `af` CLI, `curl`) don't do CORS and are
  **unaffected** by this key either way.
- A CORS preflight (`OPTIONS`) carries no credentials and is answered before the
  token gate, so cross-origin discovery works; the actual request still needs a
  valid token.

---

## Security notes

- **The token is full access.** One token = full control of the daemon under the
  single-owner model. Store it with the same care as an SSH private key; never
  commit it or paste it into shared logs.
- **Prefer loopback + SSH over `0.0.0.0`.** Binding `listen_addr` to
  `127.0.0.1` and forwarding over SSH (Option 1) keeps the port off the network
  entirely. Only bind a routable interface when you must (e.g. serving the web
  client), and put it behind a firewall.
- **TLS is never optional and never unverified.** The client refuses a plaintext
  URL and refuses a cert that neither matches the pin nor chains to a trusted CA.
- **The local socket is still local.** Enabling the TCP listener does not weaken
  the Unix socket, and it does not add a token requirement for local clients.
- **Rotate on suspected exposure.** `af token rotate` invalidates the old token
  for new connections at once — no restart, no downtime for live sessions.

## See also

- [HTTP API guide](http-api.md) — the local Unix-socket surface and the
  `{data,error}` envelope the remote listener mirrors.
- [The daemon](concepts/daemon.md) — the single-writer model behind every
  transport.
- [Configuration](configuration.md) — the global config file the keys above live
  in.
