# Web client selftest

The **web-driver-selftest** is the acceptance proof for the embedded browser web
client (`web/`, #1592 Phase 5) — the browser analogue of the
[TUI driver selftest](tui-manual-testing.md). It drives the daemon's embedded
single-page app in a headless Chromium against a **real** `af` daemon and asserts
the core flows end to end. Assertions are the gate, not screenshots.

## Running it

```bash
make web-selftest-container
```

That is the only sanctioned entry point. It:

1. Builds a dedicated container image
   (`scripts/container/Dockerfile.web-selftest`) carrying three toolchains the
   plain [testbox image](container-testing.md) deliberately omits: **Go** (to
   build `af` and run the daemon), **Node** (to run Playwright), and a real
   **Chromium** with all its system deps. It is pinned to the Playwright version
   locked in `web/package-lock.json` so the bundled browser matches the npm
   package.
2. Runs the whole harness inside **one ephemeral container**
   (`scripts/container/web-selftest-entry.sh`): builds `af`, brings up a real
   daemon on a **throwaway home** with a loopback TLS+token listener
   (`listen_addr=127.0.0.1:8899`), seeds two sessions in a mock repo behind a
   fake agent, then runs Playwright (`web/selftest/web-driver.spec.ts`).

Everything — the daemon, its tmux server, the sessions, the browser — lives on
`127.0.0.1` inside the container and dies with it. There are **no published
ports**, no access to the host tmux server, and no touch of the real
`~/.agent-factory`, exactly like the other container harnesses.

## What it asserts

Against the live SPA served over the daemon's TLS listener:

| Flow | Assertion |
| --- | --- |
| **Login** | Pasting the daemon token into the login form renders the authed app. |
| **Sidebar** | The rail lists the seeded sessions from the Snapshot/events plane, and the live pip reads "Live". |
| **Attach** | Click-to-attach opens the xterm terminal and shows the fake agent's live output (a real binary PTY frame decoded by the TS codec and painted in the browser). |
| **Keyboard (#1694)** | In the sessions view's rail mode `j`/`k` navigate the rail; `Enter` attaches the selection; `Escape` returns to the rail. |
| **View cycling (#1694/PR8)** | In rail mode `]` cycles the top-level view forward (sessions → projects → tasks) and `[` cycles it back (tasks → projects → sessions), the active view tab following each step. |
| **Tabs (#1592 PR7)** | The tab bar creates a shell tab (`+` / `t`), switches to it (click / `1`-`9`) and shows its distinct PTY output, and closes it (`×` / `w`) — the agent tab stays unclosable. |
| **Create** | The **+ New** modal creates a session and its row appears in the rail. |
| **Kill** | The kill confirm removes the session's row. |
| **Archive** | The archive confirm moves a session into the archived group. |

## Toolchain boundary

Node and Playwright stay **entirely** behind `make web-selftest-container` (and
`make web-build` / `make web-test`). `go build ./...` and `make test-container`
never invoke them — the built `web/dist/` is committed, so the Go side is
Node-free (the locked toolchain decision). The harness tests the **committed**
`web/dist/` bundle the binary embeds; rebuild it with `make web-build` after
changing `web/src/`.

## Artifacts

The harness is assertion-gated, so it needs no committed artifacts. Per-run
Playwright outputs (`web/test-results/`, `web/playwright-report/`,
`web/blob-report/`, `web/selftest/.last-run.json`) are git-ignored; a failing run
retains a trace under `web/test-results/` for local debugging.
