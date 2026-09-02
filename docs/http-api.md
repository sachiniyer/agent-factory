# `af` HTTP/JSON API reference

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

The socket is created when the daemon starts (on demand whenever `af` runs and
there is work to host, or via an autostart unit — see
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
`{data, error}` JSON envelope the CLI's `--json` flag emits, so the two surfaces
are byte-for-byte identical.

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
  "error": { "message": "agent-factory daemon is starting (restoring sessions); retry shortly" }
}
```

Both members always serialize (no `omitempty`), so a consumer can branch on
`error === null` without a presence check. Every response sets
`Content-Type: application/json`.

## Status codes

| Status | Meaning |
|--------|---------|
| `200 OK` | Success. `data` holds the response payload; `error` is `null`. |
| `400 Bad Request` | The request body was not valid JSON, or it carried a field this daemon does not recognize (see [Unknown fields](#unknown-fields)). |
| `404 Not Found` | Unknown route (e.g. `POST /v1/Nope`). |
| `405 Method Not Allowed` | Wrong verb — RPC routes are POST-only; `/v1/health` is GET-only. |
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
{"data":null,"error":{"message":"malformed JSON request body: json: unknown field \"repo_idd\""}}
```

**Requests carrying the `X-AF-Client-Version` header** — which the `af` TUI and CLI
send automatically — are decoded **leniently**: unrecognized keys are ignored. The
daemon is upgraded independently of its clients, so a client newer than the daemon
legitimately sends fields the daemon has never heard of. Fields are additive and
never renamed, so ignoring an unknown one is always safe, whereas rejecting it
would turn every version skew into a hard failure.

The bundled web UI does not send the header and is decoded strictly. It is always
served by the daemon it talks to, so it cannot be newer than that daemon and has no
skew to tolerate.

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

**Response shapes.** These are not part of the generated request-field catalog,
so they are documented here. `CreateSession` returns `{ "instance": <session> }`;
`Snapshot` and `ImportRemoteHookSessions` return `{ "instances": [<session>…] }`;
`ArchiveSession` returns `{ "ok": true, "archived_path": "…" }`;
`RestoreArchived` returns `{ "ok": true, "worktree_path": "…" }`;
`SendPrompt` returns `{ "ok": true, "status": "delivered" | "not-delivered" | "sent-unverified" | "could-not-confirm" }`;
`sent-unverified` means the paste and Enter were accepted while a readable pane
did not render exact prompt content; `could-not-confirm` means the pane observer
itself was unavailable. Neither status claims delivery.
`DeliverPrompt` returns `{ "status": "started" | "sent" }`; `CreateTab`
returns `{ "id"?: "<stable-tab-id>", "name": "<resolved-tab-name>", "tmux_name"?: "<tmux-session>" }`
(`id` is the stable tab id minted by the daemon, which an older daemon may omit; `tmux_name` is the tmux session the tab was spawned under, omitted for a
web/vscode tab that owns no PTY; it normally tracks the name but diverges
after a rename, so read it from the response rather than re-deriving it);
`CloseTab` returns `{ "name": "<resolved-tab-name>" }`; `ListTasks` returns
`{ "tasks": [<task>…] }`; `UpdateTask` returns `{ "ok": true, "task": <task> }`
(the merged record); the rest return `{ "ok": true }`. The `task` field of
`AddTask` is a full task object — the CLI/TUI build and validate it, and the
daemon re-validates and owns the write. `UpdateTask` instead takes a target `id`
and a FIELD-LEVEL `update` patch carrying only the fields to change (e.g.
`{ "id": "ab12cd34", "update": { "enabled": false } }`): the daemon merges the
patch onto the freshly-loaded record under its file lock and leaves every
unspecified field — and the scheduler-owned fields — as-stored, so a single-field
edit cannot clobber a concurrent edit another client made (#1700). See
[tasks.md](tasks.md) for the task shape.

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
| `restore-gave-up` | The process is gone and automatic Lost-session recovery exhausted its attempt budget. See `lost_restore_failure` below. |
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
# {"data":null,"error":{"message":"method GET not allowed; use POST"}}
```

Oversize body → `413` (rejected, never processed):

```bash
head -c 20000000 /dev/zero | tr '\0' 'a' \
  | curl -i --unix-socket ~/.agent-factory/daemon-http.sock \
      http://localhost/v1/AddTask --data-binary @-
# HTTP/1.1 413 Request Entity Too Large
# {"data":null,"error":{"message":"request body exceeds 16777216-byte limit: …"}}
```

## Relationship to the CLI

The HTTP API and `af sessions` / `af tasks` are two front-ends over one daemon
core. Prefer the CLI for interactive and scripting use — it handles daemon
startup, `--repo` resolution, and flag validation for you. Reach for the HTTP
API when you want to call the daemon from a language or tool without shelling
out to `af`, from inside an agent, or from a small local service. Both emit the
identical `{data, error}` envelope, so a consumer written against one reads the
other unchanged.
