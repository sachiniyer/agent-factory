# The web client

Agent Factory ships a **browser web client** — the same session rail, live
terminals, tabs, projects, and tasks you get in the [TUI](concepts/tui.md), served
straight from the daemon and rendered in a browser. It is the browser analogue of
the TUI: everything reads from the daemon's live projection, so what you see in the
browser matches what you see in `af` and `af sessions list`.

It is **on by default**. The daemon serves it on loopback
(`http://127.0.0.1:8443`) with no extra setup, so a same-machine browser reaches
it with no token. You only touch config to **disable** it or to **expose** it to
another machine; the rest of this page is a tour.

!!! note "Where the auth details live"
    This page covers the web client itself. The listener and token model it
    rides on are shared with every remote `af` client and documented in full under
    [Remote daemon access](remote-http-auth.md). This page gives you the short path
    for the common cases and links there for the rest.

---

## It's already on

The web client is served on the daemon's **plain-HTTP TCP listener**, and that
listener is bound by default at `127.0.0.1:8443`. A fresh install needs **zero**
config: start the daemon (any `af` command does), open `http://127.0.0.1:8443`, and
you're in. The address is controlled by one **global-only** key, `listen_addr` — a
cloned repo must never be able to open a network port, so it lives only in your
global config and defaults to loopback.

On startup the daemon logs a one-time banner with the address and the bearer
token:

```
daemon HTTP TCP listener enabled on 127.0.0.1:8443 (plain HTTP — terminate TLS at a proxy if needed)
  bearer token: kZ9…-…q0
  loopback peers (127.0.0.1/::1) connect with no token; network peers must present the token above
```

### Loopback (default), the network, or off

`listen_addr` takes three shapes. Because config parsing layers your file on top
of the defaults, an **absent** `listen_addr` inherits the loopback default, while
an **explicit** `listen_addr = ""` is the deliberate opt-out — the two are not the
same.

