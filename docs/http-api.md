# The HTTP API

For anyone integrating with the daemon over HTTP rather than the CLI. After this
page you will know the transport and socket path, how requests are authenticated,
the response envelope every endpoint uses, and where the generated endpoint table
lives.

The Agent Factory daemon exposes a small JSON API — a 1:1 mirror of the session
and task operations the `af` CLI performs — over a **local Unix socket**. It is
the same daemon core (`#960` single-writer model) the TUI and `af sessions` /
`af tasks` commands already drive, reached over HTTP instead of the internal
`net/rpc` control socket, so the two surfaces can never diverge.

This page is a hand-written guide to the transport, auth, and envelope; the
enumerated endpoint table is generated from the route catalog (see
[HTTP API reference](reference/api.md)). There is deliberately **no
OpenAPI/Swagger document** in v1. To discover the surface from the command line
without reading this file, run:

```bash
af api          # human-readable catalog: socket path, auth model, every endpoint + a curl example
af api --json   # the same catalog as JSON, wrapped in the shared {data,error} envelope
```

`af api` is read-only and local: it prints the catalog and the resolved socket
path but never dials the socket or starts the daemon. Its catalog is derived
from the daemon's actual route table, so it always matches what the server
serves.

## Transport & socket path

The API is served over a dedicated Unix socket, **not** a TCP port:

```
$AGENT_FACTORY_HOME/daemon-http.sock
```

`$AGENT_FACTORY_HOME` is where Agent Factory keeps its state. It resolves as:

1. the `AGENT_FACTORY_HOME` environment variable, if set (with a leading `~` /
   `~/` expanded); otherwise
2. the default config dir, `~/.agent-factory`.

So on a default install the socket is `~/.agent-factory/daemon-http.sock`. `af
api` prints the resolved path for your environment.

