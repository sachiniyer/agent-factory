# Proposal mode: technical design

## Storage

There is one git repository for each project:

```
<AF_HOME>/repos/<repoID>/
  proposals/                             the work tree: docs only
    auth-rework/
      goals.md  technical.md  api.md
    search-v2/
      goals.md
  proposals.git/                         the git dir (git init on first use)
  proposals.lock                         flock for every write and commit
```

- Agents edit `proposals/` with their own file tools, so af treats
  everything in it as untrusted:
  - The git dir and the lock live beside the work tree, never in it. Every
    git command passes `--git-dir` and `--work-tree`, so a `.git` folder or
    `gitdir:` file that an agent plants in `proposals/` is ignored. A
    `.gitattributes` there cannot name a filter or diff driver, because
    drivers are defined only in config, and no config that git reads is in
    the work tree. As a second guard, every command pins
    `core.hooksPath`, `core.fsmonitor` and `core.attributesFile`, and diffs run
    with `--no-ext-diff --no-textconv`.
  - git never scans `proposals/`. af reads each doc (`<slug>/<name>.md`)
    itself, writes it with `hash-object --no-filters`, and commits with
    `update-index`, `write-tree`, `commit-tree` and `update-ref`. A
    `.gitignore`, `.gitattributes` or nested repository in the folder
    cannot hide a doc, change it, or get into the history. Patches are made
    with `--text`.
  - af does not follow a symlink inside `proposals/`. Each read and write
    opens the store, slug and doc through directory fds with `O_NOFOLLOW`, and a
    doc must be a regular file.
- Builds before this layout kept the git dir at `proposals/.git`. On first use,
  af imports its history once into `proposals.git`. It copies only
  `objects/` (without alternates), `refs/` and `packed-refs`, and it does not
  copy the config, hooks or attributes. If a symlink or special file is in
  those folders, the import stops. When the import is done, af deletes the
  old `.git`.

- `<AF_HOME>` is `config.GetConfigDir()`. `<repoID>` is the project's ID. All
  worktrees of a repo share that ID (`config.ResolveMainRepoRoot`), so all
  sessions of a project see the same proposals.
- A slug must match `^[a-z0-9][a-z0-9-]{0,62}$`. A doc name must match the
  same pattern, and the file on disk gets a `.md` extension.
- The repository is internal state. Users do not see it, and it never gets a
  remote.

## Templates

- `af` embeds built-in templates: `goals`, `technical`, `api`, `ui`, `steps`
  and `future`. `future` lists what later features or additions may need, so that
  the current design does not block them.
- The user can override a template, or add a new one, at
  `<AF_HOME>/templates/proposal/<name>.md`. A user file overrides the
  built-in template that has the same name.
- A template can use `{{title}}`. This is the slug with dashes changed to
  spaces. Do not add other template features.

## Commits and attribution

Every change goes into a commit. Each commit carries trailers:

```
Af-Author: agent | human | external
Af-Session: <session id>          (only when Af-Author is agent)
```

- **agent** commits come from `af proposal checkpoint`, which the calling
  session runs.
- **human** commits come from Docs tab saves (step 2). The daemon knows that
  these writes come from the user.
- **external** commits snapshot changes that are on disk but not yet
  committed, when the author is not known. For example, an edit in some other
  editor. `changes` and `checkpoint` both take this snapshot first, before
  they do anything else.

Commits disable hooks and set a fixed committer identity, so the user's global
git config cannot break them or change them.

## Attachment and cursors

When a session is attached to a proposal, a ref exists in the proposals repo:

```
refs/af/cursor/<session id>     → the last commit this session has seen
refs/af/attach/<session id>     → a blob that holds the slug
```

- A session is attached to at most one proposal. When it attaches to a second
  proposal, it detaches from the first.
- `changes` returns every commit between the cursor and `HEAD` that touches
  `<slug>/` and is **not** an agent commit by this session. Then it moves the
  cursor to `HEAD`.
- `checkpoint` takes the snapshot, commits the agent's own edits, and moves
  the cursor to `HEAD`. Then `changes` does not show the agent its own edits.
- Push (step 3) calls the same code as `changes`. Push and pull move the same
  cursor, so the agent never gets the same edit twice.

### Limit of step 1

Before the Docs tab exists, user edits and agent edits both arrive as
uncommitted files on disk. Suppose the agent edits a file and does not run
`checkpoint`. Then the next snapshot records the agent's edit as external, and
`changes` shows the agent its own edit. This only adds noise; no edit is lost.
The rules that `af` prints tell the agent to run `checkpoint` after each round
of edits. From step 2, Docs tab saves commit as `human` immediately, so files
that are not committed come only from agents.

## Session and project resolution

