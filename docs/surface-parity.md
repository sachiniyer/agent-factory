# Surface parity

Agent Factory ships three clients over one daemon: the TUI, the web UI, and the
CLI. Since [#960](https://github.com/sachiniyer/agent-factory/issues/960) the
daemon is the single writer, and since
[#1592](https://github.com/sachiniyer/agent-factory/issues/1592) it is the
central orchestrator with all three clients as thin clients over one API. The
promise that follows is that they are the **same product**.

In practice capabilities drift. One surface gains a verb, an option, or a
button; the others silently fall behind; nobody notices until a user hits the
missing thing. That is a bug class, not a one-off — so it has a detector.

## The two pieces

**`parity/inventory.json`** — every user-facing capability and which surfaces
expose it, with a code pointer per cell and a verdict per row.

**`parity/parity_test.go`** — derives the real surfaces from code and fails when
they disagree with the inventory. Run it with `go test ./parity/`.

The check is deliberately code-derived on all four halves:

| Surface | Derived from |
|---|---|
| CLI | `commands.NewRootCommand()` — the real cobra tree, walked for verbs and flags |
| API | `daemon.HTTPRoutes()` — the same table that builds the live mux, with request fields reflected off the wire structs |
| TUI | `keys.EffectiveBindings(nil)` — the canonical binding table |
| Web | the `af<T>(method, body, token)` call sites in `web/src/api.ts`, the single chokepoint through which the SPA reaches the daemon |

A hand-maintained table would drift, which is the failure this exists to catch.
So the only hand-maintained part is the **verdict** — the judgment a machine
cannot make. Everything else is read out of the code at test time.

## What the check enforces

- Every CLI verb, daemon route, TUI binding, and web RPC has an inventory entry.
  Adding one without an entry fails the build.
- Nothing is inventoried that no longer exists, so the table cannot advertise a
  capability af has lost.
- Every `CreateSession` field the daemon accepts is either sent by the web or
  declared a known gap. This is the **option dimension**: "create a session" is
  at parity as a verb while the three surfaces accept different options, and
  that is exactly where the reported remote-instance gap lives.
- Quality bar: a surface marked `yes`/`partial` must cite code, a `deliberate`
  verdict must explain itself, and a `real-gap` must name an issue.

## Verdicts

| Verdict | Meaning |
|---|---|
| `parity` | every applicable surface exposes it |
| `real-gap` | a surface should have it and does not — `issue` names the ticket |
| `deliberate` | the surface legitimately cannot or should not; `notes` says why, so it is never re-reported as a gap |
| `unclear` | needs an owner decision; not filed |

`n/a` as a *status* means the surface has no analogue by nature — `af doctor`
diagnoses a broken install, including the case where the daemon is down and
neither UI can run.

## What is deliberately NOT held to parity

Not every divergence is a bug, and recording that is half the value here.

- **Navigation and layout chrome.** Each UI navigates its own layout
  idiomatically; the web has mouse drag-and-drop the TUI cannot express, the TUI
  has keyboard splits the web reaches via Alt-chords. A CLI has nothing to
  navigate. Key-for-key parity is a non-goal.
- **Scripting primitives.** `af sessions watch` blocks until a session goes
  idle; both UIs show liveness continuously, so there is nothing to block on.
  Same for `get`, `whoami`, and `--all` broadcast.
- **Host lifecycle.** Daemon install/restart, `upgrade`, `reset`, `doctor`, and
  `token` act on the host or the daemon itself. The web is served *by* the
  daemon — a button to stop it would kill the page — and the token is the
  credential the web needs before it can talk at all.
- **Internal plumbing.** `af gen-docs` (hidden), `af agent-server`
  (daemon-consumed), `af --daemon` (hidden flag).

Each of these is recorded in the inventory with a `deliberate` verdict and a
reason, so the next audit does not re-report it.

## When the check fails

The failure names the item and the fix: add it to `parity/inventory.json`, map it
in the `ledger`, and give its capability a status per surface with a code
pointer and a verdict. If the other surfaces deliberately will not have it, say
so in `notes` — that records the decision.

Do not silence the check. A new capability on one surface is precisely the
moment to decide what the other two do about it, which is the whole point.

## Updating the parser

If `web/src/api.ts` is restructured so its calls no longer match
`webCallRe`, the check fails loudly via `minWebCalls` rather than quietly
concluding the web calls nothing. Fix the parser in `parity/derive_test.go`;
never lower the floor to make it pass.
