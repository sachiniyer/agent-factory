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

## Drift is bidirectional — do not assume the web is the laggard

The obvious story is that the TUI is the mature surface and the web is playing
catch-up. **That story is wrong, and believing it will make you read this table
incorrectly.**

Capabilities land wherever the implementer happened to be standing. The first
audit (#1937) found gaps pointing in every direction:

- **The web is ahead of both others** on per-session auto-yes: it is the *only*
  surface that can set it per session. The TUI inherits one process-wide `-y`
  flag for every session it creates; the CLI reads the repo config and
  `af sessions create --autoyes` is an unknown-flag error.
- **The TUI is behind the other two** on create-time prompts: the web modal and
  `af sessions create --prompt` both send one, and the TUI cannot
  ([#1936](https://github.com/sachiniyer/agent-factory/issues/1936)). The
  `Prompt` field is plumbed end-to-end to the daemon
  (`app/session_control.go:106`) and its only construction site never populates
  it — the plumbing is finished and simply never fed.
- **The CLI is behind both UIs** on limit-retry: resuming a usage-limit-blocked
  session is TUI-only, and the CLI has no verb for it at all
  ([#1934](https://github.com/sachiniyer/agent-factory/issues/1934)).
- **Only the CLI** can choose a backend per session
  ([#1933](https://github.com/sachiniyer/agent-factory/issues/1933)).

The sharpest way to hold this: on `CreateSession`, **no surface is a superset of
another**. All three accept different subsets of the same nine-field request.

| Create option | TUI | Web | CLI |
|---|---|---|---|
| Title, program | yes | yes | yes |
| Initial prompt | **no** | yes | yes |
| Auto-yes per session | **no** | **yes** | **no** |
| Backend (docker/ssh/hook) | **no** | **no** | yes |
| Force-remote (hook) | partial | **no** | yes |
| In-place (`--here`) | **no** | **no** | yes |

So when this check fails, the question is never "does the web need to catch up?"
It is "which surfaces should have this, and which deliberately should not?" —
asked in all three directions.

## The two pieces

**`parity/inventory.json`** — every user-facing capability and which surfaces
expose it, with a code pointer per cell and a verdict per row.

**`parity/parity_test.go`** — derives the real surfaces from code and fails when
they disagree with the inventory. Run it with `go test ./parity/`.

The check is deliberately code-derived on all four halves:

| Surface | Derived from |
|---|---|
| CLI | `commands.NewRootCommand()` — the real cobra tree, walked for verbs and flags **after `initCobraDefaults` finishes building it** (see below) |
| API | `daemon.HTTPRoutes()` — the same table that builds the live mux, with request fields reflected off the wire structs |
| TUI | `keys.EffectiveBindings(nil)` — the canonical binding table |
| Web | the `af<T>(method, body, token)` call sites in `web/src/api.ts`, the single chokepoint through which the SPA reaches the daemon |

A hand-maintained table would drift, which is the failure this exists to catch.
So the only hand-maintained part is the **verdict** — the judgment a machine
cannot make. Everything else is read out of the code at test time.

## Two levels: verbs and options

A verb-level check alone is not enough, and that is not a theory — it is how
[#1948](https://github.com/sachiniyer/agent-factory/issues/1948) got missed.
`af sessions preview` exists, so "preview a session" looked like parity. But
`PreviewRequest` carries `Tab`, `TabID`, and `Full`; the TUI sends all three and
the CLI sends none, so the CLI can only ever see tab 0. The verb was present and
the **options** were not. It was found by someone using the product, not by this
check.

So the check works at two levels:

| Level | Question | Derived from |
|---|---|---|
| **Verb** | can this surface do X at all? | the cobra tree, the route catalog, the binding table, the web's RPC call sites |
| **Option** | can it do X with the options the daemon accepts? | CLI flags off the cobra tree; the wire structs by reflection, vs the AST of `api/`+`app/` and the web's request bodies |

The option level is where the interesting gaps live, because they hide behind a
verb that looks present. Three of the same shape so far — a field the daemon
accepts that a surface never sends:

- [#1933](https://github.com/sachiniyer/agent-factory/issues/1933) — the TUI
  never sets `CreateSessionRequest.Backend`
- [#1948](https://github.com/sachiniyer/agent-factory/issues/1948) — the CLI
  never sets `PreviewRequest.Tab` / `TabID` / `Full`

Every field a surface does not send must be declared in `field_coverage` as
either `{"gap": "<capability-id>"}` (a tracked divergence) or `{"ok": "<reason>"}`
(its absence is correct, and why). The field lists are derived, so **a new field
on any request forces a decision on every surface that builds it** — nobody has
to remember.

## What the check enforces

- Every CLI verb, **CLI flag**, daemon route, TUI binding, and web RPC has an
  inventory entry. Adding one without an entry fails the build. Flags count
  because a flag is a capability — `af sessions create` existing says nothing
  about whether it can pass `--backend`.
- Nothing is inventoried that no longer exists, so the table cannot advertise a
  capability af has lost.
- Every field of every audited request is either reachable from a surface or
  declared, in both directions — a field a surface has quietly *started* sending
  also fails, so a fixed gap cannot keep being described as broken.
- **The table cannot contradict itself.** A ledger mapping proves a surface
  reaches a capability, so the row cannot still say that surface is `no`; and a
  verdict cannot say `parity` while a surface is missing it, or `real-gap` once
  every surface has it. Otherwise "add the ledger entry" would be enough to make
  the check pass while the table went stale — which would make the inventory
  lie in exactly the way it exists to prevent.
- Quality bar: a surface marked `yes`/`partial` must cite code, a `deliberate`
  verdict must explain itself, and a `real-gap` must name an issue.

## Is the derivation itself honest?

This is the question that matters most, and it is not answered by the checks
above. They compare the derivation to the inventory; none of them asks whether
the derivation **sees anything**.

A derivation with a hole does not fail loudly. It silently under-reports and the
suite goes green — which is *worse than having no parity check*, because the
green gets trusted. A detector that manufactures confidence is the failure mode
this package exists to prevent, so it must not be one.

`parity/honesty_test.go` therefore pins the derivation against gaps we already
know are real, filed, and field-level — as fixtures, not aspirations:

| Fixture | Derivation path it proves |
|---|---|
| cobra's lazy surface — `af completion bash`, `af help`, `--help`, `--version` | the tree is walked **after** cobra finishes building it |
| [#1933](https://github.com/sachiniyer/agent-factory/issues/1933) — the web never sends `CreateSession.backend` | the web request-body parser |
| #1933 (TUI half) — `sessionStartRequest` has no `Backend` | the Go AST walk |
| [#1948](https://github.com/sachiniyer/agent-factory/issues/1948) — the CLI never sets `Preview.Tab/TabID/Full` | the AST **on an internal route**, invisible to the public catalog |
| [#1935](https://github.com/sachiniyer/agent-factory/issues/1935) — the web's `TaskUpdate` omits `project_path` | nested recursion **behind a wrapper route**, plus the TS-interface read and the CLI's field-by-field assignment walk |

Each fixture asserts in **both** directions: that the known gap is seen, *and*
that a field the surface demonstrably does send is not reported as a gap. A
parser returning nothing would satisfy the first half alone; it cannot satisfy
both.

If a gap is ever fixed, its fixture fails and says so. That is correct — the
fixture is retired deliberately, not silently.

The fixtures are verified by blinding each path and watching them fail: drop the
nested recursion, remove the wrapper packages, break the TS parser, or break the
call-body regex, and the matching fixture reports *"the parser is blind"* rather
than passing.

## Known blind spots

Stated so nobody mistakes a passing check for total coverage. Note which way each
one fails — **over-reporting forces a decision, under-reporting hides one**, and
only the second is dangerous.

- **Reachable ≠ user-settable** *(under-reports — the dangerous direction)*. The
  AST proves a construction site *sets* a field, not that a user can *choose* its
  value. `session.create.opt.prompt` (#1936) is exactly this trap:
  `app/session_control.go:106` sets `Prompt`, so the field reads as covered while
  the TUI's naming flow never populates it upstream. A field-level pass does not
  excuse reading the flow.
- **Internal routes are not in the *verb*-level route check** *(under-reports)*.
  `daemon.HTTPRoutes()` is the public catalog; `Preview` and `ResumeFromLimit`
  live in `internalHTTPRoutes`. Both are covered at the **option** level via
  `auditedRequests` — which is what catches #1948 — but a new internal route that
  no surface calls would not trip the route check.
- **A nested payload built by a shared helper** *(over-reports — safe)*. The TUI
  patches a task via `task.DiffTask`, in a package every surface shares, so the
  walk cannot attribute it. Those fields are reported unreached and must be
  declared `ok` with evidence: a decision is forced rather than skipped.
- **Composite literals and simple var-assignment only.** The walk reads
  `T{Field: …}` and `var x T; x.Field = …`. A request built some third way would
  read as setting nothing — over-reporting, and `minGoLiterals` trips if the
  surfaces move wholesale to another style.

## Two things that must stay true

Both were once false, both reported green, and both are now fixtures:

**Walk the tree only after cobra has finished building it.** cobra adds
`completion`, `help`, `--help` and `--version` lazily inside `Execute()`, so a
walk of the freshly-constructed tree omits commands users can actually run.
`initCobraDefaults` runs them first; `TestDerivationSeesLazyCobraSurface` fails
if it is ever removed.

**Declarations are validated from both ends.** Walking only the *derived*
requests catches a surface that is missing a declaration, but not a surface that
**drops** a request another still uses — the CLI dropping `PreviewRequest` while
the TUI keeps it would simply vanish from the derived set, leaving its
declarations to rot while the suite stayed green.
`TestFieldCoverageDeclarationsAreLive` walks the declarations the other way, so
every declared `(type, surface)` must still be something that surface really
does, and every declared field must still exist on the wire struct.

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
