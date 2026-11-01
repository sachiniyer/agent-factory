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
| `--daemon-url` | `AF_DAEMON_URL` | The daemon's URL: `http://host:port` or `ws://host:port` (the two are equivalent). `https://host` / `wss://host` are accepted too (the same pair) for a daemon behind a TLS front such as `tailscale serve` or a reverse proxy; the certificate is verified against the system CAs and there is no option to skip verification. The daemon itself serves plaintext HTTP. |
| `--token` | `AF_DAEMON_TOKEN` | The bearer token from `af token show`. |

### Save the target once

`af target set <url> [--token-stdin]` stores the URL (and, read from stdin so it
never reaches argv, the token) in this machine's af home with `0600`
permissions. Precedence is `--daemon-url`/`--token`, then `AF_DAEMON_URL`/
`AF_DAEMON_TOKEN`, then the saved target. The saved token is only ever sent to
the saved URL, never to a host named by a flag or env var. `af target show`
prints which target an invocation uses and why (never the token); `af target
clear` forgets it. A saved file that is unreadable, malformed or readable by
other users is refused rather than falling back to the local daemon.

Flags take precedence over the environment. When no remote target is set,
`af` uses the local Unix socket exactly as before — the remote path is
entirely opt-in.

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
certificate. TLS was **removed** — the daemon itself is HTTP-only now; a TLS front is reached with `https://`. If you have a stale
config:

