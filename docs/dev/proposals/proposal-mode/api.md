# Proposal mode: API design

## CLI

All commands print JSON, the same as the other `af` commands.

| Command | Needs a session | What it does |
|---|---|---|
| `af proposal start <slug> [--docs goals,technical,api,ui,future]` | yes | Creates the proposal from templates, attaches, and prints the rules. Fails if the slug exists. The default is all five docs. |
| `af proposal attach [<slug>]` | yes | Attaches to an existing proposal and prints the rules. With no slug, prints the current proposal and the rules again. |
| `af proposal detach` | yes | Removes this session's attachment and cursor. |
| `af proposal list` | no | Lists proposals by last edit: slug, docs, attached sessions, and last edit time. |
| `af proposal path <slug>` | no | Prints the absolute folder path. |
| `af proposal add-doc <slug> <name>` | no | Adds a doc from a template, or a blank doc if there is no template with that name. |
| `af proposal changes` | yes | Prints the edits that other people and other sessions made since this session's cursor, then moves the cursor. |
| `af proposal checkpoint` | yes | Commits this session's edits and moves its cursor. |
| `af proposal cat <slug> <doc>` | no | Prints a doc. This is for agents that cannot read the host disk. |
| `af proposal write <slug> <doc>` | no | Replaces a doc with stdin and commits as `agent` when there is a session, or as `external` when there is no session. |

Common flags: `--repo <path>`, and `--session <title>` for the commands that
need a session.

### start / attach output

```json
{
  "slug": "auth-rework",
  "path": "/home/u/.agent-factory/repos/7b11341568c3/proposals/auth-rework",
  "docs": ["goals.md", "technical.md", "api.md"],
  "attached_sessions": ["auth-planner"],
  "rules": "You are working on proposal auth-rework …"
}
```

`rules` is the working contract for the agent. It is a fixed text that has the
path filled in:

1. The docs are at `<path>`. Edit them with your normal file tools.
2. Before you edit, run `af proposal changes` and read what the user changed.
3. After each round of edits, run `af proposal checkpoint`.
4. Put questions for the user in `> [!question]` callouts, in the doc they
   belong to. Then stop and tell the user.
5. Do each `> [!todo]` callout, then delete it.
6. Do not change a `> [!decision]` callout without asking the user.
7. To add a doc, run `af proposal add-doc`.
8. If you lose track of the proposal, run `af proposal attach`.

### changes output

```json
{
  "slug": "auth-rework",
  "from": "3f2a…",
  "to": "9c1d…",
  "edits": [
    {"author": "human", "at": "2026-10-20T22:41:14Z",
     "files": ["api.md"], "patch": "@@ -12,3 +12,4 @@ …"}
  ]
}
```

An empty `edits` array means that nobody else changed the proposal.

### Errors

- No session found: "af proposal changes needs a session: run it inside an af
  session, or pass --session <title>".
- With `--daemon-url`, `--repo` is a path on the daemon host. With no `--repo`
  and no session, the error asks for `--repo`, because the current directory
  does not name a project on the remote host.
- Unknown slug: the error lists the slugs that exist.

## HTTP (step 2)

The CLI and the Docs tab use these routes. They follow the conventions of
`daemon/httproutes.go`:

- Every route is `POST /v1/<Verb>` with a JSON body, under the existing daemon
  auth. No route is open to the sandbox credential.
- Every reply is the shared envelope `{"data": …, "error": null}`. A failure is
  `{"data": null, "error": {"message": "…", "code": "…"}}` with status 500,
  except the save conflict below (409).
- Unknown request fields are refused (400) unless the request carries the af
  client version header.

### Project target

Every route that is not session-only takes exactly one of these fields, which
name the project:

| Field | Meaning |
|---|---|
| `session_id` | The session's stable ID. The daemon uses the session's project. The Docs tab sends this. |
| `repo_id` | The project ID (12 hex characters). |
| `repo_path` | A path inside the project on the daemon host. The CLI sends this with `--daemon-url --repo`. |

A request with none of them, or more than one, fails.

`slug` is a proposal slug. `doc` is a doc name with or without `.md` (`api`
or `api.md`). Replies always use the file name (`api.md`).

### Routes

| Route | Request | Reply `data` |
|---|---|---|
| `POST /v1/ListProposals` | target | `{proposals: [proposal]}`, newest edit first |
| `POST /v1/GetProposal` | target, `slug` | `proposal detail` |
| `POST /v1/StartProposal` | `session_id`, `slug`, `docs` | `attached proposal` |
| `POST /v1/AttachProposal` | `session_id`, `slug` (optional) | `attached proposal` |
| `POST /v1/DetachProposal` | `session_id` | `{slug, detached, tab}` |
| `POST /v1/AddProposalDoc` | target, `slug`, `name`, `author` | `{slug, doc, path, commit}` |
| `POST /v1/ReadProposalDoc` | target, `slug`, `doc` | `{slug, doc, text, commit}` |
| `POST /v1/SaveProposalDoc` | target, `slug`, `doc`, `text`, `base` | `{slug, doc, text, commit, merged}`, or 409 |
| `POST /v1/WriteProposalDoc` | target, `slug`, `doc`, `text`, `author` | `{slug, doc, path, commit, author}` |
| `POST /v1/PullProposalChanges` | `session_id` | the `changes` output above |
| `POST /v1/CheckpointProposal` | `session_id` | `{slug, commit, cursor, unseen}` |

