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
  all peers connect with NO token (require_token defaults to false; set require_token = true to require auth)
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
  is reachable from the network. Pair it with **`require_token = true`** — af
  serves a tokenless network bind and only warns (see [Remote daemon
  access](remote-http-auth.md#the-tokenless-network-warning)). Only bind a routable
  interface when you must, and put it behind a firewall. Hand-edit your global
  config and restart the daemon:

    ```toml
    # ~/.agent-factory/config.toml
    listen_addr = "0.0.0.0:8443"
    require_token = true   # strongly recommended: the default is false — no token at all
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

## Install it as an app

The web client is a PWA, so you can install it and get an app window with its own
icon instead of a browser tab — no extra tooling, it's the same daemon and the same
page.

In a Chromium browser (Chrome, Edge, Brave), open the app and click **Install app**
in the top bar; you can also use the install icon in the address bar. The button
carries a **×** — dismiss it and it won't come back.

Firefox and Safari don't offer the button. Use their own add-to-home-screen or
"Add to Dock" instead; the icons and app name come from the same manifest.

### Why the Install button isn't showing

**A browser will only install a page served from a secure context**, and that rule
decides everything here:

| Where you opened it | Secure context? | Install button |
| --- | --- | --- |
| `http://localhost:8443` / `http://127.0.0.1:8443` | yes — loopback is trusted by definition | **shows** |
| `https://af.example.ts.net` (behind a TLS proxy) | yes | **shows** |
| `http://100.x.y.z:8443` (plain HTTP over Tailscale/LAN) | **no** | **hidden** |

So if you reach af over a **plain-HTTP Tailscale or LAN address, there is no Install
button, and that is correct rather than broken**. The browser never offers an
install for an insecure origin, so a button there could only fail. Nothing else
about the app changes — it is fully functional over plain HTTP, install is simply
not on the menu.

**To install from a remote machine, put the daemon behind HTTPS** — a
TLS-terminating reverse proxy in front of `listen_addr` — and open the proxy's
`https://` origin. Tailscale's own HTTPS (`tailscale serve`) works too, since it
gives you an `https://…ts.net` origin. See
[Transport encryption](remote-http-auth.md#transport-encryption-terminate-tls-yourself).

### What gets installed

- The **app mark** — a factory on the accent tile — as the favicon, the app icon,
  and the home-screen icon (including an Android-safe maskable variant).
- A **standalone window**: no URL bar, its own entry in your launcher/dock.
- **Browser chrome that matches the app theme**, following the Auto/Light/Dark
  toggle rather than just your OS.

There is a small **service worker** behind the install. It is worth knowing exactly
what it does, because the app is a live terminal:

- It **never touches `/v1`** — not the API, not the `/v1/events` socket, not the
  session PTY streams, not web-tab or VS Code previews. Those are left on the
  browser's own network path, byte for byte as if no worker existed.
- It caches **only the static shell** (the page, the bundle, the icons), and
  **network-first**: a reachable daemon always wins, so an auto-updated af never
  serves you the old bundle. The cache is only ever consulted when the network has
  already failed, so a daemon that went away gives you the app's own "can't reach
  the daemon" screen instead of a browser error page.

Installing is not required for any of this — it's the same app either way.

---

## The auth model

Whether the web client asks you for a token depends on **where your browser is**,
judged from the real TCP connection address (never a header — those are
attacker-controlled and ignored, see [When is a token required?](remote-http-auth.md#when-is-a-token-required-loopback-vs-network)).

**By default, never.** `require_token` defaults to `false`, so the app connects
with no login screen wherever your browser is. Turning the token on is opt-in:

| Your browser is on… | Token needed? | What you see |
| --- | --- | --- |
| **Anywhere**, default (`require_token = false`) | **No** | The app loads straight through — no login screen |
| **The same machine** (loopback), `require_token = true` | No | The app loads straight through — loopback stays exempt |
| **The same machine**, `require_token` **and** `require_loopback_token` both `true` | **Yes** | A login screen asking you to paste the daemon token |
| **Another machine** (network peer), `require_token = true` | **Yes** | A login screen asking you to paste the daemon token |

The web client never guesses: it asks the daemon via `/v1/auth-info` whether
*this* connection needs a token, and skips the login screen whenever the answer is
no. That is why the default experience is simply "open the URL and you're in".

### No token by default

Making a same-machine browser hunt for a token and paste it bought no real
security — anyone on the box already runs as your user, the same trust the Unix
socket grants — and it cost every new user a login screen before they saw the
product. So the token is **off by default** and auth is opt-in. What bounds the
exposure is the *other* default: `listen_addr` is loopback-only, so nothing off
the machine can reach the daemon until you change it.

Two cases break that assumption, and both are on you to close:

- **A shared / multi-user machine**: the loopback listener has no per-user gating,
  so every local account can reach it. Set **both** `require_token = true` and
  `require_loopback_token = true` (the latter is inert on its own), or
  `listen_addr = ""` to turn the web server off.
- **A network bind**: pointing `listen_addr` at a routable interface while leaving
  the tokenless default would serve an **unauthenticated control plane** to anyone
  who can route to it. af allows that and warns once at daemon start rather than
  refusing, so this one is on you. Set
  `require_token = true` to bind the network, or keep `listen_addr` on loopback
  and reach it over SSH/Tailscale port-forwarding.

See the [Security notes](#security-notes).

### With `require_token = true`: paste the token

When you turn the token on, the web client shows a login screen with a single
field. Get the token from the **host**:

```console
$ af token show
token: kZ9abc...-...q0
```

Paste the `token` value into the field and click **Connect**. **You paste it once**:
the token is saved in the browser's `localStorage` for that origin, so a reload, a
new tab, and a browser restart all reconnect silently. A rejected token shows an
actionable error (`That token was rejected. Check af token show on the host and try
again.`) and is **forgotten**, so the next visit prompts cleanly instead of retrying
a dead credential. A connection that fails for any other reason — the daemon is
down, the wrong host — reports the daemon's own message and **keeps** the token: a
daemon restart doesn't cost you a re-paste.

See [Turning auth on](remote-http-auth.md#turning-auth-on-require_token-true) for
the full setup, and the note there on why `require_loopback_token` does nothing
unless `require_token` is also `true`.

**Disconnect** (top-right) forgets the stored token and returns you to the login
screen — useful on a shared machine.

---

## A tour of the app

The app is one screen with two top-level views, switched by the **view tabs** in
the top bar, the **project switcher** (top-right), or the `[` / `]` keys:

```
┌───────────────────────────────────────────────────────────────────────┐
│  Agent Factory   [ Sessions ]  Tasks       project ▾   ● Live   Disconnect│  ← app bar
├──────────────────────┬────────────────────────────────────────────────┤
│ Sessions      3 ▼ +New│ fix-login-flow · Live │ Agent │ Terminal │ + │  ← title + tabs
│                       │ ┌────────────────────────────────────────────┐ │
│ ● fix-login-flow      │ │                                            │ │
│   ⎇ fix/login         │ │                                            │ │
│ ○ add-metrics         │ │                                            │ │
│   ⎇ metrics           │ │   (live agent terminal — xterm.js)         │ │  ← attach terminal
│ ◆ nightly-refactor    │ │                                            │ │
│   ⎇ refactor          │ │                                            │ │
│                       │ └────────────────────────────────────────────┘ │
└──────────────────────┴────────────────────────────────────────────────┘
```

The **app bar** carries, left to right: the **Agent Factory** brand, the two
**view tabs** (Sessions / Tasks), the **project switcher** (top-right; lists
every project with per-project session + working counts, and scopes the rail and
Tasks view to the selected project), a **Live** pip showing the daemon
event-stream state (`Live` / `Connecting…` / `Reconnecting…`), and
**Disconnect**.

On a phone, width is assigned by function rather than desktop shrink behavior. With
no session selected, the session-drawer toggle and view tabs share one aligned row in
their keyboard order; the current **project** gets the wide slot on the next row beside
one **More** button. Long project names end in an ellipsis. The decorative brand
disappears, while Live status, install, theme, and Disconnect remain available as
comfortable touch targets inside **More** instead of being squeezed or removed.
Selecting a session collapses the app bar to just the hamburger — the pane's tab row
takes the whole top — and those project/view/**More** controls move into the drawer
the hamburger opens, as an overlay that never resizes the pane underneath.

### Sessions view

The default view: a **rail** of the selected project's sessions on the left, a
**main pane** on the right that attaches the selected session's live terminal.
The project switcher (top-right) scopes the rail and Tasks view; the rail lists
only sessions whose repo root matches the selected project.

**The rail** mirrors the TUI sidebar. Each row carries the same three signals as
the TUI:

- a **status dot** (waiting on you, hit a usage limit, dead-and-recovering,
  archived),
- the **title**, with the TUI's `[lost]` / `[deleting]` / `[limit]` / `[remote]`
  prefixes, and
- the **branch** as a secondary `⎇` line.

Rows are ordered exactly like the TUI: live sessions first (oldest created first),
the archived group last (newest first). The header shows the count of the sessions
currently **shown**, the **filter** control (below), and a **`+ New`** button that
opens the new-session modal (its project picker is seeded from the repos af has
seen).

#### Filtering by state

The rail shows the work you can still act on: **archived sessions are hidden by
default**, and every other state is shown. The **funnel** control in the rail header
opens a checkbox per state — Working, Ready, Lost, Dead, Limit reached, Archived —
so you can reveal the archive or narrow to just one group (only what's working, say).
Archived rows render dimmed when shown, so they read as inactive.

The filter is a **display filter, applied within the selected project**: the daemon
still sends every session, the rail just draws the ones you asked for. It also
governs `j`/`k`, which walk exactly the rows on screen. Your choice is remembered
per browser (localStorage), and the control shows a dot when it differs from the
default. When a filter hides everything, the rail says so — and tells you how many
sessions are behind it rather than looking empty.

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

On a phone the rail is a drawer over the main pane. Actions that take you somewhere
else — **+ New**, selecting a session, and Archive/Restore/Kill — close it before
opening the terminal or modal. The state filter stays open while you change it, and
tapping the dimmed scrim dismisses the drawer directly.

#### The attach terminal

The main pane hosts a real terminal (xterm.js) streaming the agent's live output
over the daemon's WebSocket PTY plane — the same bytes the TUI paints. One pane-header
row holds the session title, terminal connection state (`Live` / `Connecting…` /
`Reconnecting…` / `Agent exited`), horizontally scrolling tabs, and the conditional
**Retry** escape. The title keeps a useful minimum and ellipsizes; Retry never shrinks;
the tab strip owns the remaining width and scrolls rather than wrapping.

#### Per-session actions

Each actionable session's rail row reserves space for two quiet glyph actions. They
stay visible on the selected row and reveal on hover or keyboard focus on any other
row, without moving the title. A creating or legacy id-less row renders status only —
the daemon projects no lifecycle action, so neither the TUI nor web invents one:

- **`▪` Archive** — move a live session into the archived group. On an archived
  row the same slot becomes **`↶` Restore**.
- **`⌫` Kill** — permanently tear the session down. Its resting treatment is
  muted, while the confirmation remains explicitly destructive.

Both actions keep their confirmation step. A **Retry** button appears in the pane
header only when the selected session is blocked at a usage limit; it is the escape
from that wall and stays hidden in every other state. Send follow-up instructions by
typing in the attached terminal (or with `af sessions send-prompt`).

**Create** a session with **`+ New`**; the pending row appears immediately without
destructive controls, then opens attached when creation completes. Kill and archive
resolve the row that exposed the action by its stable id, so acting on an unselected
row and titles that collide across repos are unambiguous. Accessible action names also
include that row's title.

### Tabs

Just like the TUI, a session isn't limited to its agent — each one holds up to
**nine tabs**, all running in the *same* worktree. The tab bar shares the pane
header row with the title at every viewport width:

```
┌──────────┬────────────┬──────────┬─────────────┐
│  Agent   │  Terminal  │  btop  × │ + New tab ▾ │
└──────────┴────────────┴──────────┴─────────────┘
```

- **Tab 0 is the agent** — it's unclosable (killing the session tears it down).
- **`t`** opens a new shell tab directly. The labelled **`+ New tab`** menu
  offers **Terminal** and **VS Code** where the resulting pane will appear.
- **`w`** (or a tab's **`×`**) closes the active shell tab.
- **`1`–`9`** switch to that tab (without attaching, like j/k); **clicking** a tab
  switches *and* attaches.

Tabs labelled **Agent** / **Terminal** / a process name mirror the TUI's labels. A
failed tab op (e.g. hitting the nine-tab cap) surfaces as a brief toast rather than
a modal. **Off-box sessions** (docker, ssh, and remote-hook) run their workspace on
another host, so there is no daemon-side worktree to spawn a tab in — the tab bar
explains that the runtime has a fixed tab list, its `×` affordances are withdrawn,
and the `t` / `w` keys are disabled. Archived sessions similarly say to restore
before creating tabs instead of leaving an unexplained blank.

### Project switcher

The **project switcher** in the top-right of the app bar lists every project (repo)
af has a session or task in, each showing a per-project session + working count —
the cross-project glance that replaces the old all-projects rail. Selecting a
project scopes the rail and Tasks view to it; the choice persists across reloads.
Projects with no live sessions show an empty-state prompt (`No active sessions in
<project> — + New`). The reversible delete-project control sits in the switcher
menu footer.

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
CLI/API — they are not in the tab bar's **New tab** menu, because their target
comes from whatever the agent is running rather than from anything the UI could
ask you for (a [VS Code tab](#vs-code-tabs), which always targets the worktree,
*is* in that menu):

```bash
# a local dev server on port 5173 (Vite/Next/CRA/…)
af sessions tab-create <title> --kind web --port 5173

# any URL (localhost or external)
af sessions tab-create <title> --kind web --url http://localhost:3000
af sessions tab-create <title> --kind web --url https://example.com/docs

# a target may point at a specific page, not just a server root
af sessions tab-create <title> --kind web --url http://localhost:8899/viewer.html
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

A web tab is **pure metadata** — a URL, with no process behind it — so it
outlives anything that tears processes down: it survives a daemon/`af` restart
and it survives **archive → restore** with its target intact (unlike
shell/process tabs, whose processes are torn down at archive time and do not come
back). If the target is down when you restore, the tab renders the same
unreachable-target fallback it would at any other time — start the dev server
again and reload.

While a session is **archived** its web tabs are preserved but **inert**: the tab
shows an "archived — restore it to load this web tab" placeholder instead of
loading, the daemon refuses to proxy its target, and the tab can't be deleted (so
the URL is still there when you restore). This is deliberate — the stored target
is a bare `localhost:PORT` from whenever the tab was created, and by the time you
come back that port may belong to something else entirely. `af sessions restore`
brings the tab back to life.

!!! note "How the proxied URL maps to the dev server"
    The proxy serves the dev server under `/v1/webtab/<session>/<tab>/`, and the
    browser-visible path **mirrors the dev server's own path** beneath it:

    | target | browser URL | dev server sees |
    |---|---|---|
    | `http://localhost:3000` | `/v1/webtab/<s>/<t>/` | `/` |
    | `http://localhost:8899/viewer.html` | `/v1/webtab/<s>/<t>/viewer.html` | `/viewer.html` |
    | `http://localhost:8899/app/viewer.html` | `/v1/webtab/<s>/<t>/app/viewer.html` | `/app/viewer.html` |

    Because the depth matches, the browser resolves the app's **relative** URLs
    exactly where the dev server expects them — a sibling (`x.css`), a
    **parent-relative** one (`../shared.css`), and a **subdirectory target** all
    work, and a cookie the app scopes to a sub-path (`Path=/app`) rides on the
    matching proxied requests. Requesting the tab's bare root redirects to the
    target's path, so the URL mirrors from the first navigation on. WebSocket-based
    hot reload is proxied on a best-effort basis.

!!! warning "Absolute asset paths are not proxied"
    An app that hard-codes **absolute** asset paths (`/assets/app.js`,
    `/static/js/bundle.js`) will not find them through the proxy. An absolute path
    resolves against the **origin root**, so it escapes the tab's prefix before the
    daemon ever sees which tab it belongs to. Configure the dev server with a
    matching base path (**Vite** `base`, **CRA/webpack** `homepage` /
    `publicPath`, **Next** `basePath`) or serve relative asset URLs.

    Such a request returns a **404** naming the problem. It cannot be rerouted:
    the preview iframe is sandboxed to an **opaque origin** (so a previewed dev
    server can never reach the web UI or read its token), and a browser sends no
    `Referer` from such a frame, leaving nothing to attribute the request by.
    Answering it with the web UI's own page instead would hand the app HTML where
    it asked for JavaScript — a silent, unexplained breakage — so it fails loudly
    instead. Lifting the limitation needs a dedicated preview origin
    ([#1856](https://github.com/sachiniyer/agent-factory/issues/1856)).

!!! note "Previews over a token-protected listener"
    Over a **token-protected** network listener, iframe sub-resource requests are
    kept authorized via a path-scoped cookie (see
    [Remote HTTP auth](remote-http-auth.md)). If a preview loads only partially
    over a direct network listener, prefer an **SSH-forwarded loopback** port
    (which needs no token) — the common remote-preview path.

The iframe is sandboxed (scripts and forms run, but it gets an opaque origin, so a
proxied preview cannot reach the web UI or read its token). A small **reload**
control sits above every web tab for dev-preview refreshes.

## VS Code tabs

A **VS Code tab** is a full VS Code editor, in the browser, opened on the
session's **worktree** — so you can read and edit what an agent is building
without leaving the web UI. It renders as a pane like any other tab, and works in
splits and drag/drop.

Unlike a web tab it takes **no target**: the session's worktree is always what it
opens. That is what makes it offerable from the tab bar's labelled **New tab**
menu, which lists **Terminal** and **VS Code**. From the CLI:

```bash
af sessions tab-create <title> --kind vscode [--name editor]
```

!!! note "code-server is not bundled — install it yourself"
    af **detects** an editor rather than shipping one. It looks for
    [`code-server`](https://github.com/coder/code-server#install) first, then
    [`openvscode-server`](https://github.com/gitpod-io/openvscode-server), on the
    daemon's `PATH`. If neither is installed the tab still creates, and the pane
    shows an install hint instead of an error — install the editor and reload the
    pane. To point af at a binary outside `PATH` (or under another name), set:

    ```toml
    # ~/.agent-factory/config.toml
    vscode_server_binary = "/opt/code-server/bin/code-server"
    ```

    This key is **global-only**: it names a binary the daemon executes, so a
    repo's checked-in config can never choose what af runs on your machine.

**How it runs.** The daemon starts **one** code-server per session — shared by
every VS Code tab and pane in it — the first time a pane renders, listening on
a **0600 unix socket** in a `0700` directory under the af home (no TCP listener
at all). The browser reaches the editor through the daemon's `/v1/webtab/` proxy,
which is what makes it work for a **remote viewer** (Tailscale/SSH) and what puts
the daemon's auth policy in front of that route. On a cold start the pane briefly
shows "VS Code is still starting…" and resolves itself.

The editor is stopped when its last VS Code tab is closed, and when the session is
archived or killed — and on daemon shutdown, so nothing is left running. If it
ever dies, the next render starts a fresh one. Nothing about it is persisted: the
tab survives a restart and simply starts a new editor when you next open it.

!!! warning "`--auth none`, and why that is safe here"
    The editor runs with authentication **off**, because it is only ever
    reachable through its 0600 unix socket, which is gated to **your** account by
    filesystem permissions — the same posture the daemon's own control socket
    has. Anything running as you can dial that socket; that same-user boundary is
    the protection, not the proxy. Your browser reaches it through the daemon's
    `/v1/webtab/` proxy, so the daemon's auth policy applies to that route. It
    runs as **you**, with your `PATH` and your code-server settings/extensions.

    Note that a VS Code pane is deliberately **not** origin-sandboxed the way a web
    tab is: VS Code cannot run under an opaque origin. That is acceptable because
    the daemon controls what is served there (a code-server it started, on your
    worktree) — and that editor already gives whoever reaches it a terminal on your
    machine. Do not expose the daemon's listener to a network you do not trust.

In the **TUI** a VS Code tab shows a placeholder — a terminal can't render an
editor. Tab navigation (`1`–`9`, the sidebar tree) treats it like any other tab.

## Keyboard reference

| Key | In the Sessions view |
| --- | --- |
| `j` / `k`, `↓` / `↑` | Move the rail selection (never attaches) |
| `Enter` | Attach the selected session's terminal |
| `Escape` | Detach — return the keyboard to the rail (or close a modal) |
| `1`–`9` | Switch to that tab of the selected session |
| `t` | New shell tab in the worktree |
| `w` | Close the active shell tab (tab 0, the agent, is unclosable) |
| `[` / `]` | Cycle the top-level view (Sessions → Tasks) |

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

    - `require_token = true` **and** `require_loopback_token = true` — loopback
      peers must present the bearer token too (`af token show`). Both are needed:
      `require_token` defaults to `false`, which disables the token for everyone,
      so `require_loopback_token` alone changes nothing; or
    - `listen_addr = ""` — disable the web server entirely.

!!! warning "A network bind requires the token"
    `require_token` defaults to `false`, so pointing `listen_addr` at a routable
    interface (`0.0.0.0:8443`, a LAN/Tailscale IP) would serve **full control of
    your daemon to anyone who can reach the port, with no credential**. The daemon
    **warns once at daemon start** on that combination and serves it anyway: a
    non-loopback `listen_addr` should set `require_token = true`. This is the boundary on the
    zero-friction default — a stock install is protected by the loopback bind, and
    af will not let that protection be removed silently.

- **The token is full access.** Under the single-owner model, one token grants full
  control of the daemon. Treat it like a password; never commit it or paste it into
  a shared log. Rotate it with `af token rotate` if you suspect exposure.
- **The browser keeps the token in `localStorage`.** That is what makes you paste it
  once instead of every visit, and it is a real tradeoff: anything that can run
  same-origin JavaScript in the tab can read it. af's own bundle is fully
  self-contained (no CDN, no third-party script, and the daemon's CSP is
  `default-src 'self'`), so the exposure is an XSS in af itself — not a supply-chain
  script. The token still rides the `Authorization` header rather than a cookie the
  browser would attach automatically, so there is no CSRF surface. On a **shared
  machine**, use **Disconnect** when you step away (it erases the stored token), or
  use a private/incognito window, whose storage the browser discards on close.
- **Prefer loopback + SSH over `0.0.0.0`.** Binding `listen_addr` to `127.0.0.1`
  and forwarding over SSH keeps the port off the network entirely, encrypts the
  channel, and still gives you the browser UI.
- **af serves plain HTTP — front it for encryption.** The listener terminates no
  TLS. Never expose it beyond loopback without a TLS-terminating reverse proxy
  (nginx/Caddy), a private network (Tailscale/VPN), or an SSH tunnel — the token
  travels over the connection as-is.
- **The loopback exemption is scoped to a loopback bind.** It applies only when
  `listen_addr` is loopback (`127.0.0.1`/`::1`/`localhost`). On a **network** bind
  (`0.0.0.0`/routable) the token is enforced for every peer, loopback-origin
  included, so a same-host reverse proxy cannot bypass it. Behind a proxy on a
  loopback-bound daemon, auth is the proxy's job (or set `require_loopback_token
  = true`). See
  [Reverse proxies and the loopback exemption](remote-http-auth.md#reverse-proxies-and-the-loopback-exemption).

## See also

- [Remote daemon access](remote-http-auth.md) — the full listener, token, CORS,
  and `require_token` reference the web client rides on (including how to terminate
  TLS at a proxy).
- [The TUI](concepts/tui.md) — the terminal client the web client mirrors.
- [Tasks & automation](tasks.md) — the scheduled tasks the Tasks view drives.
- [HTTP API guide](http-api.md) — the `/v1/...` API the same listener serves.
- [Web client selftest](web-selftest.md) — the maintainer acceptance harness for
  the web client.