- **`wss://` / `https://` daemon URLs** → fine when something in front of the
  daemon serves a certificate your system trusts (`tailscale serve`, a reverse
  proxy). They are verified against the system CAs; a pinned self-signed
  certificate no longer works — use `http://` over a private network, or a TLS
  front with a real certificate.
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
>
> **A proxy that forwards the browser's `Host` needs that name allowed.** The daemon
> answers only requests addressed to one of its own names, so add the proxy's public
> name with `af config set network.allowed_hosts <name>` — `tailscale serve` and
> Caddy forward the incoming `Host` by default. See [Host names the daemon answers
> to](#host-names-the-daemon-answers-to).

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

One caveat covers the whole mixed-version window: the refusal lives in this
build, so it only protects what this build runs. A daemon binary that predates
it still binds and serves the tokenless network posture its config asks for —
the *running* daemon is the one that decides. If `af daemon status` shows a
listener `bound` on a network address, take that at face value and restart the
daemon on its host (`ssh box 'af upgrade && af daemon restart'`) so the
refusing build owns the socket. Guarding a newer CLI's writes to an *older*
remote daemon and live-vs-disk drift reporting
in `af daemon status` and `af doctor` are tracked as follow-ups (#5184, #5185).

On a network you fully trust — a private Tailscale tailnet, a locked-down VPN —
a tokenless listener may feel reasonable, and `network.allow_unauthenticated_network = true`
is the deliberate way to say so. If you would rather not open an unauthenticated
port at all, bind loopback and reach it over the tailnet with SSH
port-forwarding (Option 1), or set the token.

---

## Which verbs follow the target

With `--daemon-url`, `AF_DAEMON_URL` or a saved target (`af target set`), every `af` verb does one of three things, and none of them answers about the wrong machine without saying so:

| Verbs | With a remote target |
|---|---|
| `af` (the TUI), `sessions`, `tasks`, `projects`, `proposal`, `quota`, `daemon status`, global `config set` / `config unset` | **Follow the target.** They read and write that daemon over its API and never fall back to this machine. `af daemon status` reports the target's health route (version, phase, boot id), not this machine's sockets or autostart unit. |
| `token show` / `token rotate`, `daemon restart` / `install` / `uninstall` / `adopt`, `doctor`, `reset`, `config get` / `list` / `validate` / `migrate`, `config set` / `unset --project`, `accounts`, `hosts` | **Refuse**, naming the daemon they declined to act on, because they read or change a file, process or unit that exists only on the machine they run on and no daemon API stands in for it. Run them on the daemon host (for example over ssh). |
| `target`, `upgrade`, `bug-report`, `version`, `keys`, `debug`, `api`, `sessions whoami` | **Act on this client** by design: its binary, its saved target, its terminal or tmux pane, or this build's static catalogs. |

A skew error against a remote daemon names that daemon and a remedy you can run from the client: `the daemon at http://box:8443 is out of date … ssh box 'af upgrade && af daemon restart'`. A new verb has to pick one of the three: `commands/targetclass_test.go` fails the build for a verb with no class.

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

## Host names the daemon answers to

**The guarantee:** every browser-facing listener the daemon serves answers a
request only when its `Host` header names this daemon. Any other `Host` is
refused before any route runs — the static web UI, every `/v1/...` route, and
every WebSocket upgrade (the terminal stream and the events feed) alike.

Why it matters: a browser decides "same origin" from the **name** in the URL,
not from the address that name resolved to. Without this check, a web page whose
own hostname was re-pointed at `127.0.0.1` after it loaded (DNS rebinding) would
be same-origin with the tokenless loopback web UI, and could read every session
and call every route, including the ones that create sessions and send prompts.
A page cannot choose the `Host` its browser sends, so checking it closes that.

These always work, on any port:

- `localhost` and loopback IPs such as `127.0.0.1` and `[::1]`;
- the host of the address the listener was bound from (`network.listen_addr`) —
  for a wildcard bind such as `0.0.0.0:8443`, that is `http://0.0.0.0:8443` itself;
- the IP address the client actually connected to — so a wildcard bind
  (`0.0.0.0:8443`) reached as `http://192.168.1.5:8443` works with no entry;
- a request with **no** `Host` header from this same machine (HTTP/1.0 tools;
  browsers always send one).

Anything else needs an entry in `network.allowed_hosts`. The usual case is a
same-host reverse proxy that forwards the browser's `Host` unchanged:
**`tailscale serve` does exactly that** (for a TCP backend it sets the proxied
request's `Host` to the incoming one), so the daemon sees your
`<machine>.<tailnet>.ts.net` name, and that name must be allowed. Caddy's
`reverse_proxy` also forwards the incoming `Host` by default. nginx's
`proxy_pass` sends its upstream address (`127.0.0.1:8443`) unless you set
`proxy_set_header Host $http_host`, in which case add the public name too. (Use
`$http_host`, not `$host`: `$host` drops the port, so a front door on a
non-default port would be forwarded as the wrong one.) A proxy that
rewrites `Host` passes this check, but the web UI's terminals and event stream
also need the browser's name forwarded — see [behind a reverse
proxy](#behind-a-reverse-proxy) below.

```bash
# On the daemon's machine — applies on the next request, no restart
af config set network.allowed_hosts box.tailnet.ts.net
```

```toml
# ~/.agent-factory/config.toml (global-only)
[network]
allowed_hosts = ["box.tailnet.ts.net"]
```

- An entry is a bare DNS name or IP literal — no scheme, path, or port — and
  matches that host on any port, so one entry covers both the main UI
  (`https://box.tailnet.ts.net`) and the per-session editor ports
  (`https://box.tailnet.ts.net:9100`). Case and one trailing dot are ignored. There
  are no wildcards.
- `af config set` replaces the whole list (comma-separate several names) and
  refuses a malformed entry, naming the spelling that works; a hand-edited
  malformed entry is dropped at load with a warning, so it never admits anything.
- A refused request gets **`421 Misdirected Request`** with a short plain-text body
  naming the `Host` it refused and the exact line that would allow it. The daemon
  logs one line per refused name (rate-limited), with the peer address and never
  the request's path or body, and `af doctor` lists the names it has refused
  recently.
- If you see a refusal for a name you do **not** recognise, do not add it: that is
  the check doing its job against a page that tried to reach af under its own
  name.
- A `*.localhost` **subdomain** is not allowed by default. Chromium and Firefox
  resolve every `*.localhost` name to loopback themselves, but Safari before macOS
  26 asks the ordinary macOS resolver, which falls through to the network's DNS
  servers ([WebKit bug 160504](https://bugs.webkit.org/show_bug.cgi?id=160504)), so
  a hostile network could point such a name at a page of its own and then rebind
  it. Nothing in af needs one here; list it in `network.allowed_hosts` if you do.
- The web-tab preview listener (`network.preview_listen_addr`) keeps its own,
  stricter rule: it answers only the per-tab `af….localhost` names the web UI hands
  out — each one a credential the daemon minted — and every other `Host` gets a
  plain 404, so `network.allowed_hosts` does not apply there. The headless agent-server is not a browser surface; its token is
  mandatory for every peer, and it has no `Host` rule.

## Cross-site browser requests are refused

**The guarantee:** a browser request from another site never reaches the
daemon's API or its WebSocket streams over TCP, whatever `network.require_token`
says. Non-browser clients (the `af` CLI and TUI, the in-sandbox `af`, scripts)
are unaffected except for one rule: a request body must be sent as
`Content-Type: application/json`.

Why it matters: a browser sends some requests to any address without asking
first — a form-encoded, multipart or `text/plain` POST, or one with no body — and
a WebSocket handshake is not covered by CORS at all. The page that sends one
cannot read the answer, but under the tokenless default the side effect would
still happen: a session created with a prompt, a prompt sent to an agent, a
session killed. The Host check above does not stop this, because such a request
is addressed to a genuine `127.0.0.1`.

Before any route runs, the daemon judges each request to `/v1/…` in this order:

1. **An origin you listed** in `network.cors_allowed_origins` is admitted, exactly
   as before (see [CORS](#cors-for-the-browser-web-client)).
2. **Fetch metadata.** When the browser sends `Sec-Fetch-Site`, only
   `same-origin` and `none` (a URL the user typed or bookmarked) are admitted.
   `cross-site` is refused, and so is `same-site`: on this machine that means
   another port of the same host — where an agent's dev server runs — and nothing
   af serves needs it.
3. **Origin**, for an older browser that sends no fetch metadata: an `Origin` that
   is not the daemon's own (the same host name and port the request was addressed
   to, and `https` when the request arrived over HTTPS) is refused, `null`
   included.
4. **Content type.** Every POST, PUT, PATCH and DELETE must send
   `Content-Type: application/json`, even with no body. A form cannot, a JSON
   request from another site needs a preflight the daemon grants only to the
   origins in step 1, and a bare POST with no type at all is the one shape an old
   browser can send without any other signal. (The headless agent-server also
   accepts a body-less call with no `Content-Type`, which is what the daemon driving
   it sends; its token is mandatory.)

A refusal is `403` (steps 2–3) or `415` (step 4) with the daemon's usual error
envelope, and the daemon logs it, rate-limited, without the body or the query.

- **`curl` over TCP:** send JSON as JSON —
  `curl --json '{}' …`, or `curl -H 'Content-Type: application/json' -d '{}' …`.
  Plain `-d` sends a form and `-X POST` alone sends no type; both get `415`. The
  Unix socket is unchanged, so the `curl --unix-socket … -d '{}'` examples in the
  [HTTP API guide](http-api.md) keep working.
- **Not covered, deliberately:** the web-tab and editor content under
  `/v1/webtab/…`, and the preview listener. That content belongs to the app being
  previewed — its sandboxed frame's own requests are cross-site by definition —
  and it goes to that app's dev server or editor, never to af's API.

### Behind a reverse proxy

Some browser requests reach the daemon with no fetch metadata, and the daemon
then judges them by `Origin` alone, so it must be able to see the name and port
the browser used. That covers **every WebSocket** (Chromium sends no fetch
metadata on a handshake), and so every terminal and the event stream. Behind a
**plain-http** front door it covers **every request**, POSTs included: browsers
send fetch metadata only to HTTPS and loopback origins. Over HTTPS, the web UI's
ordinary requests are marked `same-origin` and need nothing more.

- **A proxy that keeps `Host`** works as is. `tailscale serve` forwards
  `Host: box.tailnet.ts.net` with `X-Forwarded-Proto: https`, and
  `Origin: https://box.tailnet.ts.net` names the same host on the same (implied)
  port 443. Caddy's `reverse_proxy` behaves the same way. Both need the name in
  `network.allowed_hosts`, as above.
- **A proxy that rewrites `Host`** must forward the browser's in
  `X-Forwarded-Host`, port included, with `X-Forwarded-Proto`. The daemon honours
  them only from a proxy on the same machine (a loopback connection), and only when
  the forwarded name is one the daemon answers to, so list the public name in
  `network.allowed_hosts` too:

```nginx
location / {
    proxy_pass http://127.0.0.1:8443;
    proxy_http_version 1.1;                       # WebSockets need these three
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header X-Forwarded-Host $http_host; # the name AND port the browser used
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Use `$http_host`, not `$host`: `$host` has no port. A forwarded name with no port
never vouches for an `Origin` that names one, so a front door on a non-default port
(`listen 8444 ssl`) forwarded as `$host` gets its own WebSockets refused rather
than guessed at — and since the daemon cannot see the port the proxy dropped, a
page served by another app on the same host's default port could pass for that
front door. `X-Forwarded-Port` (`proxy_set_header X-Forwarded-Port $server_port;`)
is also honoured, from the same loopback proxy, as the port of a port-less
forwarded name, and closes that too.

A proxy that rewrites `Host` and forwards nothing (nginx's bare `proxy_pass`)
leaves such a request indistinguishable from a foreign page's, so it is refused
with `403`, naming `X-Forwarded-Host`.

The same holds for every **editor-origin port** (`network.editor_origin_ports`).
af accepts an editor's WebSocket only from the editor's own page, judged exactly
this way, so a front door for those ports either keeps `Host` or sends
`X-Forwarded-Host $http_host`, not `$host`, in each port's server block. Otherwise
the editor loads but never connects. An editor port answers a name in
`network.allowed_hosts` on any port, so the one entry covers them all.

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
  valid token. A preflight from an origin that is **not** listed is refused
  outright, as is any other request from it (see [Cross-site browser
  requests](#cross-site-browser-requests-are-refused)).

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
- **The daemon answers only its own names.** A request whose `Host` is not
  `localhost`, a loopback IP, the listener's own address, the address the client reached, or a
  `network.allowed_hosts` entry is refused with `421` before any route runs, so a
  DNS-rebinding page cannot reach the tokenless loopback API. Add a name only if you
  reach af through it on purpose. See [Host names the daemon answers
  to](#host-names-the-daemon-answers-to).
- **No other site can drive the daemon from a browser.** Cross-site requests and
  WebSocket handshakes are refused before any route runs, in every auth posture,
  and request bodies must be JSON. See [Cross-site browser
  requests](#cross-site-browser-requests-are-refused).
- **Rotate on suspected exposure.** `af token rotate` invalidates the old token
  for new connections at once — no restart, no downtime for live sessions.

## See also

- [HTTP API guide](http-api.md) — the local Unix-socket surface and the
  `{data,error}` envelope the remote listener mirrors.
- [The daemon](daemon.md) — the single-writer model behind every
  transport.
- [Configuration](configuration.md) — the global config file the keys above live
  in.
