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

## Four levels: verbs, options, enums, identifiers

A verb-level check alone is not enough, and that is not a theory — it is how
[#1948](https://github.com/sachiniyer/agent-factory/issues/1948) got missed.
`af sessions preview` exists, so "preview a session" looked like parity. But
`PreviewRequest` carries `Tab`, `TabID`, and `Full`; the TUI sends all three and
the CLI sends none, so the CLI can only ever see tab 0. The verb was present and
the **options** were not. It was found by someone using the product, not by this
check.

So the check works at four levels, each blind to the one below it:

| Level | Question | Derived from |
|---|---|---|
| **Verb** | can this surface do X at all? | the cobra tree, the route catalog, the binding table, the web's RPC call sites |
| **Option** | can it do X with the options the daemon accepts? | CLI flags off the cobra tree; the wire structs by reflection, vs the AST of `api/`+`app/` and the web's request bodies |
| **Enum** | does it offer the same VALUES for those options? | the canonical Go enum, vs what a surface actually lists |
| **Identifier** | is the string we SHOW the string we ACCEPT? | the display rule (`session.TabLabel`) vs the resolver (`session.TabMatches`) |

The option level is where the interesting gaps live, because they hide behind a
verb that looks present. Three of the same shape so far — a field the daemon
accepts that a surface never sends:

- [#1933](https://github.com/sachiniyer/agent-factory/issues/1933) — the TUI
  never sets `CreateSessionRequest.Backend`
- [#1948](https://github.com/sachiniyer/agent-factory/issues/1948) — the CLI
  never sets `PreviewRequest.Tab` / `TabID` / `Full`

The **enum** level is the newest and the easiest to miss, because the other two
both pass while it drifts. `web/src/modals.ts:173` hardcodes a copy of
`tmux.SupportedPrograms` ([#1970](https://github.com/sachiniyer/agent-factory/issues/1970)):
the web *does* send `program`, so field coverage calls it covered, and adding a
sixth agent server-side would leave the web silently unable to offer it with the
whole suite green. A surface serving a stale copy of something the daemon owns is
the #1933 shape one level down, so it gets the same answer — derive both sides
and compare, rather than trusting a copy to stay in step. The structural fix is
to SERVE the enum (the `ListBackends` pattern), at which point the check is
deleted and the row flips to `parity`.

Every field a surface does not send must be declared in `field_coverage` as
either `{"gap": "<capability-id>"}` (a tracked divergence) or `{"ok": "<reason>"}`
(its absence is correct, and why). The field lists are derived, so **a new field
on any request forces a decision on every surface that builds it** — nobody has
to remember.

## The identifier axis: what we show vs what we take

The nastiest of the four, because it is invisible until someone types it
([#1984](https://github.com/sachiniyer/agent-factory/issues/1984)):

```
$ af sessions tab-delete alpha --name Terminal
session "alpha" has no tab named "Terminal"      # the TUI tab bar says "Terminal"
$ af sessions tab-delete alpha --name shell
# works
```

The TUI rendered a **label** and the CLI demanded a **name**, so the error
asserted a tab was absent while the user could see it on screen — and left them
to discover the mapping. One concept, two representations: the same disease as
#1972 and #1970.

The rule now lives beside the `Tab` type, not in the TUI, because **a label a
user can read is an identifier they will type**:

- `session.TabLabel` — the one definition of what a user SEES. `ui/tree` delegates
  to it, so display cannot drift from what is accepted.
- `session.TabMatches` — accepts the canonical name **or** the label. Additive:
  `--name shell` keeps working for every script, nothing is renamed, no display
  changes.
- `session.TabIdentifiers` — renders a tab as both spellings, so *"no tab named
  X"* lists the valid options instead of asserting an absence. That is worth more
  than the alias: it still fires for a real typo, but turns a dead end into a fix.

The label is an **input** spelling only — the resolver canonicalises before
returning, or `tab-delete --name Terminal` would answer `{"name":"Terminal"}`, a
string that is not any tab's identity.

`TestDisplayedTabIdentifiersAreAccepted` enforces the invariant for every
`TabKind`, including kinds with no UI yet, so a new kind cannot ship a label its
own CLI rejects.

## The CLI-vs-CLI axis: argument shape

Parity is not only between surfaces. Within one noun group, does the same
CONCEPT take the same SHAPE across sibling verbs? It is the same failure — a user
who learned one verb cannot predict its sibling — and it was found the same way,
by someone driving the CLI and getting stuck.

`af sessions create --prompt X` takes the prompt as a flag; `af sessions
send-prompt <title> <prompt>` took it positionally and hard-errored with
**"unknown flag: --prompt"** — naming the flag as wrong without mentioning that
the positional is what it wants. Two siblings, one concept, two shapes.

Both halves are derived: flags from the cobra tree, positionals from each
command's `Use` line. The check is a non-empty **intersection** of accepted
forms, not identical sets — a verb that accepts both forms is compatible with
either neighbour, which is why the fix is always **additive**: teach one verb the
other's form and keep the old one. `send-prompt` now accepts `--prompt` as an
alias and its positional still works, so nothing breaks and the shapes reconcile.

The one hand-maintained part is `synonyms`, and it is keyed **per verb** on
purpose: `--name` on `create` means the session title, but `--name` on
`tab-create <title> --name <tabname>` means the tab. A global `name → title` rule
invents a divergence that is not there.

Still open and declared, not silent:
[#1972](https://github.com/sachiniyer/agent-factory/issues/1972) — `af sessions
create --name` is the only sessions verb taking the title as a flag, and the only
one calling it *name* rather than *title*, while ten siblings take `<title>`
positionally.

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
| [#1933](https://github.com/sachiniyer/agent-factory/issues/1933) — the web now **does** send `CreateSession.backend` (#1968 landed), via a `const body` variable | the web body parser, incl. variable resolution |
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

## The audit knows its own denominator

The question that comes before "do the surfaces agree?" is **what did the audit
actually look at?**

An audit that under-covers does not merely miss gaps. It asserts parity over
surfaces it never opened, and it is believed, because it is a green check — which
turns *"we have a gap"* into *"we have a gap and a test says we do not"*. Every
hole found in this package so far had exactly that shape: a walk that ran before
cobra finished building the tree, a web scan that skipped subdirectories, an enum
check that read one of two selectors, a request the analyzer dropped instead of
reporting.

So the audit states its denominator, and **fails closed**:

```
go test ./parity/ -v -run TestAuditCoverageReport
```

```
=== surface-parity audit coverage ===
  cli.arg-concepts                 ? go.cli.request-sites   ?
  cli.commands                     55 go.tui.files           41
  cli.flags                       138 go.tui.request-sites   ?
  cli.noun-groups                   ? inventory.capabilities 65
  cli.verbs                        47 tui.bindings           44
  daemon.audited-request-types     ? web.enum-sites          ?
  daemon.public-routes             ? web.rpcs               16
  go.cli.files                      10 web.source-files       ?
  verdicts:  parity=20  deliberate=20  real-gap=10  unclear=15
  SKIPPED: none — every surface above was read
```

Three rules make that number honest:

1. **Anything not covered is a finding, not a pass.** A file that will not parse,
   a construct the analyzer cannot read, a directory not entered — each is
   reported with its reason and fails the run.
2. **Unanalyzable is never a shrug.** A request built in a shape the walk cannot
   read used to vanish from the derived set, taking its declarations with it.
   Now it is named: *"cli builds PreviewRequest in a shape this analyzer cannot
   read … its field coverage is therefore UNVERIFIED."*
3. **The denominator itself has floors.** If `cli.verbs` or `web.rpcs` collapses,
   the run fails — a shrinking denominator makes every parity claim above it
   meaningless, and it is exactly what a silently-blinded derivation looks like.

## The web body parser reads variables, not just literals

The web builds some request bodies as a variable — `const body = {…}; body.x = y;
af("Method", body, token)` — which #1968 introduced for CreateSession's optional
`backend`. A parser that only reads an inline literal after the method name goes
BLIND on that shape and reports the body as empty, which is under-coverage: it
would have said the web sends nothing for CreateSession and reported false parity
over every create option.

So the parser resolves the body argument: an inline `{…}` literal, or a plain
variable traced to its nearest preceding `const|let|var … = {…}` plus any
`body.field = …` additions. Anything else at the body position — a function call,
a spread — is reported UNANALYZABLE and fails the RPC's coverage, never dropped.
This is finding (4)'s web analogue and it is LIVE, not latent: #1968's
`body.backend = …` is exactly it.

## Reach is derived from values, not types

For a wrapper payload the obvious move is to read its TypeScript interface. That
is wrong in the dangerous direction: an interface says what is **possible**, and
a client that never sends a field still passes.

`TaskUpdate` declares seven options; the single call site
(`web/src/index.ts:862`) sends `{ enabled }`. Reading the type credited the web
with six options it cannot reach and reported parity over them. Reading the
values it actually passes reports them as the gaps they are (#1935).

## "Could not check" is a third answer

Worth stating because this package learned it twice, independently, and it
generalises past parity.

The `ListBackends` contract (#1968) returns three outcomes, not two:
**available**, **unavailable + reason**, and **unknown + reason** — where
*unknown* means the daemon could not check (a repo config that would not parse),
which is a different answer from yes and from no. Collapsing it into either one
invents a fact. The same PR found the related trap: **configured is not
available** — the hook backend was reported available without checking its
commands were runnable.

This audit has exactly that shape and resolves it the same way. A request the
analyzer cannot read is not "reached" and not "unreached" — it is **unanalyzable**,
and it is a finding:

> `cli builds PreviewRequest in a shape this analyzer cannot read … its field
> coverage is therefore UNVERIFIED`

Before that existed, an unreadable construct simply vanished from the derived
set — *unknown* collapsing into *fine*, which is how a checker reports green over
code it never understood. If you add a dimension here, give it three answers.

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