- **Inside a session:** commands use `resolveSelfSession()`
  (`api/sessions.go`). The session record gives the session ID and the repo.
- **Outside a session:** `list`, `path`, `cat` and `write` follow the shared
  project rules: `--repo` first, then the current directory's project, and if
  there is neither, an error that asks for `--repo`.
- `start`, `attach`, `detach`, `changes` and `checkpoint` need a session. A
  user in a normal shell can pass `--session <title>`.

## Transport

- **Step 1** read and wrote the local disk directly.
- **Step 2** moves the store behind daemon HTTP routes (see `api.md`). The CLI
  and the Docs tab both use them, so `--daemon-url` works. The daemon is the
  only process that writes the store.

## Three-way merge (step 2)

A Docs tab save sends the new text together with the commit it started from.
If `HEAD` changed that file since that commit, the daemon runs
`git merge-file` with the base, theirs (`HEAD`) and ours (the save).

- **The merge is clean:** the daemon commits the result as `human` and sends
  the merged text back.
- **The merge has a conflict:** the daemon does not commit. It sends both
  versions back, and the tab shows a conflict banner.

## File watching (step 2)

The daemon checks the watched proposals in its existing poll loop. A proposal
is watched when a session is attached to it or has a Docs tab for it. For each
watched proposal, the daemon reads the folder and compares the mtime and size
of each doc with the last values it saw. A change publishes a
`proposal.changed` event on the existing events WebSocket, so open tabs reload
clean buffers immediately. For a buffer that has edits, the tab merges on its
next save.

- This uses no new dependency. A stat of a few files on each tick costs less
  than fsnotify, which needs a watch for each folder and its own goroutine.
- The check does not commit. The next read, save, `changes` or `checkpoint`
  snapshots the change as before.
- The list of attachments is read again only when the attach refs change.

## Push (step 3)

The daemon sends a session its unseen edits as a prompt, so the agent does not
have to pull them. `api.md` (Push) has the routes, the status shape and the
prompt.

### Conditions

In auto mode, the daemon sends when all of these are true:

1. The last human save (`SaveProposalDoc`) was 10 s or more ago. A save
   records its time for every session attached to the proposal.
2. The session is `Ready` (it is idle and waits for input). For Claude this is
   reliable: `task/claude_working.go` keeps a session `Running` through a
   mid-turn pause behind a still pane.
3. No TUI is attached full-screen to the session (the same pause lease that
   holds task deliveries, #1586), so a push does not submit what the user is
   typing in the pane.
4. `changes` for that session would not be empty.
5. The push is not held, and no earlier push is unconfirmed.

**Send now** skips the 10 s wait, the hold and condition 3. It still waits
for `Ready` and for an unconfirmed delivery.

### Code

- `proposal.DecidePush` is a pure function from one session's state and the
  time to a push state and a send decision. It holds every timing rule, so
  `proposal/push_test.go` tests the rules without a daemon.
- `proposal.Service` keeps one push entry for each attached session: mode,
  last human save, hold, send now, delivery in flight, the time of an
  unconfirmed delivery, a retry time, and a cached unsent count. The count
  comes from `git log` with no diff, and it is counted again after each
  commit through the daemon, each pull or checkpoint, and every 30 s.
- The poll loop calls `ScanProposals` and then `pushProposals`
  (`daemon/proposals_push.go`). That builds one `PushTarget` for each loaded
  session: the push mode of each Docs tab, `Ready` from the liveness, and the
  TUI pause lease. A send runs on its own goroutine, so a slow delivery never
  holds the poll loop.
- The push mode is stored on the Docs tab (`push_mode` in the tab record), so
  it stays after a restart. The rest of the state is in memory. After a
  daemon restart, a push starts again with the next human save.

### Send

1. The daemon runs the same store call as `changes`, which moves the cursor.
2. It builds the prompt from the edits (`proposal.PushPrompt`). Above 8 KB of
   patches the prompt names the files only.
3. It calls `SendPromptWithStatus`, the path of `af sessions send-prompt`.
4. `delivered` confirms the push. `sent-unverified`, `could-not-confirm` and
   an ambiguous send error leave it unconfirmed: no other push goes to the
   session until the daemon sees it busy, or for 60 s. A refusal before any
   byte reached the pane (`prompt not delivered`) or `not-delivered` puts the
   cursor back with a compare-and-swap ref update, and the push tries again
   after 10 s.

## Code layout

- `proposal/` is a new package for the store, templates, git operations,
  cursors and the rules text. It must not import `daemon` or `app`.
- `api/proposals.go` holds the `af proposal` Cobra commands.
- The agent usage text in `session/systemprompt.go` gets a short "Proposals"
  section. The plugins and the CLI reference are generated from it
  (`scripts/gen-docs.sh`).