- **`127.0.0.1:8443` — the default.** The listener is reachable only from the same
  machine. A browser on that machine is a **loopback** peer and needs **no token**
  (see [the auth model](#the-auth-model)). You get this with no config at all. To
  reach it from another machine, forward the port over SSH — nothing new is
  exposed to the network:

    ```bash
    # on your laptop: forward local :8443 to the host's loopback listener
    ssh -N -L 8443:127.0.0.1:8443 you@workstation
    ```

- **`0.0.0.0:8443` (or a LAN/Tailscale IP) — expose to the network.** The listener
  is reachable from the network. A browser on another machine is a **network** peer
  and must present the bearer token by default. Only bind a routable interface when
  you must, and put it behind a firewall. Hand-edit your global config and restart
  the daemon:

    ```toml
    # ~/.agent-factory/config.toml
    listen_addr = "0.0.0.0:8443"
    ```

    ```bash
    af daemon restart   # live sessions keep running; the new daemon re-adopts them
    ```

    See [Remote daemon access](remote-http-auth.md#option-2-direct-tcp-token) for the
    full network setup, including CORS and `require_token`.

- **`""` — disable the web server.** An explicit empty value turns the HTTP TCP
  listener off entirely; the daemon runs pure-unix-socket and serves no browser UI:

    ```toml
    # ~/.agent-factory/config.toml
    listen_addr = ""
    ```

    ```bash
    af daemon restart
    ```

If the default loopback port is already taken (for example a second daemon), the
bind is **logged and skipped** — the daemon keeps running, just without the web
server. A web-port conflict never blocks session management.

The listener is **plain HTTP** — af terminates no TLS of its own. If you expose it
beyond loopback and want transport encryption, put it behind a reverse proxy
(nginx/Caddy) or a private network (Tailscale/VPN). See
[Transport encryption](remote-http-auth.md#transport-encryption-terminate-tls-yourself).

---

## Open it in a browser

Point your browser at the listener's address:

```
http://127.0.0.1:8443/
```

The same listener serves both the app (at `/`) and the API (at `/v1/...`), so
there is nothing else to run — the daemon *is* the web server. It serves plain
HTTP; there is no certificate warning to click through.

If you front the daemon with a TLS-terminating reverse proxy, point the browser at
the **proxy's** `https://` origin instead, and let the proxy reach af's plaintext
backend — see
[Transport encryption](remote-http-auth.md#transport-encryption-terminate-tls-yourself).

---

## The auth model

Whether the web client asks you for a token depends on **where your browser is**,
judged from the real TCP connection address (never a header — those are
attacker-controlled and ignored, see [When is a token required?](remote-http-auth.md#when-is-a-token-required-loopback-vs-network)).

| Your browser is on… | Token needed? | What you see |
| --- | --- | --- |
| **The same machine** as the daemon (loopback), default | **No** | The app loads straight through — no login screen |
| **The same machine**, `require_loopback_token = true` | **Yes** | A login screen asking you to paste the daemon token |
| **Another machine** (network peer), default | **Yes** | A login screen asking you to paste the daemon token |
| **Another machine**, `require_token = false` | No | The app loads straight through |

### Same machine: no token (by default)

A browser on the daemon's own machine already has the same trust the local Unix
socket grants (anyone on the box runs as your user), so requiring a token would be
friction with no security gain **on a single-user machine**. The web client detects
this and **skips the login screen entirely** — you land directly in the app.

On a **shared / multi-user machine** that assumption breaks: the loopback listener
has no per-user gating, so every local account can reach it. Set
`require_loopback_token = true` to require the token from loopback peers too (the
app then shows the same login screen a network peer sees), or `listen_addr = ""` to
turn the web server off. See the [Security notes](#security-notes).

### Another machine: paste the token

When a token *is* required, the web client shows a login screen with a single
field. Get the token from the **host**:

```console
$ af token show
token: kZ9abc...-...q0
```

Paste the `token` value into the field and click **Connect**. The token is stored
in the browser tab's `sessionStorage`, so a page reload resumes automatically; a
rejected token shows an actionable error (`That token was rejected. Check
af token show on the host and try again.`).

To reach the app from a trusted network without a token at all — a private
Tailscale tailnet, a locked-down VPN — the daemon supports an opt-in
`require_token = false`. It disables the token for network peers too and logs a
loud startup warning (the listener is plain HTTP, so front it with a proxy or a
private network). See
[Opting out on a trusted network](remote-http-auth.md#opting-out-on-a-trusted-network-require_token-false).

**Disconnect** (top-right) forgets the stored token and returns you to the login
screen — useful on a shared machine.

---

## A tour of the app

The app is one screen with three top-level views, switched by the **view tabs** in
the top bar or the `[` / `]` keys:

```
┌───────────────────────────────────────────────────────────────────────┐
│  Agent Factory   [ Sessions ] Projects  Tasks        ● Live   Disconnect│  ← app bar
├──────────────────────┬────────────────────────────────────────────────┤
│ Sessions        3 +New│  fix-login-flow · Live · fix/login             │  ← pane header
│                       │ ┌────────────────────────────────────────────┐ │
│ ● fix-login-flow      │ │ Agent │ Terminal │ +                       │ │  ← tab bar
│   ⎇ fix/login         │ ├────────────────────────────────────────────┤ │
│ ○ add-metrics         │ │                                            │ │
│   ⎇ metrics           │ │   (live agent terminal — xterm.js)         │ │  ← attach terminal
│ ◆ nightly-refactor    │ │                                            │ │
│   ⎇ refactor          │ │                                            │ │
│                       │ └────────────────────────────────────────────┘ │
│                       │           [ Prompt ]  [ Archive ]  [ Kill ]     │  ← pane actions
└──────────────────────┴────────────────────────────────────────────────┘
```

The **app bar** carries, left to right: the **Agent Factory** brand, the three
**view tabs** (Sessions / Projects / Tasks), a **Live** pip showing the daemon
event-stream state (`Live` / `Connecting…` / `Reconnecting…`), and **Disconnect**.

### Sessions view

The default view: a **rail** of every session on the left, a **main pane** on the
right that attaches the selected session's live terminal.

**The rail** mirrors the TUI sidebar. Each row carries the same three signals as
the TUI:

- a **status dot** (running, waiting on you, hit a usage limit, dead-and-recovering,
  archived),
- the **title**, with the TUI's `[lost]` / `[deleting]` / `[limit]` / `[remote]`
  prefixes, and
- the **branch** as a secondary `⎇` line.

Rows are ordered exactly like the TUI: live sessions first (oldest created first),
the archived group last (newest first). The header shows the session count and a
**`+ New`** button that opens the new-session modal (its project picker is seeded
from the repos af has seen).

#### Selecting vs. attaching (the keyboard model)

The web client uses the TUI's explicit **navigate-then-attach** model, so the
keyboard never surprises you:

- **`j` / `k` (or `↑` / `↓`) navigate the rail.** Moving the selection *does not*
  attach — it just highlights a row. j/k always navigate, even after you've been
  typing to an agent.
- **`Enter` attaches** the selected session: the main pane's terminal takes the
  keyboard, and keystrokes now flow to the agent.
- **`Escape` detaches**, handing the keyboard back to the rail. (The Escape is
  swallowed — it never leaks a stray byte to the agent.)
- **Clicking a row** does both at once: it selects *and* attaches, the same as
  `Enter` on that row. Clicking directly into the terminal also attaches; clicking
  or tabbing away returns to rail navigation.

The pane you're driving is highlighted with an accent border, so the active mode is
always legible.

#### The attach terminal

The main pane hosts a real terminal (xterm.js) streaming the agent's live output
over the daemon's WebSocket PTY plane — the same bytes the TUI paints. The pane
header shows the session title, the terminal's connection state (`Live` /
`Connecting…` / `Reconnecting…` / `Agent exited`), and the branch.

#### Per-session actions

Below the terminal, three buttons act on the selected session (each behind a
modal):

- **Prompt** — deliver a one-off prompt to the agent (the web analogue of
  `af sessions send-prompt`).
- **Archive** — move the session into the archived group (restorable).
- **Kill** — tear the session down (behind a confirm).

**Create** a session with **`+ New`**; the new row appears in the rail and opens
attached. All of create / kill / archive / prompt resolve the session by its stable
id, so titles that collide across repos are never ambiguous.

### Tabs

Just like the TUI, a session isn't limited to its agent — each one holds up to
**nine tabs**, all running in the *same* worktree. The tab bar sits between the
pane header and the terminal:

```
┌──────────┬────────────┬──────────┬───┐
│  Agent   │  Terminal  │  btop  × │ + │
└──────────┴────────────┴──────────┴───┘
```

- **Tab 0 is the agent** — it's unclosable (killing the session tears it down).
- **`t`** (or the **`+`** button) opens a new shell tab in the worktree.
- **`w`** (or a tab's **`×`**) closes the active shell tab.
- **`1`–`9`** switch to that tab (without attaching, like j/k); **clicking** a tab
  switches *and* attaches.

Tabs labelled **Agent** / **Terminal** / a process name mirror the TUI's labels. A
failed tab op (e.g. hitting the nine-tab cap) surfaces as a brief toast rather than
a modal. **Remote-hook sessions** have their tabs fixed by their hook config, so
their `+` / `×` affordances and the `t` / `w` keys are disabled.

### Projects view

The **Projects** view groups every session by its repo root — one section per
project af has seen — the browser analogue of the TUI's projects pane. It's a
**read-and-jump** surface: clicking a session under its project **opens it**, which
switches back to the Sessions view and attaches its terminal. Handy as a
per-project session switcher when you're juggling several repos.

```
Projects  2

  agent-factory   3
  ~/code/agent-factory
    ● fix-login-flow
    ○ add-metrics
    ◆ nightly-refactor

  website   1
  ~/code/website
    ● copy-tweaks
```

### Tasks view

The **Tasks** view lists the scheduled automations the daemon owns and drives their
lifecycle from the browser — the analogue of the TUI's automations pane and
`af tasks`. Each row shows the task name, its trigger (`cron: …` or `watch: …`),
whether it's enabled (`[✓]` / `[ ]`), its target session, and its last-run time and
status.

Actions per row:

- **Enable / Disable** — flip the task on or off.
- **Trigger** — fire a cron task now (shown only for **enabled cron** tasks; the
  daemon has no manual fire for watch or disabled tasks).
- **Remove** — delete the task.

**`+ Add`** opens a modal to create one: a name, a project, a **cron** or **watch**
trigger with its value, the prompt to deliver, an optional target session, and the
agent program. A cron task requires a prompt; a watch task requires its command
(and may use `{{line}}` in the prompt to interpolate the matched line). This is the
same contract as [`af tasks add`](tasks.md) — the daemon re-validates on submit, so
a bad cron expression comes back as an inline error.

---

## Web tabs

Alongside terminal tabs, a session can hold **web tabs** — a tab that renders a
**site in an iframe** instead of a terminal. The primary use is a **live
dev-server preview**: an agent runs a dev server in its worktree and injects a tab
pointing at it, so you watch the app render in the browser as the agent builds it.

Web tabs are created from inside a session (by an agent or by you) with the
CLI/API — there is no `+`-button for them, because the target comes from whatever
the agent is running:

```bash
# a local dev server on port 5173 (Vite/Next/CRA/…)
af sessions tab-create <title> --kind web --port 5173

# any URL (localhost or external)
af sessions tab-create <title> --kind web --url http://localhost:3000
af sessions tab-create <title> --kind web --url https://example.com/docs
```

How the target is rendered depends on whether it is **local** or **external**:

- **Local (`--port`, `localhost`, `127.0.0.1`, `::1`):** the **daemon
  reverse-proxies** it under a same-origin path (`/v1/webtab/…`), and the web UI
  iframes that. Because the daemon shares the machine with the dev server, the
  preview works **even when you view the web UI remotely** (over Tailscale or an
  SSH-forwarded port) — a raw `localhost` iframe would otherwise hit *your*
  machine, not the daemon's. Same-origin also sidesteps the dev server's
  `X-Frame-Options`. Only loopback targets are proxied — the daemon never proxies
  an external host, so it can't be turned into an open proxy.
- **External (`https://…`):** the web UI iframes it **directly** (never through the
  daemon). This is best-effort: many sites send `X-Frame-Options` /
  `frame-ancestors` and the **browser blocks embedding**. af does not try to defeat
  framing protections — every external web tab carries an always-present **"open
  ↗"** link (the guaranteed escape hatch), and if the site does not load in time
  (slow / unreachable) the tab shows a clean fallback panel with an **"Open in a
  new tab"** link.

In the **TUI** a web tab shows a placeholder (the target URL + "view in the web
UI or open in a browser") — a terminal can't render a browser. Tab navigation
(`1`–`9`, the sidebar tree) treats it like any other tab.

!!! note "Dev-server base path"
    The proxy serves the dev server under `/v1/webtab/<session>/<tab>/`. Apps that
    request assets with **relative** paths resolve correctly under that prefix. An
    app that hard-codes **absolute** asset paths (`/assets/app.js`) will not find
    them through the proxy — configure the dev server with a matching base path
    (e.g. Vite's `base`) or serve relative asset URLs. WebSocket-based hot reload
    is proxied on a best-effort basis.

    Over a **token-protected** network listener, iframe sub-resource requests are
    kept authorized via a path-scoped cookie; if a preview loads only partially
    over a direct network listener, prefer an **SSH-forwarded loopback** port
    (which needs no token) — the common remote-preview path.

The iframe is sandboxed (scripts and forms run, but it gets an opaque origin, so a
proxied preview cannot reach the web UI or read its token). A small **reload**
control sits above every web tab for dev-preview refreshes.

## Keyboard reference

| Key | In the Sessions view |
| --- | --- |
| `j` / `k`, `↓` / `↑` | Move the rail selection (never attaches) |
| `Enter` | Attach the selected session's terminal |
| `Escape` | Detach — return the keyboard to the rail (or close a modal) |
| `1`–`9` | Switch to that tab of the selected session |
| `t` | New shell tab in the worktree |
| `w` | Close the active shell tab (tab 0, the agent, is unclosable) |
| `[` / `]` | Cycle the top-level view (Sessions → Projects → Tasks) |

`[` / `]` work in every view; the rest are the Sessions view's session/tab keys.
While a terminal is attached, all keys except `Escape` flow to the agent.

---

## Security notes

!!! warning "Shared machines: the default loopback web UI has no local auth"
    The default `127.0.0.1:8443` web listener is reachable by **any local process
    or user** on the machine with **no token** — that's what makes zero-config
    local access work. Unlike the daemon's unix control socket, whose `0600`
    permissions restrict it to **your** account, the loopback web listener grants
    every local account on the box the same full control of your sessions.

    On a **single-user machine** (a laptop, a personal workstation) this is fine —
    anyone who can run a process as you already has that access. On a
    **shared / multi-user machine**, close the gap one of two ways:

    - `require_loopback_token = true` — loopback peers must present the bearer
      token too (`af token`), same as network peers; or
    - `listen_addr = ""` — disable the web server entirely.

- **The token is full access.** Under the single-owner model, one token grants full
  control of the daemon. Treat it like a password; never commit it or paste it into
  a shared log. Rotate it with `af token rotate` if you suspect exposure.
- **Prefer loopback + SSH over `0.0.0.0`.** Binding `listen_addr` to `127.0.0.1`
  and forwarding over SSH keeps the port off the network entirely, encrypts the
  channel, and still gives you the browser UI.
- **af serves plain HTTP — front it for encryption.** The listener terminates no
  TLS. Never expose it beyond loopback without a TLS-terminating reverse proxy
  (nginx/Caddy), a private network (Tailscale/VPN), or an SSH tunnel — the token
  travels over the connection as-is.
- **Loopback is same-machine trust, not "internal network" trust.** The token-free
  exemption is for `127.0.0.1` / `::1` only. If a same-host reverse proxy fronts the
  daemon, every request looks loopback and auth must live at the proxy.

## See also

- [Remote daemon access](remote-http-auth.md) — the full listener, token, CORS,
  and `require_token` reference the web client rides on (including how to terminate
  TLS at a proxy).
- [The TUI](concepts/tui.md) — the terminal client the web client mirrors.
- [Tasks & automation](tasks.md) — the scheduled tasks the Tasks view drives.
- [HTTP API guide](http-api.md) — the `/v1/...` API the same listener serves.
- [Web client selftest](web-selftest.md) — the maintainer acceptance harness for
  the web client.
