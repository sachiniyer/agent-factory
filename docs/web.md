# The web client

Agent Factory ships a **browser web client** — the same session rail, live
terminals, tabs, projects, and tasks you get in the [TUI](concepts/tui.md), served
straight from the daemon and rendered in a browser. It is the browser analogue of
the TUI: everything reads from the daemon's live projection, so what you see in the
browser matches what you see in `af` and `af sessions list`.

It is **off by default**. The daemon only serves it once you enable a network
listener (`listen_addr`), because a browser can't reach the daemon's local Unix
socket. Enabling that listener is the one setup step; the rest of this page is a
tour.

!!! note "Where the auth details live"
    This page covers the web client itself. The listener, TLS, and token model it
    rides on are shared with every remote `af` client and documented in full under
    [Remote daemon access](remote-tcp-auth.md). This page gives you the short path
    for the common cases and links there for the rest.

---

## Enable it

The web client is served on the daemon's **TLS TCP listener**. Turn it on with a
single global-config key, then restart the daemon so it binds the port.

`listen_addr` is a **global-only** key — a cloned repo must never be able to open a
network port — so **hand-edit** your global config:

```toml
# ~/.agent-factory/config.toml
listen_addr = "127.0.0.1:8443"   # loopback only — recommended (see below)
```

```bash
af daemon restart   # live sessions keep running; the new daemon re-adopts them
```

On startup the daemon logs a one-time banner with the address, TLS fingerprint,
and the bearer token:

```
daemon TLS TCP listener enabled on 127.0.0.1:8443 (self-signed=true)
  cert fingerprint: sha256:2f1c…
  bearer token: kZ9…-…q0
  loopback peers (127.0.0.1/::1) connect with no token; network peers must present the token above
```

### Loopback vs. the network

- **`127.0.0.1:8443` (recommended).** The listener is reachable only from the same
  machine. A browser on that machine is a **loopback** peer and needs **no token**
  (see [the auth model](#the-auth-model)). To reach it from another machine,
  forward the port over SSH — nothing new is exposed to the network:

    ```bash
    # on your laptop: forward local :8443 to the host's loopback listener
    ssh -N -L 8443:127.0.0.1:8443 you@workstation
    ```

- **`0.0.0.0:8443`.** The listener is reachable from the network. A browser on
  another machine is a **network** peer and must present the bearer token by
  default. Only bind a routable interface when you must, and put it behind a
  firewall. See [Remote daemon access](remote-tcp-auth.md#option-2-direct-tcp-token)
  for the full network setup, including CORS and `require_token`.

TLS is always on. With no `tls_cert`/`tls_key` configured, the daemon generates a
self-signed certificate once — which your browser will warn about the first time.
See [TLS trust](#opening-it-in-a-browser) below.

---

## Open it in a browser

Point your browser at the listener's address over **HTTPS**:

```
https://127.0.0.1:8443/
```

The same TLS listener serves both the app (at `/`) and the API (at `/v1/...`), so
there is nothing else to run — the daemon *is* the web server.

**Self-signed certificate (the default).** The first visit shows a browser
certificate warning because the daemon's cert is self-signed. Accept it to
continue (in Chrome: **Advanced → Proceed**; in Firefox: **Advanced → Accept the
Risk**). This is a one-time trust-on-first-use step per browser. If you'd rather
not see the warning, point the daemon at a real CA-signed certificate — see
[TLS trust](remote-tcp-auth.md#tls-trust).

!!! warning "Use `https://`, not `http://`"
    The listener is **TLS-only**. A plaintext `http://` URL will not connect —
    the daemon never serves the app in the clear.

---

## The auth model

Whether the web client asks you for a token depends on **where your browser is**,
judged from the real TCP connection address (never a header — those are
attacker-controlled and ignored, see [When is a token required?](remote-tcp-auth.md#when-is-a-token-required-loopback-vs-network)).

| Your browser is on… | Token needed? | What you see |
| --- | --- | --- |
| **The same machine** as the daemon (loopback) | **No** | The app loads straight through — no login screen |
| **Another machine** (network peer), default | **Yes** | A login screen asking you to paste the daemon token |
| **Another machine**, `require_token = false` | No | The app loads straight through |

### Same machine: no token

A browser on the daemon's own machine already has the same trust the local Unix
socket grants (anyone on the box runs as your user), so requiring a token would be
friction with no security gain. The web client detects this and **skips the login
screen entirely** — you land directly in the app.

### Another machine: paste the token

When a token *is* required, the web client shows a login screen with a single
field. Get the token from the **host**:

```console
$ af token show
token:           kZ9abc...-...q0
tls_fingerprint: sha256:2f1c9e...af
```

Paste the `token` value into the field and click **Connect**. The token is stored
in the browser tab's `sessionStorage`, so a page reload resumes automatically; a
rejected token shows an actionable error (`That token was rejected. Check
af token show on the host and try again.`).

To reach the app from a trusted network without a token at all — a private
Tailscale tailnet, a locked-down VPN — the daemon supports an opt-in
`require_token = false`. It disables the token for network peers too (TLS still
applies) and logs a loud startup warning. See
[Opting out on a trusted network](remote-tcp-auth.md#opting-out-on-a-trusted-network-require_token-false).

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

- **The token is full access.** Under the single-owner model, one token grants full
  control of the daemon. Treat it like a password; never commit it or paste it into
  a shared log. Rotate it with `af token rotate` if you suspect exposure.
- **Prefer loopback + SSH over `0.0.0.0`.** Binding `listen_addr` to `127.0.0.1`
  and forwarding over SSH keeps the port off the network entirely while still
  giving you the browser UI.
- **TLS is never optional.** The listener is always TLS, and the client never skips
  certificate verification — a self-signed cert is trusted by pinning, a CA-signed
  one by the system trust store.
- **Loopback is same-machine trust, not "internal network" trust.** The token-free
  exemption is for `127.0.0.1` / `::1` only. If a same-host reverse proxy fronts the
  daemon, every request looks loopback and auth must live at the proxy.

## See also

- [Remote daemon access](remote-tcp-auth.md) — the full listener, TLS, token, CORS,
  and `require_token` reference the web client rides on.
- [The TUI](concepts/tui.md) — the terminal client the web client mirrors.
- [Tasks & automation](tasks.md) — the scheduled tasks the Tasks view drives.
- [HTTP API guide](http-api.md) — the `/v1/...` API the same listener serves.
- [Web client selftest](web-selftest.md) — the maintainer acceptance harness for
  the web client.