The socket is created when the daemon starts (on demand — a bare `af` launch
starts it, or via an autostart unit — see
[tasks.md](tasks.md#daemon-lifecycle)). If the socket does not exist, the daemon
is not running.

## Authentication

There is **no token and no TCP port**. Authentication is the filesystem:

- The socket is a **Unix domain socket**, reachable only from the local host —
  never the network.
- It is created with **`0600` permissions** (owner read/write only), so only
  the user who owns the daemon process can connect. Group and other have no
  access.

This matches the model of the daemon's internal control socket. It is a
single-user, local-only API by design: anyone who can read the socket already
runs as your user and could drive `af` directly, so no additional secret buys
anything. Do not proxy this socket to a network interface.

!!! note "Reaching the daemon from another machine"

    To drive the daemon from a **different host** — a remote TUI, or the
    browser web client — don't proxy this socket. SSH to the host and run `af`
    there, or expose the HTTP+token TCP listener to the network (it's on by
    default on loopback; point `network.listen_addr` at a routable host:port). The
    listener is plain HTTP — front it with a TLS-terminating proxy or a private
    network. Both are covered in [Remote daemon access](remote-http-auth.md).

## Response envelope

Every response — success or failure, on every endpoint — is the same
`{data, error}` JSON envelope shape the CLI's `--json` flag emits. HTTP
errors additionally carry the daemon outcome markers described below.

A **success** carries the payload under `data` with `error: null`:

```json
{
  "data": { "ok": true },
  "error": null
}
```

A **failure** sets `data: null` and populates `error.message`:

```json
{
  "data": null,
  "error": {
    "message": "agent-factory daemon is starting (restoring sessions); retry shortly",
    "daemon_rejected": true
  }
}
```

Both members always serialize (no `omitempty`), so a consumer can branch on
`error === null` without a presence check. Every response sets
`Content-Type: application/json`.

Ordinary errors emitted by the daemon's HTTP handler include
`error.daemon_rejected: true`, an optional boolean identifying a daemon
rejection. Only the boolean `true` counts; strings and other truthy values do not. A JSON `error` body or a `500`/`503` status alone does not establish
that provenance: a reverse proxy may return its own error after forwarding a
mutation that the daemon accepted. Clients must treat an unmarked `5xx`, a lost
reply, or an unfamiliar error envelope as an uncertain outcome, check the
session's state, and avoid automatically retrying the mutation. The web client
also treats `502` and `504` as uncertain even if a marker is present, since the
daemon does not emit those gateway statuses. The marker is a
protocol convention, not authentication against a proxy that deliberately copies
it.

`error.code: "proposal_conflict"` is the one failure that also carries `data`:
a `409` from `SaveProposalDoc` puts the three versions of the doc there so the
editor can show the conflict. Every other failure keeps `data: null`.

`error.code: "mutation_committed"` is the existing, separate outcome marker for a
mutation that reached durable storage before a follow-up failed. It takes
precedence over `daemon_rejected` if both occur: surface the detail without
retrying the completed mutation. The daemon omits `daemon_rejected` from these
committed-outcome envelopes. CLI `--json` ordinary errors retain their existing
shape without the HTTP provenance marker.

`error.code: "project_rebound"` (status `409`, with `daemon_rejected: true`) is
the refusal both rebind routes give a compare-and-set rebind — see
[Rebinding a project](#rebinding-a-project). Nothing was written, so it is a
definitive refusal, not an uncertain outcome.

## Status codes

| Status | Meaning |
|--------|---------|
| `200 OK` | Success. `data` holds the response payload; `error` is `null`. |
| `400 Bad Request` | The request body was not valid JSON, or it carried a field this daemon does not recognize (see [Unknown fields](#unknown-fields)). |
| `404 Not Found` | Unknown route (e.g. `POST /v1/Nope`). |
| `405 Method Not Allowed` | Wrong verb — RPC routes are POST-only; `/v1/health` is GET-only. |
| `409 Conflict` | `POST /v1/RebindProject` and `POST /v1/RebindProjectGuarded`: part of the request's expected pair no longer matches the project's record — `expected_root` against the recorded root, `expected_checkout_id` against the recorded checkout marker (which a same-path reclone or repair rotates) — because another rebind or re-clone landed first. `error.code` is `project_rebound`. See [Rebinding a project](#rebinding-a-project). |
| `409 Conflict` | `SaveProposalDoc`: the save conflicts with a newer edit and nothing was committed. `error.code` is `proposal_conflict`, and `data` is not `null` — it holds `{ours, theirs, base, head}` (see `docs/dev/proposals/proposal-mode/api.md`). |
| `413 Request Entity Too Large` | The body exceeded the 16 MiB cap. The request is **rejected, never truncated-then-processed** — the daemon is never reached. |
| `500 Internal Server Error` | The handler ran but returned an error (validation failure, not-found session, a disabled task refused by `TriggerTask`, etc.). `error.message` carries the detail. |

The status maps the *transport* outcome; a business-logic failure (e.g. "session
not found") is a `500` with a descriptive `error.message`, not a bare status.

## Unknown fields

How the daemon treats a request field it does not recognize depends on whether the
request identifies itself as an af client, because the same unknown field means
opposite things to the two kinds of caller.

**Hand-authored requests** (curl, `af api`, your own scripts) are decoded
**strictly**: an unrecognized key is a `400`. This is deliberate. An unknown key is
almost always a typo, and dropping it silently can *widen* what an RPC does — a
typo'd `repo_idd` leaves `repo_id` empty, which turns a one-repo `Snapshot` into an
all-repo `Snapshot`. Failing loudly is the safer answer:

```console
$ curl --unix-socket ~/.agent-factory/daemon-http.sock \
    -X POST http://af/v1/Snapshot -d '{"repo_idd":"typo"}'
{"data":null,"error":{"message":"malformed JSON request body: json: unknown field \"repo_idd\"","daemon_rejected":true}}
```

**Requests carrying the `X-AF-Client-Version` header** — which the `af` TUI and CLI
send automatically — are decoded **leniently**: unrecognized keys are ignored. The
daemon is upgraded independently of its clients, so a client newer than the daemon
legitimately sends fields the daemon has never heard of. Fields are additive and
never renamed, so ignoring an unknown one is always safe, whereas rejecting it
would turn every version skew into a hard failure.

The bundled web UI sends the header too and is decoded leniently. Its service
worker and an already-open tab can outlive a daemon restart, so
`RestoreSession.expected_daemon_boot_id` is sent only after a Snapshot supplies a
`boot_id`; a current daemon refuses a mismatch before restore admission, while a
legacy Snapshot keeps the older request shape.

Setting the header by hand is supported but simply opts you out of typo checking —
it is not an authentication or trust boundary.

Note this lenient decoding only exists in daemons that ship with it. If a **newer
client** talks to an **older daemon**, that daemon still rejects the newer field,
and af clients report it as a version skew:

```text
daemon is out of date and rejected the "tab_id" field this client sent —
restart it with `af daemon restart`
```

Restarting the daemon (so it matches the `af` binary on disk) is the fix.

## Warm-up behavior

The daemon binds its sockets **before** it finishes restoring sessions. During that window:

- `GET /v1/health` answers immediately (it is a pure liveness probe) — it does
  not wait for the restore.
- State-dependent routes (session and task RPCs) return an error envelope with
  the message `agent-factory daemon is starting (restoring sessions); retry
  shortly`. Treat it as **retryable**: the daemon is alive; the same request
  succeeds once the restore completes.

## Endpoints

The full, enumerated route table — every method, path, and request-body field —
is generated from the daemon's route catalog and lives in the
**[HTTP API reference](reference/api.md)**. It cannot drift from the server:
the same catalog backs the mux, the `af api` command, and that page. Request-body
fields are the JSON keys of each RPC request struct; a route with no listed
fields accepts an empty body (`-d '{}'` or no `-d` at all).

`GET /v1/health` is the one non-POST route: a liveness probe (alias for the
internal `Ping` RPC) that answers even while the daemon is restoring sessions,
with response `data` of `{ "ok": true }`.

### Rebinding a project

`POST /v1/RebindProject` moves a registered project's stable `id` to the
checkout at `path`. It accepts an optional observed pair — `expected_root`,
the root the caller last saw the project bound to, and `expected_checkout_id`,
the checkout marker behind it (the `root` and `checkout_id` from
`POST /v1/ListProjects`):

```bash
curl --unix-socket ~/.agent-factory/daemon-http.sock \
    -X POST http://af/v1/RebindProject -d '{"id":"prj_…","path":"/home/me/src/repo","expected_root":"/home/me/old/repo","expected_checkout_id":"chk_…"}'
```

With either expected field set, the rebind is a compare-and-set on the pair.
The daemon applies it only if the registry still records that root *and* that
checkout id when the registry lock is taken — the checkout half is what sees a
reclone or repair at the same path, which keeps the root spelling but mints a
new marker — so of two rebinds made from the same observed pair, exactly one
applies. The other gets `409` with `error.code: "project_rebound"`, and its
message names the root and checkout the project is bound to now. Re-read the
registry, show the user the current pair, and retry with it if they still want
the move. A request whose `path` is already the recorded root and whose
checkout still matches also succeeds, so a retried request that already landed
is not refused.

`expected_root` and `expected_checkout_id` are optional. A request that omits
both is applied whatever the project is bound to, so the last writer wins —
what clients written before the fields existed send, never refused for the
omission. The TUI and web client send the pair they displayed. `af projects
rebind` does not; it rebinds unconditionally, as before.

A daemon that predates the fields treats them like any unknown fields (see
[Unknown fields](#unknown-fields)). It rejects a hand-authored request with
`400`. For a request with `X-AF-Client-Version`, such as the TUI's, it ignores
them and applies the rebind without the check — which is why a guarded client
sends the pair to `POST /v1/RebindProjectGuarded` instead: that route exists
only on daemons that implement the precondition, so a daemon that predates it
answers its marked 404 and the client surfaces an upgrade refusal rather than
silently downgrading to last writer wins. The route is the capability check —
atomic with the mutation, where a `GET /v1/health` probe answered by one
daemon could never vouch for the sibling a proxy might route the mutation to.
`RebindProjectGuarded` refuses a request that omits both expected fields;
last-writer-wins stays on `POST /v1/RebindProject`.

**Not in the catalog.** The generated table is `af api`'s discovery surface — the
client-facing session and task RPCs — so the daemon serves several routes it does
not list. They fall into three groups:

- **Internal JSON-envelope RPCs**, same shape as the cataloged ones but deliberately
  unadvertised as public API: `POST /v1/Preview` (the TUI's render path),
  `POST /v1/PauseStatusPoll` and `POST /v1/ResumeStatusPoll` (attach coordination),
  and `POST /v1/HandoffSessionV2` (the version-bound account-aware handoff
  transport).
- **Non-RPC surfaces**, which speak something other than the envelope: the WebSocket
  planes `GET /v1/sessions/{id}/stream`, `GET /v1/sessions/{id}/stream-info` and
  `GET /v1/events`; the config-assistant trio on `/v1/config-assistant` and the
  account-login stream `GET /v1/account-login/stream` (bare-session PTY WebSockets
  that serve no Instance); `GET /v1/auth-info`, the tokenless probe that reports
  whether this peer must present a token (see
  [remote daemon access](remote-http-auth.md)); `GET /v1/preview-auth`; `POST /v1/editor-origin`, which mints a session's editor-origin bootstrap URL for a remote viewer (see [Web UI](web.md#per-session-editor-origins-for-remote-viewers)); and the
  web-tab reverse proxy under `/v1/webtab/`.
- **The profiling endpoint**, `GET /v1/debug/pprof/{profile}`, when it is switched
  on. It is **off by default**, is served on **this socket only** and never on
  `network.listen_addr`, and returns the ordinary 404 unknown-route envelope while
  it is off. Its handlers are `net/http/pprof`'s, mounted with the same `GET`-only
  method qualification the stdlib uses. See
  [Profiling the daemon](dev/daemon-memory.md#profiling-the-daemon).

**Response shapes.** These are not part of the generated request-field catalog,
so they are documented here. `CreateSession` returns `{ "instance": <session> }`;
`Snapshot` returns `{ "instances": [<session>…] }`;
`ArchiveSession` returns `{ "ok": true, "archived_path": "…" }`;
`RestoreArchived` returns `{ "ok": true, "worktree_path": "…" }`;
`SendPrompt` returns `{ "ok": true, "status": "delivered" | "not-delivered" | "sent-unverified" | "could-not-confirm" }`;
`sent-unverified` means the paste and Enter were accepted while a readable pane
did not render exact prompt content; `could-not-confirm` means the pane observer
itself was unavailable. Neither status claims delivery.
`PruneSessions` returns
`{ "ok": true, "older_than": "<duration>", "archived_before": "<rfc3339>", "candidates": [<entry>…], "skipped"?: [<entry>…], "reclaimable_bytes": <int>, "warnings"?: [<string>…] }`:
without `"apply"` it is the read-only dry run for the archived-session
reclaim — it lists in `candidates` each archived session a reclaim would
remove — `{ "id"?, "title", "repo_id", "branch", "archived_at",
"reclaimable_bytes" }` — while af writes nothing: no deletion, no record
update, no git write. With `"apply": true` it deletes exactly the
worktrees that same verdict lists — the branch is always kept and each
deleted row stays as a tombstone marked pruned — and the response gains
`"applied": true`, `"pruned": [<entry>…]` (the rows whose worktree was
deleted), `"incomplete": [<entry>…]` (deletion started but the record is
not final; a re-run finishes them), `"reclaimed_estimate_bytes"` (an
"up to" sum of the pruned rows' allocated-size estimates, never measured
free space), and `"free_bytes_before"`/`"free_bytes_after"`/
`"free_bytes_known"` (the measured free space on the archive
filesystem(s) around the run, so reflinks and shared extents surface as a
visible gap rather than silent credit). `"only": [<session id>…]` binds an
apply to the plan a caller already saw and confirmed — the CLI sends it
after the operator approves the dry-run listing — and is refused on a dry
run. `skipped` entries (`{ "id"?, "title", "repo_id", "reason" }`) were
evaluated and refused; one marked `"interrupted": true` had an earlier
apply not finish and belongs in a re-run's plan. An archived
worktree still holding uncommitted or ignored files is **refused**, not
counted: the kept branch does not contain them and they are the only copy —
restore the session or clean the tree first. `older_than` is required and
must be a positive Go duration measured from each session's archive time;
the request needs a scope — `repo_id` or `all: true`, which are mutually
exclusive. `reclaimable_bytes` counts **allocated** disk
blocks (`st_blocks` × 512, like `du`): sparse holes are not counted, and a
hard-linked file is credited only when deleting the tree removes its last
link. It is an upper bound on what a deletion frees, not apparent file size:
extents shared with a copy-on-write reflink clone outside the tree are still
counted, so on Btrfs/XFS the free space recovered can be less than the total. A
session whose recorded origin repository
is gone is refused — with the repo deleted there is no kept branch, so the
archived worktree may be the last copy of the work;
`DeliverPrompt` returns `{ "status": "started" | "sent" }`; `CreateTab`
returns `{ "id"?: "<stable-tab-id>", "name": "<resolved-tab-name>", "tmux_name"?: "<tmux-session>" }`
(`id` is the stable tab id minted by the daemon, which an older daemon may omit; `tmux_name` is the tmux session the tab was spawned under, omitted for a
web/vscode tab that owns no PTY; it normally tracks the name but diverges
after a rename, so read it from the response rather than re-deriving it);
`CloseTab` returns `{ "name": "<resolved-tab-name>" }`; `RenameTab` returns `{ "name": "<resolved-tab-name>" }`; `ReorderTab` returns `{ "name": "<resolved-tab-name>", "index": <int> }`; `ListTasks` returns
`{ "tasks": [<task>…] }`; `AddTask` and `UpdateTask` return
`{ "ok": true, "task": <task> }` (the stored record; an older daemon omits
`AddTask`'s); the rest return `{ "ok": true }`. `AddTask`'s `task` is the bare
stored record — the daemon sets it before reloading schedules and, unlike
`UpdateTask`'s live-enriched record, never attaches the live schedule-health
fields (`arming`, `next_run_at`) that the next sentence attributes to every
returned task; and a sandbox-credential `AddTask` receives a narrowed
projection (`sandboxSafeTask`) that withholds persisted fields such as
`project_path`, `repo_id`, `host`, and `program`. The request `task` field of
`AddTask` is a full task object — the CLI/TUI build and validate it, and the
daemon re-validates and owns the write. `UpdateTask` instead takes a target `id`
and a FIELD-LEVEL `update` patch carrying only the fields to change (e.g.
`{ "id": "ab12cd34", "update": { "enabled": false } }`): the daemon merges the
patch onto the freshly-loaded record under its file lock and leaves every
unspecified field — and the scheduler-owned fields — as-stored, so a single-field
edit cannot clobber a concurrent edit another client made (#1700). Both mutating
calls take an optional `actor` naming the calling surface (`cli`, `tui`, `api`,
`daemon-upgrade`) for the task's audit trail; a client that sends none is
recorded from the transport, so an HTTP caller is `api` (#3623). `TriggerTask`
takes the same optional `actor`, recorded as a `triggered` entry when the
manual fire lands — scheduled cron and watch fires write no entry (#6318).
Every task the
daemon returns carries its schedule health — `overdue`, `missed_occurrences`,
and, from the live scheduler entry, `next_run_at` and `arming` — derived at read
time and never stored. See [tasks.md](tasks.md) for the task shape and
[Is it actually firing?](tasks.md#is-it-actually-firing) for what those mean.

Session JSON timestamps (`af sessions list --json` and session responses) use
RFC 3339 timestamps:

| Field | Meaning |
| --- | --- |
| `created_at` | When the session was created. |
| `updated_at` | When the session state last mutated: lifecycle, identity, prompt delivery, tab roster, or pane activity. Reads, serialization, and cache refreshes do not advance it. |
| `archived_at` | When the session's archive move committed; `--older-than` for `PruneSessions` is measured from it. Omitted on live sessions; records archived before the field existed report nothing and prune falls back to `updated_at`. |

`updated_at` survives saves and restarts. Older records retain their last save
time; a missing or zero stored value falls back to `created_at` when loaded.

`Snapshot` accepts additive list filters in the request body: `live: true`
excludes archived sessions; `statuses` is an array of lifecycle names
(`running`, `ready`, `lost`, `dead`, `archived`, or `limit-reached`) matched as
OR alternatives; `created_after` is an RFC 3339 creation-time lower bound; and
`limit` is a positive maximum row count. These filters compose, run after
`repo_id` scoping, and are applied by the daemon before the response is encoded.
Omitting all four preserves the complete response and its existing stable order.

`UpdateTask`, `RemoveTask`, and `TriggerTask` also accept an optional `expect`
object — `{ "enforce": true, "project_path": "/repos/alpha" }` — asserting the
project the task was bound to when the caller authorized it. The daemon
re-checks it against the freshly-loaded record inside the same locked operation
and refuses the write if the task has since been re-bound, so a client that
checks scope in one request and mutates in another cannot act on a task that
moved projects in between. Omitting `expect` (or sending `enforce: false`) skips
the check, which is what a caller with no project context does — existing
clients are unaffected.

### Backlog

Eleven POST routes serve a project's backlog (the `af backlog` CLI calls them): `GetBacklog`, `AddBacklogItems`, `UpdateBacklogItem`, `MoveBacklogItem`, `SetBacklogItemStatus`, `DeleteBacklogItem`, `PostBacklogMessage` (a note, a question from the item's session, or an answer that is delivered to it), `StartBacklogItem`, `SetBacklogSettings`, `GetBudget` and `SetBudget`. `StartBacklogItem` returns at once with a run that is still starting; the session is created on a daemon-tracked goroutine, and an item left starting by a restart is settled on the daemon's first poll. The per-route fields are in [the generated reference](reference/api.md). A project request names its project with exactly one of `repo_id` or `repo_path` (a path on the daemon host). None of these routes accepts the scoped sandbox credential.

Every write publishes a `backlog.changed` event on `GET /v1/events` with `{repo_id, ids}`: the ids the write touched, including items whose waits-on link was removed by a delete and both parents of a move. The frame names the project and the ids the write touched, not the new rows or their derived state, and a write that changes a parent's roll-up (queueing or finishing a child) does not name the parent; re-read `GetBacklog` on any frame. An empty `ids` means a project-level change (the runner settings): refresh the project. The global budget publishes no event. Refusals are ordinary `500` envelopes with the reason in `error.message`; an invalid budget amount (non-finite, negative or out of range) is refused before anything is written.

### Session state names

The `<session>` objects above spell three of their enums twice, on purpose
(#3631).

`status`, `liveness` and `tabs[].kind` are **integers** and stay integers —
external scripts already decode them as numbers, and retyping them would break
every one of those consumers. Beside each sits a **string twin** that names the
same value, so a payload can be read without consulting Go source:

| Integer | Twin | Vocabulary |
|---------|------|------------|
| `status` | `status_name` | `running`, `ready`, `loading`, `deleting`, `dead`, `lost`, `archived` |
| `liveness` | `liveness_name` | `running`, `ready`, `lost`, `dead`, `archived`, `limit-reached` |
| `tabs[].kind` | `tabs[].kind_name` | `agent`, `shell`, `process`, `web`, `vscode` |

The twins are **always present** — never omitted, even when the integer beside
them is (`liveness` carries `omitempty`) — and they are derived by whichever
binary encodes the payload rather than stored, so a record read off disk or
received from an older daemon is named correctly all the same. Nothing decodes
them back: a client that sends one is ignored.

`liveness_name` is the vocabulary the `statuses` filter above accepts, and it is
resolved by the SAME derivation the filter runs — including the fallback to the
legacy `status` int for a record written before `liveness` existed. So the round
trip holds: every row a `statuses` value selects reports that value as its
`liveness_name`, and vice versa.

That derivation refuses to guess. A record whose liveness cannot be resolved —
one from a daemon predating `liveness` that was caught mid-create or mid-kill, so
its only state is a transient `status`, or one carrying a `status` this `af` does
not know — reports `liveness_name: ""` and is selected by **no** `statuses`
value. It is still listed; it simply does not claim a state it is not in. Such a
row still names its own legacy axis (`status_name: "deleting"`), so the pair
says exactly what is known.

`status_name` names the LEGACY composed axis, which is not always the same
answer, and the difference is the reason both fields exist:

- `loading` and `deleting` are in-flight operations with no liveness of their
  own, so no `statuses` word can select them.
- A session parked at a usage-limit wall has no legacy `status` value at all: it
  composes to `ready` (`status: 1`, `status_name: "ready"`) while
  `liveness_name` reports `limit-reached`. **Filter and branch on
  `liveness_name`**; read `status_name` only when you are reading `status`.

`tabs[].kind_name` uses the same words as `tab_kinds[].kind` — the two arrays no
longer disagree about what a `kind` is. Every creatable kind's name is also its
`--kind` / `CreateTab` `kind` value, so a tab's reported kind can be handed
straight back to a create call; `agent` and `process` are named but are not
creatable (see `tab_kinds` for what a session will accept).

An integer this binary does not recognize — a record from a newer `af` — names
itself `""` rather than guessing a word, in every one of the three twins.

### Session idle diagnosis

The `<session>` objects above mirror `af sessions list` output. Two of their
fields carry the daemon's **idle diagnosis** (#3168, #3175) and deserve a
contract statement, because reading more into them than they promise is exactly
the mistake they were designed to prevent:

**`idle_reason`** (string, omitted when no reason is established) — the daemon's
mechanically established explanation for why a session is not doing visible
work. The vocabulary is closed:

| Value | The daemon observed |
|-------|---------------------|
| `usage-limit` | The session is parked at a [usage-limit wall](usage-limits.md). |
| `process-exited` | The agent process is gone — the row's liveness is Lost or Dead. |
| `restore-gave-up` | The process is gone and automatic Lost-session recovery has stopped — either it exhausted its failed-attempt budget, or it confirmed enough restore-and-die cycles to conclude the agent cannot stay up. See `lost_restore_failure` below for the two verdicts. |
| `restore-waiting-on-environment` | The process is gone and automatic Lost-session recovery is parked on an environmental failure — a condition outside the session itself, such as a full disk, not a session defect — and retries on a capped backoff without spending the structural attempt budget. See `lost_restore_wait` below. |
| `recreate-pending` | A recognized recreate notice is waiting, unacknowledged, on the root session. |
| `prompt-not-delivered` | The last prompt send observed that the prompt did not arrive. |
| `delivery-unconfirmed` | The last send ended `sent-unverified` or `could-not-confirm` (uncertainty, not failure — see `SendPrompt` above), and no pane change has been observed since. |
| `no-pane-change-since-delivery` | The last prompt was delivered, and the pane has not changed since. |
| `settled-after-pane-change` | The row settled back to Ready, and the pane changed *after* the last prompt attempt. When later churn resolves an unconfirmed send, this is all af reports — it never retroactively claims delivery (#3162). |

**`lost_restore_failure`** (object, omitted unless automatic recovery gave up) —
the durable terminal restore result: `{ "attempts": 6, "error": "..." }`. The
session remains Lost, and the daemon does not automatically retry it again. An
explicit restore remains available; a successful runtime replacement clears the
field. Because it is stored on the session record, daemon restarts and the TUI,
web, and HTTP clients retain the same last error.

What `attempts` counts depends on which verdict ended the loop, and `error` is
the discriminator — there is no separate field naming the verdict:

- **Failed-attempt exhaustion** — `attempts` counts consecutive FAILED restore
  attempts that spent the structural budget (6 by default), and `error` is the
  last attempt's failure: the spawn died before it could be confirmed alive,
  the preserve push failed, and so on.
- **Confirmed-flap give-up** — `attempts` counts CONFIRMED SUCCESSFUL restores
  (4 inside a 30-minute window) that each went lost again anyway, and `error`
  begins `the session was confirmed restored N times in 30m0s and went lost
  again each time`. Four successes are not four failures: a consumer that
  renders this as "4 attempts failed" inverts the evidence — every one of
  those spawns worked, and the agent exited after it was confirmed up.

The confirmed-flap count is in-memory evidence only: it does not survive a
daemon restart the way this record does, and a restart re-opens the count at
zero while the durable verdict stands.

**`lost_restore_wait`** (object, omitted unless automatic recovery is waiting on
the environment) — the non-terminal counterpart of `lost_restore_failure`:
`{ "attempts": 3, "error": "..." }`. `attempts` counts consecutive environmental
failures in the current episode and `error` is the last observed environmental
error, sanitized the same way. The session remains Lost and the daemon keeps
retrying on a capped backoff — these attempts do NOT draw down the budget
`lost_restore_failure` reports against. A successful restore, a runtime
replacement, a structural failure, or a kill tombstone clears the field, and a
row that reloads inert — one no automatic retry can ever reach, such as a
sandbox session after a daemon restart — drops it on load rather than report
an armed retry that does not exist.

**`last_pane_churn_at`** (RFC 3339 timestamp, omitted when no churn is on
record) — when the daemon last observed the session's pane **render different
bytes**. The evidence is cleared when the agent process is replaced, so a
retired process's output is never attributed to its successor.

**No value in this vocabulary means "the task finished" — and none means "asked
a question" or "wedged".** The daemon observes pane churn (bytes changing in a
terminal) and prompt-delivery evidence, not the meaning of the agent's output. A
completed task, a question waiting for your answer, and a stuck agent can render
exactly alike, so af reports the observation — `settled-after-pane-change` —
and leaves the interpretation to you: read the pane. An absent `idle_reason` is
likewise "no reason established" (the row is working, an operation is in flight,
or there is no delivery evidence yet), not "all clear".

The vocabulary can grow. Treat an unknown value as no reason — the TUI and web
rows render no idle-reason label for values they don't know, so an older client
never mislabels a newer daemon's observation.

### Session warnings

Session objects also carry daemon-projected warnings as additive,
omitted-when-empty fields. Two are single strings — `archive_warning` (an
incomplete archive's bounded notice) and `callback_unavailable` (why the session
has no af callback) — and one is a list:

**`agent_config_skipped`** (array of strings, omitted when empty) — one
`agent:file: <reason>` entry per declared [`sandbox_agent_config`](backends.md#ssh-backend)
file the session's account-scope placement refused: missing, unreadable, not a
regular file, unparsable, or pushing the placement archive over its 64 MiB
bound. Recorded at provision and
refreshed by every reprovision and account swap that stages a scope, so a clean
placement clears it and the row never warns about a file the live scope now
holds. Every surface renders the joined entries under the `af agent config
skipped:` prefix.

## Examples

Health check:

```bash
curl --unix-socket ~/.agent-factory/daemon-http.sock http://localhost/v1/health
# {"data":{"ok":true},"error":null}
```

List every session (all repos):

```bash
curl --unix-socket ~/.agent-factory/daemon-http.sock \
  http://localhost/v1/Snapshot -d '{}'
```

Send a prompt into an existing session:

```bash
curl --unix-socket ~/.agent-factory/daemon-http.sock \
  http://localhost/v1/SendPrompt \
  -d '{"title":"fix-auth","prompt":"run the tests and report failures"}'
# {"data":{"ok":true,"status":"delivered"},"error":null}
```

List tasks (no body needed):

```bash
curl --unix-socket ~/.agent-factory/daemon-http.sock http://localhost/v1/ListTasks -d '{}'
```

Wrong verb → `405`:

```bash
curl -i --unix-socket ~/.agent-factory/daemon-http.sock http://localhost/v1/ListTasks
# HTTP/1.1 405 Method Not Allowed
# {"data":null,"error":{"message":"method GET not allowed; use POST","daemon_rejected":true}}
```

Oversize body → `413` (rejected, never processed):

```bash
head -c 20000000 /dev/zero | tr '\0' 'a' \
  | curl -i --unix-socket ~/.agent-factory/daemon-http.sock \
      http://localhost/v1/AddTask --data-binary @-
# HTTP/1.1 413 Request Entity Too Large
# {"data":null,"error":{"message":"request body exceeds 16777216-byte limit: …","daemon_rejected":true}}
```

## Relationship to the CLI

The HTTP API and `af sessions` / `af tasks` are two front-ends over one daemon
core. Prefer the CLI for interactive and scripting use — it handles daemon
startup, `--repo` resolution, and flag validation for you. Reach for the HTTP
API when you want to call the daemon from a language or tool without shelling
out to `af`, from inside an agent, or from a small local service. Both emit the
identical `{data, error}` envelope, so a consumer written against one reads the
other unchanged.

### Task completion choices

`POST /v1/ListOnComplete` accepts `{}` and returns
a shared-envelope payload whose `values` array contains `{value, hint}` options,
for example `{"value":"kill","hint":"deletes the run's session and its branch — permanent"}`.
Values follow
`task.OnCompleteValues()` order, least destructive first. Clients should render
this catalog rather than copy the enum or consequence wording. The default is
the first option. Send `on_complete` on the task in `AddTask`, or on `update` in
`UpdateTask`; send the selected verb unchanged. The store canonicalizes keep to
empty on both create and update. Completion policies apply only to spawned
sessions, so tasks with `target_session` must use empty.