Shapes:

```jsonc
// proposal (ListProposals rows)
{"slug": "auth-rework", "path": "/abs/path/on/daemon/host",
 "docs": ["goals.md", "api.md"], "attached_sessions": ["auth-planner"],
 "last_edit": "2026-10-20T22:41:14Z"}

// proposal detail (GetProposal)
{"slug": "auth-rework", "path": "/abs/path/on/daemon/host",
 "head": "9c1d…",
 "docs": [{"name": "goals", "file": "goals.md"}, {"name": "api", "file": "api.md"}],
 "templates": ["technical", "ui", "future"],
 "open_questions": 2,
 "attached_sessions": ["auth-planner"]}

// attached proposal (StartProposal, AttachProposal)
{"slug": "auth-rework", "path": "…", "docs": ["goals.md"],
 "attached_sessions": ["auth-planner"], "rules": "You are working on …",
 "tab": "docs"}
```

- `templates` lists the template names (built-in and user) that the proposal
  does not use yet, in template order. The **+** menu shows them.
- `open_questions` counts the `> [!question]` callouts in all docs of the
  proposal.
- `head` is the store commit that the docs are at. Every doc read returns the
  same kind of commit as `commit`.
- `tab` is the name of the session's Docs tab for this slug. Start and attach
  create the tab when the session has none for the slug. `tab` is absent when
  the daemon cannot create it; then `tab_error` holds the reason. Detach closes
  the session's Docs tabs for the slug, and its `tab` is the closed tab name or
  `""`. Detach, and an attach that moves the session to another slug, close the
  old Docs tabs first. When that close fails, the request fails and the
  attachment does not change, so a retry closes them.
- `docs` in `StartProposal` is a list of doc names. Empty means all five
  built-in templates.
- `author` is `human`, `agent` or `external`. `agent` needs `session_id`, and
  the commit records that session. The Docs tab sends `human`.
- `ReadProposalDoc` first commits uncommitted edits of that doc as `external`,
  and publishes that commit as a `proposal.changed` event. Then `text` is the doc at `commit`, and the tab can send that `commit` as
  the `base` of its next save.

### Save and merge

`SaveProposalDoc` is the Docs tab save. It always commits as `human`.

- `base` is the `commit` from the last read or save of this doc. It is
  required, and it must be a lowercase hex commit id (7 to 64 characters). Any
  other value, such as a ref name, fails with `unknown base commit`.
- The daemon first commits uncommitted edits of the doc as `external`. Then
  "theirs" is the doc at the new head.
- If theirs is the same as the doc at `base`, the daemon writes `text` and
  commits it. Reply: `merged: false`.
- If theirs changed, the daemon runs `git merge-file` with ours = `text`,
  base = the doc at `base`, theirs = the doc at head.
  - A clean merge: the daemon writes and commits the merged text. Reply:
    `merged: true`, and `text` is the merged text.
  - A conflict: the daemon commits nothing and replies with status 409:

```json
{"data": {"ours": "…", "theirs": "…", "base": "…", "head": "9c1d…"},
 "error": {"message": "api.md changed since base 3f2a…, and the edits conflict",
           "code": "proposal_conflict"}}
```

- `commit` in a 200 reply is the new head. When nothing changed, no commit is
  made and `commit` is the current head. Send it as the next `base`.
- After a 409, the tab resolves the conflict and saves again with `base` set
  to `head` from the 409 reply.

### Event

When a doc of a watched proposal changes, the daemon publishes this event on
the existing events socket (`GET /v1/events`):

```json
{"type": "proposal.changed",
 "data": {"repo_id": "7b11341568c3",
          "session_ids": ["8f0c…"],
          "slug": "auth-rework", "doc": "api.md",
          "commit": "9c1d…", "author": "human"}}
```

- A proposal is watched when a session is attached to it or has a Docs tab for
  it.
- `session_ids` lists those sessions. It can be empty.
- Saves, writes and add-doc through the daemon publish the event with their
  commit and author.
- The daemon also checks the watched docs on disk in its poll loop (mtime and
  size). An edit on disk, for example by an agent, publishes the event with
  `"commit": null` and `"author": "external"`, the author that the next
  snapshot gives it. The check does not commit; the next read, save,
  `changes` or `checkpoint` does.
- A deleted doc publishes the event too. The tab reads the proposal again.

## Push (step 3)

```
POST /v1/SendProposalChanges      send changes now (the Send now button)
POST /v1/SetProposalPushMode      {mode: "auto"|"manual"}
```

These names are not final.

## Tab kind (step 2)

```
af sessions tab-create <title> --kind docs --proposal <slug>
```

- The tab kind `docs` is value 5 in `tabs[].kind`, and `tabs[].kind_name` is
  `"docs"`. The tab record has `"proposal": "<slug>"`.
- `CreateTabRequest` takes `kind: "docs"` and `proposal: "<slug>"`. The slug
  must exist in the session's project. `--proposal` is not valid for other
  kinds.
- The tab has no tmux process, like a web tab. It works for every backend,
  because the store is on the daemon host.
- The TUI shows a placeholder that says the Docs tab is web only.
- `start` and `attach` create this tab for the calling session when it has no
  Docs tab for the slug, and print `"tab": "<name>"`. `detach` closes it.
