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
    there, or expose the TLS+token TCP listener to the network (it's on by
    default on loopback; point `listen_addr` at a routable host:port). Both are
    covered in [Remote daemon access](remote-tcp-auth.md).

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
| `400 Bad Request` | The request body was not valid JSON. |
| `404 Not Found` | Unknown route (e.g. `POST /v1/Nope`). |
| `405 Method Not Allowed` | Wrong verb — RPC routes are POST-only; `/v1/health` is GET-only. |
| `413 Request Entity Too Large` | The body exceeded the 16 MiB cap. The request is **rejected, never truncated-then-processed** — the daemon is never reached. |
| `500 Internal Server Error` | The handler ran but returned an error (validation failure, not-found session, a disabled task refused by `TriggerTask`, etc.). `error.message` carries the detail. |

The status maps the *transport* outcome; a business-logic failure (e.g. "session
not found") is a `500` with a descriptive `error.message`, not a bare status.

## Warm-up behavior

The daemon binds its sockets **before** it finishes restoring sessions, which
can take minutes on repos with remote-hook sessions. During that window:

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
`DeliverPrompt` returns `{ "status": "started" | "sent" }`; `CreateTab` /
`CloseTab` return `{ "name": "<resolved-tab-name>" }`; `ListTasks` returns
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
# {"data":{"ok":true},"error":null}
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
