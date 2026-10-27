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
- Remote daemon in step 1: "af proposal does not support --daemon-url yet;
  run it on the daemon host".
- Unknown slug: the error lists the slugs that exist.

## HTTP (step 2)

All routes are under the existing daemon auth.

```
GET  /v1/repos/{repoID}/proposals
GET  /v1/repos/{repoID}/proposals/{slug}                 docs + head commit
GET  /v1/repos/{repoID}/proposals/{slug}/docs/{doc}      text + commit
PUT  /v1/repos/{repoID}/proposals/{slug}/docs/{doc}      {text, base} → 200 {text, commit, merged} | 409 {ours, theirs, base}
POST /v1/repos/{repoID}/proposals/{slug}/docs            {name} → add-doc
POST /v1/sessions/{id}/proposal/send                     send changes now (the Send now button)
PUT  /v1/sessions/{id}/proposal/push                     {mode: "auto"|"manual"}
```

Event on the existing events socket:

```json
{"type": "proposal.changed", "repo_id": "…", "slug": "auth-rework",
 "doc": "api.md", "commit": "9c1d…", "author": "agent"}
```

## Tab kind (step 2)

```
af sessions tab-create <title> --kind docs --proposal <slug>
```

`start` and `attach` create this tab when the session is local and the web UI
is available. When they create the tab, the output also includes `"tab": "<name>"`.
