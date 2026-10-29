# Proposal mode: future

These features are not part of the current plan. The current design must not
block them.

## Export into the repo

`af proposal export <slug> <dir>` copies the docs into the project, for
example into `docs/design/`. Then a proposal can go into a PR.

- Keep the docs as plain markdown with no af-only syntax, except the
  callouts. Then an export needs no conversion.
- The export could remove open `[!todo]` callouts, or it could refuse to
  run while `[!question]` callouts are still open.

## Hand a proposal to the sessions that implement it

`af sessions create --proposal <slug>` attaches a new session when it starts.
For example, one session for each step in `goals.md`.

- Cursors and attachments use the session ID, so this needs no change to
  storage.
- More than one session can be attached to the same proposal. `changes`
  already shows each session the edits by other sessions.

## Remote and sandboxed agents

Docker, ssh and sandbox sessions cannot read the host disk. For now, `start`
and `attach` refuse these sessions, because the rules tell the agent to edit
files at a host path.

- `cat` and `write` already give them access through the CLI.
- When the CLI moves to the daemon routes (step 2), `--daemon-url` works too.
- Later, af could sync the proposal folder into the sandbox. This must not
  change the store layout.

## Read-only view in the TUI

The TUI could render a proposal read-only, for example with glamour. It reads
the same files, so it needs no new API.

## History and undo in the Docs tab

Every save is already a commit. A history panel could show the commits for a
doc, with author and time, and restore an old version as a new commit.

## Selection actions

Select text in the Docs tab, then choose **Ask agent** or **Expand**. This
sends a prompt that quotes the selection and names the doc. It uses the same
send path as push.

## Cross-references

Links between docs (`[API](api.md#routes)`) work as plain markdown. Later,
the Docs tab could follow them to the right sub-tab and heading.

## Things the current design does not try to support

- Two humans who edit the same doc at the same time. This needs a CRDT, and
  the three-way merge does not cover it.
- Proposals that span more than one project. The store is per project.
- Proposal status. If users ask for it later, add it as a field in a small
  metadata file in the proposal folder, not as a folder layout.
