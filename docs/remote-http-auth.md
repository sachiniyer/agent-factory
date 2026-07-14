# Remote daemon access: HTTP and tokens

By default the Agent Factory daemon is reachable **only** from the machine it
runs on. Local clients use a Unix socket whose `0600` permissions are the entire
auth story (see the [HTTP API guide](http-api.md#authentication)), and the
bundled web client is served on a **loopback** port (`listen_addr` defaults to
`127.0.0.1:8443`) that only the same machine can reach. Neither is exposed to the
network, and no shared secret is needed on the same box — anyone who can reach
either already runs as your user.

Sometimes you want a client on **another machine** to drive that daemon: your
laptop's TUI pointed at a workstation, a script on a build box, or a browser web
client. There are two ways to do that, and **they are not equal on security** —
pick the first one unless you have a specific reason not to.

> **The daemon serves plain HTTP — af terminates no TLS of its own.** The bearer
> token (below) travels over the connection, so if the listener is reachable by
> anyone you don't trust, put it behind a reverse proxy that terminates TLS
> (nginx, Caddy) or reach it over a private network (Tailscale, a VPN, or an SSH
> tunnel). See [Transport encryption](#transport-encryption-terminate-tls-yourself).

| | SSH (recommended) | Direct TCP + token |
|---|---|---|
| **New secrets to manage** | None — reuses your existing SSH keys | A bearer token you must generate, store, and rotate |
| **Network exposure** | Nothing new is listening; the daemon stays on its Unix socket | A plain-HTTP port is open (bind it to loopback, not `0.0.0.0`, if you can) |
| **Transport encryption** | The SSH channel encrypts everything | None from af — terminate TLS at a proxy or ride a private network |
| **Setup** | You already have it | Edit config, restart the daemon, distribute a token |
| **Works for a browser web client** | No | Yes — this is what it's for |
| **Use it when** | You can SSH to the host (almost always) | You genuinely can't tunnel, or you're serving the web client |

> **Rule of thumb:** if you can `ssh` to the box, use SSH. Reach for the TCP
> listener only when you can't — most commonly to serve the browser web client,
> which cannot open an SSH tunnel — and front it with a proxy or private network.

---

## Option 1 — SSH (recommended)

SSH already solves "authenticated, encrypted access to a remote machine" with
keys you already manage. Lean on it instead of minting a new credential.

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
to **loopback** on the host — which is the default — and forward it through SSH
rather than exposing it to the network:

```toml
# ~/.agent-factory/config.toml on the HOST — loopback only, never 0.0.0.0
# (this is already the default; shown here to be explicit)
listen_addr = "127.0.0.1:8443"
```

```bash
# On your LAPTOP: forward local :8443 to the host's loopback listener over SSH
ssh -N -L 8443:127.0.0.1:8443 you@workstation
```

Now a client on your laptop can talk to `http://127.0.0.1:8443` (see
[Option 2](#option-2-direct-tcp-token) for the flags). The token still applies,
and the encrypted SSH channel carries the traffic — so even though af itself
speaks plain HTTP, nothing rides the wire in the clear, and the port is never
reachable from the network.

---

## Option 2 — Direct TCP + token

Enable this when SSH isn't an option — most importantly, to serve the browser
web client. It opens a **plain-HTTP** TCP listener on the daemon, gated by an
optional **bearer token**.

The local Unix socket is **unaffected** — it stays tokenless and keeps working
for local clients exactly as before.

The TCP listener is **on by default, bound to loopback** (`listen_addr` defaults
to `127.0.0.1:8443`) so the bundled web client works out of the box on the same
machine. This section is about the other case: making it reachable **from the
network**, which is always an explicit opt-in — and which you should front with
TLS termination or a private network (see
[Transport encryption](#transport-encryption-terminate-tls-yourself)).

### 1. Point the listener at the network

`listen_addr` is a **global-only** key (a cloned repo must never be able to open
a network port), and it is not one of the scalar keys `af config set` writes, so
**hand-edit** your global config. Change the default loopback address to a
routable one:

```toml
# ~/.agent-factory/config.toml
listen_addr = "0.0.0.0:8443"   # routable — reachable from the network (opt-in)
                               # (the default "127.0.0.1:8443" is loopback-only)
```

Then restart the daemon so it binds the port:

```bash
af daemon restart   # live sessions keep running; the new daemon re-adopts them
```

On enable, the daemon logs a one-time banner with the bound address and the
bearer token — the operator's channel to the freshly generated credential:

```
daemon HTTP TCP listener enabled on 0.0.0.0:8443 (plain HTTP — terminate TLS at a proxy if needed)
  bearer token: kZ9…-…q0
  loopback peers (127.0.0.1/::1) connect with no token; network peers must present the token above
```

### 2. Read the token

On the **host**, `af token show` prints the bearer token. It is generated on
first access, so this is safe to run even before the listener is enabled:

```console
$ af token show
token: kZ9abc...-...q0
```

```bash
af token show --json    # same value wrapped in the {data,error} envelope
```

The **`token`** is the bearer credential. Under the single-owner auth model, one
token grants **full access**; treat it like a password. It lives in
`~/.agent-factory/daemon-token` with `0600` permissions. Because af serves plain
HTTP, the token travels over the connection as-is — only expose the listener
where the transport is otherwise protected (a proxy, a private network, or SSH).

### 3. Connect a remote client

Point any `af` client at the daemon with two flags (each has an environment
fallback):

| Flag | Env var | Meaning |
|---|---|---|
| `--daemon-url` | `AF_DAEMON_URL` | The daemon's URL: `http://host:port` or `ws://host:port` (the two are equivalent). A `wss://`/`https://` URL is rejected with an HTTP-only error — af serves no TLS; terminate it at a proxy and point af at the proxy's plaintext backend, or use a private network. |
| `--token` | `AF_DAEMON_TOKEN` | The bearer token from `af token show`. |

Flags take precedence over the environment. When `--daemon-url` (or
`AF_DAEMON_URL`) is unset, `af` uses the local Unix socket exactly as before —
the remote path is entirely opt-in.

```bash
# One-off command against a remote daemon
af sessions list \
  --daemon-url http://workstation:8443 \
  --token "$(ssh you@workstation af token show --json | jq -r .data.token)"

# Or export the environment once and drop the flags
export AF_DAEMON_URL=http://workstation:8443
export AF_DAEMON_TOKEN=kZ9abc...-...q0
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

### Migrating from the old TLS listener

Earlier versions served this listener over TLS and pinned a self-signed
certificate. TLS was **removed** — af is HTTP-only now. If you have a stale
config:

- **`wss://` / `https://` daemon URLs** → change them to `http://` (or `ws://`).
  A TLS-scheme `--daemon-url` fails fast with a clear message pointing you at
  `http://`, rather than mysteriously hanging.
- **`--tls-fingerprint` / `AF_DAEMON_TLS_FINGERPRINT`** → **removed.** Drop them;
  there is no certificate to pin.
- **`tls_cert` / `tls_key` config keys** → **removed.** An old config that still
  carries them still loads — the keys are ignored with a warning, not a hard
  error — but they do nothing. Delete them and terminate TLS at a proxy instead.

---

## Transport encryption: terminate TLS yourself

af serves plain HTTP and speaks no TLS. That is deliberate: the mandatory
self-signed certificate the old listener generated was pure friction (accept a
cert, pin a fingerprint) with no benefit on a private network, and anyone who
wants real TLS already runs a proxy or a tunnel that does it better. So when you
expose `listen_addr` to anything beyond loopback, put encryption in front of it:

- **A reverse proxy** — nginx or Caddy terminating TLS on `:443` and proxying to
  af's plaintext `127.0.0.1:8443`. This is the right choice for a public or LAN
  hostname; the proxy also handles real certificates (Let's Encrypt, a corporate
  CA) and CORS. Point browsers/clients at the **proxy's** `https://`/`wss://`
  origin; point the proxy's backend at af's `http://` port.
- **A private network** — Tailscale, WireGuard, or a VPN. The overlay encrypts
  everything, so af's plaintext listener bound to the tailnet interface is safe
  between trusted peers. Reach it directly at `http://<tailscale-ip>:8443`.
- **An SSH tunnel** — [Option 1](#option-1-ssh-recommended). The SSH channel
  encrypts the forwarded connection end to end.

> **How a same-host proxy interacts with the token depends on af's OWN bind
> address** (see [Reverse proxies and the loopback
> exemption](#reverse-proxies-and-the-loopback-exemption)): a proxy in front of a
> **loopback-bound** af is exempt (auth is then the proxy's job, unless you set
> `require_loopback_token = true`), while a **network-bound** af enforces the token
> even for the proxy's loopback connection — so the proxy must forward the token.
> A same-host proxy can never silently bypass the token on a network-bound listener.

---

## When is a token required? Loopback vs network

The token is enforced **per connection**, judged from the peer's real transport
address — never from a header. There are three cases:

| Peer | Default (`require_token` unset/true) | `require_token = false` |
|---|---|---|
| **Loopback** (`127.0.0.1` / `::1`) — a browser or client on the **same machine** | **No token** — same trust as the local Unix socket | No token |
| **Network** — any other source address | **Token required** (401 without it) | **No token** — token disabled |

### Loopback is exempt by default

A browser on the **same machine** as the daemon already has the local trust the
Unix socket grants (anyone on the box runs as your user), so requiring it to
paste a token would be friction with no security gain. Loopback peers therefore
connect with **no token** — the browser web client detects this and skips its
login screen entirely.

The exemption applies **only when `listen_addr` is loopback-bound** (the default
`127.0.0.1:8443`, or `::1`/`localhost`). On a **network** bind (`0.0.0.0`, a
routable/Tailscale IP, or `:port` = every interface) it is **withheld**: the token
is enforced for every peer, loopback-origin requests included. That is the fix
that stops a same-host reverse proxy from bypassing the token — see
[Reverse proxies and the loopback exemption](#reverse-proxies-and-the-loopback-exemption).

Loopback is determined **only** from the TCP connection's source address
(`net.IP.IsLoopback` on the real `RemoteAddr`). It is **never** inferred from
`X-Forwarded-For`, `X-Real-IP`, `Forwarded`, `Host`, or `Origin` — those are all
attacker-controlled, so a network peer that forges them to claim `127.0.0.1` is
**still rejected**. A source address cannot be spoofed and still complete the TCP
handshake, so this is the only trustworthy signal.

#### Reverse proxies and the loopback exemption

A same-host reverse proxy (nginx/Caddy — the way you add TLS) connects to af from
`127.0.0.1`, so **every** request it forwards has a loopback source address,
indistinguishable from a genuine local user. How af treats that connection is
scoped to af's **own bind address**, which is what makes the exemption safe:

- **af bound to loopback** (`127.0.0.1:8443` — the recommended proxy backend, and
  the only address a same-host proxy needs to reach). af is unreachable except
  from the same machine, so it **exempts** the proxy's loopback connection: proxied
  requests reach af with no token, and the **proxy** is responsible for auth
  (terminate it there, or enforce nothing if the proxy itself is access-controlled).
  To make af **also** demand the token from the proxy, set
  `require_loopback_token = true`.
- **af bound to a network address** (`0.0.0.0` or a routable/Tailscale IP). af
  **enforces** the token even for the proxy's loopback connection, so the proxy
  must forward it (`proxy_set_header Authorization` in nginx, or a client cert /
  the token in Caddy). A same-host proxy can **never** silently bypass the token on
  a network-bound listener — that was the bypass this rule closes.

Either way, don't assume "the proxy connects over loopback, so af trusts it": on a
network-bound listener af does **not**, and on a loopback-bound listener the trust
is a deliberate convenience you can tighten with `require_loopback_token`.

#### Shared machines: the loopback exemption is weaker than the Unix socket

The loopback exemption trusts the **machine**, not a **user**. The Unix control
socket is gated by filesystem permissions (`0600`) — only **your** account can
open it. The loopback web listener has no such per-user gate: **any local process
or user** on the box can reach `127.0.0.1:8443` and drive your sessions with no
token. On a single-user machine that's equivalent to the Unix socket (anyone who
runs a process as you already has that access); on a **shared / multi-user
machine** it is strictly weaker.

Close the gap with **`require_loopback_token`** (default `false`). Set it `true`
and loopback peers must present the bearer token too — the same credential a
network peer uses — so a same-machine account without the token is rejected:

```toml
# ~/.agent-factory/config.toml — require the token even from loopback
require_loopback_token = true
```

```bash
af daemon restart
```

The daemon then logs `require_loopback_token=true: loopback peers … must present
the token above`, and the browser web client shows its paste-token login for
same-machine visitors. (To turn the web server off entirely instead, set
`listen_addr = ""`.) `require_loopback_token` only tightens the loopback path and
is independent of `require_token`; note that `require_token = false` drops the
token for **all** peers, loopback included, so it overrides this key.

### Network peers still require the token — by default

Enabling `listen_addr` on a LAN, Tailscale, or public interface does **not**
silently expose an unauthenticated control plane: a non-loopback peer must
present the token, unchanged. This is the safe default and you should keep it —
but remember the token travels over plain HTTP, so still front the listener with
TLS termination or a private network.

### Opting out on a trusted network (`require_token = false`)

On a network you fully trust — a private Tailscale tailnet, a locked-down VPN —
you can drop the token for network peers too. `require_token` is a
**global-only** boolean (a cloned repo can never disable your auth), settable
with `af config set require_token false` or by hand-editing the global config:

```toml
# ~/.agent-factory/config.toml (global-only), default true
require_token = false
```

When `false`, the daemon logs a loud startup **warning** — anyone who can reach
`listen_addr` then has full control with no credential:

```
WARNING: require_token=false — the daemon web API on "0.0.0.0:8443" accepts
NETWORK peers with NO token; anyone who can reach it has full control. The
listener is plain HTTP (no TLS), so this leaves it fully open. Unset
require_token (or set it true) to re-enable auth.
```

Do not set this on any interface an untrusted party can reach.

---

## Rotating the token

`af token rotate` replaces the bearer token with a fresh one and prints it:

```console
$ af token rotate
token: nW4new...-...t8
```

Rotation takes effect **immediately for new connections** — the auth gate
re-reads the token file on every request, so no daemon restart is needed. Any
in-flight streams keep running until they reconnect.

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

If you front af with a reverse proxy, the browser's origin is the **proxy's**
origin — allow-list that (an `https://` origin), and let the proxy reach af's
plaintext backend.

---

## Security notes

- **The token is full access.** One token = full control of the daemon under the
  single-owner model. Store it with the same care as an SSH private key; never
  commit it or paste it into shared logs.
- **af is plain HTTP — the token is not encrypted in transit by af.** Never
  expose `listen_addr` on an untrusted network without a TLS-terminating proxy, a
  private network (Tailscale/VPN), or an SSH tunnel in front of it.
- **Prefer loopback + SSH over `0.0.0.0`.** Binding `listen_addr` to `127.0.0.1`
  and forwarding over SSH (Option 1) keeps the port off the network entirely and
  encrypts the channel. Only bind a routable interface when you must (e.g. serving
  the web client), and put it behind a proxy and a firewall.
- **The local socket is still local.** Enabling the TCP listener does not weaken
  the Unix socket, and it does not add a token requirement for local clients.
- **The loopback exemption is scoped to a loopback bind.** It applies only when
  `listen_addr` is loopback (`127.0.0.1`/`::1`/`localhost`), judged from the real
  connection address. On a **network** bind (`0.0.0.0`/routable) the token is
  enforced for every peer, loopback-origin included — so a same-host reverse proxy
  cannot bypass it. Behind a proxy on a loopback-bound af, auth is the proxy's job
  (or set `require_loopback_token = true`). See
  [Reverse proxies and the loopback exemption](#reverse-proxies-and-the-loopback-exemption).
- **Loopback trust is machine-wide, not per-user.** The default loopback web UI
  is reachable with **no token by any local account** — weaker than the Unix
  socket's `0600` owner-only gate. On a **shared / multi-user machine**, set
  `require_loopback_token = true` (loopback then needs the token too) or
  `listen_addr = ""` (disable the web server). See
  [Shared machines](#shared-machines-the-loopback-exemption-is-weaker-than-the-unix-socket).
- **`require_token = false` disables auth for the whole network.** Use it only on
  a network where every reachable party is trusted, and never on `0.0.0.0`
  without a firewall. The startup warning is there to make an accidental setting
  impossible to miss.
- **Rotate on suspected exposure.** `af token rotate` invalidates the old token
  for new connections at once — no restart, no downtime for live sessions.

## See also

- [HTTP API guide](http-api.md) — the local Unix-socket surface and the
  `{data,error}` envelope the remote listener mirrors.
- [The daemon](concepts/daemon.md) — the single-writer model behind every
  transport.
- [Configuration](configuration.md) — the global config file the keys above live
  in.
