# Remote daemon access: HTTP and tokens

For anyone who wants to reach their daemon — the web client, the TUI, or the API —
from a second machine. After this page you will know the two ways to do that, why
one of them is the recommendation, and exactly when a token is required.

By default the Agent Factory daemon is reachable **only** from the machine it
runs on. Local clients use a Unix socket whose `0600` permissions are the entire
auth story (see the [HTTP API guide](http-api.md#authentication)), and the
bundled web client is served on a **loopback** port (`network.listen_addr` defaults to
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
[network]
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

The TCP listener is **on by default, bound to loopback** (`network.listen_addr` defaults
to `127.0.0.1:8443`) so the bundled web client works out of the box on the same
machine. This section is about the other case: making it reachable **from the
network**, which is always an explicit opt-in — and which you should front with
TLS termination or a private network (see
[Transport encryption](#transport-encryption-terminate-tls-yourself)).

### 1. Point the listener at the network

`network.listen_addr` is a **global-only** key (a cloned repo must never be able to open
a network port). From the default `network.require_token=false`, the CLI order matters:
the write guard refuses to *create* a tokenless network posture, so turn the token on
first, then move the address — the reverse sequence fails at the first command:

```console
$ af config set network.require_token true
$ af config set network.listen_addr 0.0.0.0:8443
```

Or hand-edit your global config directly — the TOML block lands atomically, so key
order inside it does not matter:

```toml
# ~/.agent-factory/config.toml
[network]
listen_addr = "0.0.0.0:8443"   # routable — reachable from the network (opt-in)
                                # (the default "127.0.0.1:8443" is loopback-only)
require_token = true            # STRONGLY recommended: the default is false (no token)
```

It defaults to `false`, so a network
bind without it would serve an **unauthenticated** control plane to everyone who can
route to the port — and af **refuses the listener** rather than serve that. Omit
the token only on a network you fully trust (a private tailnet/VPN) or behind an
authenticating proxy, and say so with `network.allow_unauthenticated_network = true`
— the explicit opt-in that lets the refused bind proceed.

`af config set network.listen_addr` rebinds the listener in place — no restart; a raw
hand-edit of the block above still needs `af daemon restart`.

On enable, the daemon logs a one-time banner with the bound address and the
bearer token — the operator's channel to the freshly generated credential:

```
daemon HTTP TCP listener enabled on 0.0.0.0:8443 (plain HTTP — terminate TLS at a proxy if needed)
  bearer token: kZ9…-…q0
  listener is network-bound: every peer must present the token above, INCLUDING loopback-origin requests …
```

Had you left `network.require_token` at its `false` default without the opt-in,
the daemon would not have bound the port at all — it would have logged the
refusal at ERROR and kept the unix sockets serving. See [the unauthenticated
network listener](#the-unauthenticated-network-listener).

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

**The TUI's config editor follows the target too.** Pressing `,` in a
remote-targeted session shows the *daemon host's* config values, and its header
reads `<daemon URL> · <that host's config.toml path>` — the daemon first, because
the header is clipped to the overlay's width and which *machine* you are editing
outranks which file; and never abbreviated to `~`, since two of your own boxes
usually share a home layout and `~/.agent-factory/config.toml` would read as the
file on your laptop. Saving a
row writes it on the daemon, through the same admission-gated handler `af config
set --daemon-url` and the web form use; a daemon too old to serve those routes is
refused by name and version, and af never falls back to reading or writing your
own machine's config for a remote target. The config *assistant* (`C`) is
local-only and says so: it hand-edits the config file next to it, which is your
machine's, so it refuses a remote target instead of quietly editing the wrong
host. The editor’s
Accounts section also lists and registers accounts on that daemon; login stays
local-only, with the CLI's refusal naming the remote daemon inside the Accounts
section (#3950).

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
expose `network.listen_addr` to anything beyond loopback, put encryption in front of it:

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
> `network.require_loopback_token = true`), while a **network-bound** af enforces the token
> even for the proxy's loopback connection — so the proxy must forward the token.
> A same-host proxy can never silently bypass the token on a network-bound listener.

### The web-tab preview cookie and `X-Forwarded-Proto`

A [web tab](web.md#web-tabs)'s preview is an iframe, and an iframe's sub-resource
requests can carry neither an `Authorization` header nor an `?access_token` query.
So when a token authorizes the preview's top-level navigation, the daemon replies
with a cookie — `af_webtab_token`, `HttpOnly`, `SameSite=Strict`, and scoped to the
`/v1/webtab/` path — and the gate accepts that cookie **only** under that prefix.
It is never honored on the RPC surface, so it adds no ambient credential there.

The cookie's **`Secure`** attribute tracks the scheme the request actually arrived
over, because a browser silently **discards** a `Secure` cookie delivered over
`http://` to a non-localhost origin:

| how the browser reached af | `Secure` |
|---|---|
| plain HTTP straight to `network.listen_addr` (Tailscale/VPN/LAN) | omitted |
| HTTPS to a **TLS-terminating proxy** that sets `X-Forwarded-Proto: https` | set |
| direct TLS (`r.TLS`) | set |

If you front af with a TLS-terminating proxy, **set `X-Forwarded-Proto`** — nginx
and Caddy do by default (`proxy_set_header X-Forwarded-Proto $scheme;`). Without
it the cookie is issued without `Secure`; it still works, but it loses the
downgrade protection your `https://` origin could have given it.

The header is only ever trusted to *add* the flag, never to remove one: a peer
that forges it merely asks for a stricter cookie its own plain-HTTP browser then
refuses to store, so the failure is a broken preview rather than a weakened one.
The token itself is always verified by the gate regardless.

> On a plain-HTTP listener the token traverses the wire in the clear either way —
> the `?af_webtab_token` query that bootstraps the preview shares that hop. `Secure`
> is not what protects it; [terminating TLS](#transport-encryption-terminate-tls-yourself)
> or using a private overlay is.

---

## When is a token required? Loopback vs network

**The token is off by default.** `network.require_token` defaults to `false`, so a fresh
daemon serves its web UI and API to every peer with **no token at all**. Auth is
strictly **opt-in**: set `network.require_token = true` to turn it on. What keeps that
default safe is the *other* default — `network.listen_addr` is loopback-only
(`127.0.0.1:8443`), so nothing off the machine can reach it until you say so.

Once you do enable the token, it is enforced **per connection**, judged from the
peer's real transport address — never from a header:

| Peer | Default (`network.require_token` unset/`false`) | `network.require_token = true` |
|---|---|---|
| **Loopback** (`127.0.0.1` / `::1`) — a browser or client on the **same machine** | **No token** | **No token** on a loopback bind (unless `network.require_loopback_token = true`) |
| **Network** — any other source address | **No token** — but the bind is refused unless `network.allow_unauthenticated_network` opts in | **Token required** (401 without it) |

> **A non-loopback `network.listen_addr` should set `network.require_token = true`.** That
> combination — `network.listen_addr` on a routable interface *and* the tokenless default
> — would be an unauthenticated control plane, so af **refuses the listener**
> unless `network.allow_unauthenticated_network` opts in (see
> [the unauthenticated network listener](#the-unauthenticated-network-listener)).

### Why token-less by default

The web UI is bundled into the daemon and served on loopback. Making a
same-machine browser hunt for `af token show` and paste a credential bought no
real security — anyone on the box already runs as your user, the same trust the
`0600` Unix socket grants — and it cost every new user a login screen before
they saw the product. So the default is: open `http://localhost:8443` and it
connects. The web client reads the daemon's answer from `/v1/auth-info` and skips
its login screen whenever no token is required.

The trade-off is deliberate: `af` ships open rather than closed, and the
loopback-only `network.listen_addr` is what bounds the blast radius — out of the
box, the tokenless posture fronts a listener nothing off-box can reach.
Exposing the daemon to a network is an explicit act: a tokenless network bind is
refused unless `network.require_token` authenticates it or
`network.allow_unauthenticated_network` opts the exposure in (see
[the unauthenticated network listener](#the-unauthenticated-network-listener)).

### Loopback is exempt even with the token on

A browser on the **same machine** as the daemon already has the local trust the
Unix socket grants, so even with `network.require_token = true` loopback peers still
connect with **no token** — the token is asked of network peers only. Set
`network.require_loopback_token = true` to close that too (see below).

The exemption applies **only when `network.listen_addr` is loopback-bound** (the default
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
  `network.require_loopback_token = true`.
- **af bound to a network address** (`0.0.0.0` or a routable/Tailscale IP). af
  **enforces** the token even for the proxy's loopback connection, so the proxy
  must forward it (`proxy_set_header Authorization` in nginx, or a client cert /
  the token in Caddy). A same-host proxy can **never** silently bypass the token on
  a network-bound listener — that was the bypass this rule closes.

Either way, don't assume "the proxy connects over loopback, so af trusts it": on a
network-bound listener af does **not**, and on a loopback-bound listener the trust
is a deliberate convenience you can tighten with `network.require_loopback_token`.

#### Shared machines: the loopback exemption is weaker than the Unix socket

The loopback exemption trusts the **machine**, not a **user**. The Unix control
socket is gated by filesystem permissions (`0600`) — only **your** account can
open it. The loopback web listener has no such per-user gate: **any local process
or user** on the box can reach `127.0.0.1:8443` and drive your sessions with no
token. On a single-user machine that's equivalent to the Unix socket (anyone who
runs a process as you already has that access); on a **shared / multi-user
machine** it is strictly weaker.

Close the gap with **`network.require_loopback_token`** (default `false`). Set it `true`
and loopback peers must present the bearer token too — the same credential a
network peer uses — so a same-machine account without the token is rejected:

```toml
# ~/.agent-factory/config.toml — require the token even from loopback
[network]
require_loopback_token = true
```

```bash
af daemon restart
```

The daemon then logs `network.require_loopback_token=true: loopback peers … must present
the token above`, and the browser web client shows its paste-token login for
same-machine visitors. (To turn the web server off entirely instead, set
`network.listen_addr = ""`.)

> **`network.require_loopback_token` does nothing on its own.** It only *tightens* the
> loopback path, so it has effect only while tokens are otherwise enforced — and
> `network.require_token` now defaults to `false`, which disables the token for everyone,
> loopback included. To lock down a shared machine you must set **both**:
>
> ```toml
> [network]
> require_token = true
> require_loopback_token = true
> ```

### Turning auth on (`network.require_token = true`)

`network.require_token` is a **global-only** boolean (a cloned repo can never change your
daemon's auth posture), settable with `af config set network.require_token true` or by
hand-editing the global config:

```toml
# ~/.agent-factory/config.toml (global-only), default false
[network]
require_token = true
```

`af config set` applies the change to a running daemon at once: the auth gate reads
`network.require_token` from live config on every request, so it takes effect on the next
request — no rebind, no restart (a raw hand-edit still needs `af daemon restart`).
Network peers must then present the token
(401 without it); loopback peers stay exempt on a loopback bind unless you also
set `network.require_loopback_token = true`. The web client has no polling
loop, so an idle, already-open client stays in the app until an event or a
reconnect triggers a Snapshot resync; only that resync's 401 flips
`authRequired` and routes to the paste-token form — an arbitrary request such
as a config edit surfaces a tab error instead and leaves the app running. The
login therefore appears on the next Snapshot resync after the flip, not
immediately. Get the credential with
`af token show`.

Set it whenever `network.listen_addr` is anything but loopback, unless you genuinely
trust every host that can route to the port. Without it — or without the
`network.allow_unauthenticated_network` opt-in — af refuses to bind the listener
at all (see below). Remember the token still travels over plain HTTP, so
pair it with TLS termination or a private network.

### The unauthenticated network listener

Leaving the default `network.require_token = false` while binding `network.listen_addr` to a
routable interface would mean anyone who can reach the port has full control with
no credential — including `DeliverPrompt`, which types instructions into a running
agent and submits them, so it is remote code execution, not just data exposure.

**af refuses to bind that listener.** #2090 briefly made it a startup refusal —
the daemon would not come up at all — #2168 reversed that to warn-and-serve, and
#5137 landed between them: the **TCP listener** is refused, never the daemon. The
daemon starts and its unix sockets keep working, so the TUI, CLI, and sessions
are unaffected; what you get is one ERROR line in the daemon log:

```
ERROR: network.listen_addr "0.0.0.0:8443" is reachable from the network and network.require_token
is false, so af's full control API — including DeliverPrompt, which runs
instructions through your agents — would be served to anyone who can reach that
address, with no authentication and no TLS · the TCP listener is refused; the
daemon itself still starts and the unix control socket still works, so the TUI,
CLI, and sessions are unaffected · fix one of: `af config set
network.require_token true` to require a bearer token (`af token show` prints it),
`af config set network.listen_addr 127.0.0.1:8443` to serve this machine only, or
`af config set network.allow_unauthenticated_network true` to accept the risk
explicitly
```

`af doctor` carries a `listener` **FAIL** row for the refused posture, `af daemon
status` prints it as `tcp listener: <addr> (refused)` with the same reason, and
`af config set` / `af config unset` **refuse to write** a combination that would
be refused — with the same message — unless the write itself completes a valid
combination. "Non-loopback" means anything that is not `127.0.0.0/8`, `::1`, or
`localhost`: wildcards (`0.0.0.0`, `[::]`, `:8443`), private and routable
addresses, hostnames, and Tailscale `100.64.0.0/10` addresses all count — a
tailnet is a network like any other and gets no special case.

The three fixes, any one of which lets the listener bind:

- `af config set network.require_token true` — require the bearer token from
  network peers (`af token show` prints it). Recommended.
- `af config set network.listen_addr 127.0.0.1:8443` — serve this machine only.
- `af config set network.allow_unauthenticated_network true` — accept the risk
  explicitly. The listener then binds and serves the control API
  unauthenticated, and af says so once: an exposure notice in the daemon log, a
  `listener` warning row in `af doctor`, and the same notice in `af daemon
  status`. `af config set` warns rather than refusing on the writes that reach
  this posture.

The refusal is scoped to network binds: the ordinary loopback default is
tokenless too and binds normally — nothing off-box can reach it, which is
exactly what makes the tokenless default safe. It is also scoped to the
**control** listener: `network.preview_listen_addr` has its own origin model
(never the control API) and is unaffected.

Note that `network.require_loopback_token = true` does **not** substitute for the token. It
only withdraws the loopback exemption, and while `network.require_token` is `false` the
token is disabled for *every* peer, so that exemption is already moot — and it does
not rescue a refused bind. A network bind that you want authenticated needs
`network.require_token = true`.

**Upgrading?** If your config already carries a non-loopback
`network.listen_addr` with `network.require_token = false` and no opt-in, the daemon
still starts — the install never breaks — but the web/HTTP listener stays
refused until you pick one of the three fixes. `af doctor` and `af daemon status`
name the posture and the fixes.

The mixed-version window — an older daemon still running while the CLI is
newer — is covered on three sides:

- A write that could *create* the refused posture — `network.listen_addr` to
  a non-loopback address, or `network.require_token` off — sent by a newer
  `af` to a daemon that predates the refusal is declined by the **client**,
  which names the daemon restart that arms the gate. For a remote target
  (`--daemon-url` / `AF_DAEMON_URL`) the check is the request itself:
  exposure-capable writes go to `/v1/SetConfigValueGuarded` /
  `/v1/UnsetConfigValueGuarded`, routes only refusal-capable daemons serve, so
  an older daemon's 404 is the refusal — the capability proof and the write
  arrive in one request and cannot be split by a daemon swap. The web config
  form selects the same guarded route, so a stale browser tab writing through
  a rollback or a mixed-version proxy fails closed the same way. Remote writes
  never fall back to your local config.
- Safe writes — the token on, the address back to loopback — keep the plain
  routes, since an older daemon applies them safely and they are exactly the
  remediation.
- `af daemon status` and `af doctor` pointed at a stale daemon that is *still
  serving* the tokenless network listener report the live exposure and the
  restart, not the refusal the new code would apply.

During an `af` self-upgrade the old supervisor validates that the candidate
rebound the listener it was serving — an expectation that predates the
refusal. A candidate whose transaction journal recorded the TCP listener bound
therefore keeps it bound for the bounded probation window rather than failing
validation into a rollback that would restore the still-exposed old daemon;
the ordinary daemon that replaces it after commit refuses the bind at its own
startup. The journal records only that the listener was bound, not the auth
posture it served, so while the deferral holds the candidate's socket demands
the bearer token regardless of the refused file — it can tighten what the old
daemon served, never widen it.

On a network you fully trust — a private Tailscale tailnet, a locked-down VPN —
a tokenless listener may feel reasonable, and `network.allow_unauthenticated_network = true`
is the deliberate way to say so. If you would rather not open an unauthenticated
port at all, bind loopback and reach it over the tailnet with SSH
port-forwarding (Option 1), or set the token.

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
daemon needs that origin explicitly allow-listed. `network.cors_allowed_origins` is an
**exact-match** allow-list (no wildcards, no suffix matching):

```toml
# ~/.agent-factory/config.toml (global-only)
[network]
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
  expose `network.listen_addr` on an untrusted network without a TLS-terminating proxy, a
  private network (Tailscale/VPN), or an SSH tunnel in front of it.
- **Prefer loopback + SSH over `0.0.0.0`.** Binding `network.listen_addr` to `127.0.0.1`
  and forwarding over SSH (Option 1) keeps the port off the network entirely and
  encrypts the channel. Only bind a routable interface when you must (e.g. serving
  the web client), and put it behind a proxy and a firewall.
- **The local socket is still local.** Enabling the TCP listener does not weaken
  the Unix socket, and it does not add a token requirement for local clients.
- **The loopback exemption is scoped to a loopback bind.** It applies only when
  `network.listen_addr` is loopback (`127.0.0.1`/`::1`/`localhost`), judged from the real
  connection address. On a **network** bind (`0.0.0.0`/routable) the token is
  enforced for every peer, loopback-origin included — so a same-host reverse proxy
  cannot bypass it. Behind a proxy on a loopback-bound af, auth is the proxy's job
  (or set `network.require_loopback_token = true`). See
  [Reverse proxies and the loopback exemption](#reverse-proxies-and-the-loopback-exemption).
- **Loopback trust is machine-wide, not per-user.** The default loopback web UI
  is reachable with **no token by any local account** — weaker than the Unix
  socket's `0600` owner-only gate. On a **shared / multi-user machine**, set
  **both** `network.require_token = true` and `network.require_loopback_token = true` (the latter
  is inert on its own), or `network.listen_addr = ""` (disable the web server). See
  [Shared machines](#shared-machines-the-loopback-exemption-is-weaker-than-the-unix-socket).
- **The default is tokenless — auth is opt-in.** `network.require_token` defaults to
  `false`, so what protects a stock install is the loopback-only `network.listen_addr`,
  not a credential. Pointing `network.listen_addr` at a network would serve an
  unauthenticated control plane, so af refuses the listener unless you set
  `network.require_token = true` or opt in with
  `network.allow_unauthenticated_network`. See [the unauthenticated network
  listener](#the-unauthenticated-network-listener).
- **Rotate on suspected exposure.** `af token rotate` invalidates the old token
  for new connections at once — no restart, no downtime for live sessions.

## See also

- [HTTP API guide](http-api.md) — the local Unix-socket surface and the
  `{data,error}` envelope the remote listener mirrors.
- [The daemon](daemon.md) — the single-writer model behind every
  transport.
- [Configuration](configuration.md) — the global config file the keys above live
  in.
