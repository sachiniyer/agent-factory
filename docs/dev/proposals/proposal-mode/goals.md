# Proposal mode: goals

## Problem

It is hard to work on a plan together with an agent. The plan lives in the
agent's chat output. You cannot edit it in place. When you want a change, you
describe the edit in a prompt and the agent writes the whole plan again.

A plan is usually a small set of focused documents: high-level goals, a
technical design, an API design, a UI design, and notes on future work. You
want to read and edit these documents next to the agent, and you want the agent
to see your edits.

The VS Code tab can do this, but it is slow to open and it is not good for
reading markdown.

## Goals

1. **Files are the interface.** A proposal is a folder of markdown files. An
   agent works on it with its usual file tools. Agents do not need a special
   protocol, so every agent that `af` supports can take part.
2. **Shared by the project.** A proposal belongs to the project, not to a
   session or a worktree. More than one session can work on the same proposal,
   and the proposal stays after a session ends.
3. **Opt-in.** A session works on a proposal only when the user asks for it:
   "make a proposal for X" or "work on the X proposal". After that request, the
   agent knows what to do. The `af proposal` command output tells it the rules.
4. **Edits go both ways.** The user edits the files directly. The agent learns
   about those edits by push (af sends them when the user stops typing and the
   agent is idle) and by pull (the agent asks for them).
5. **Fast to read and edit.** The web UI has a Docs tab that opens immediately
   beside the agent. It shows rendered markdown that you can edit in place, in
   the same way as Obsidian's live preview.

## Non-goals

- A proposal has no status (draft, accepted, implemented). `af proposal list`
  sorts by the last edit.
- The TUI does not get an editor. The Docs tab is web only.
- No real-time co-editing between two humans. There is no CRDT.
- The files are not in the project's git history. A later
  `af proposal export` can copy them into the repo.
- The name is "proposal", not "plan", so that it is not confused with Claude
  Code's plan mode.

## Callouts

Agents and users use a small set of callouts in the documents:

| Callout | Written by | Meaning |
|---|---|---|
| `> [!question]` | agent | It needs a decision from the user. The user answers under it. |
| `> [!todo]` | user | An instruction. The agent does it, then deletes the callout. |
| `> [!decision]` | either | A settled point. The agent does not change it without asking. |

## Delivery

1. **Storage, CLI and skill.** The proposal store, `af proposal`, the
   templates, and the agent instructions. This step works with any editor.
2. **Docs tab.** The web editor, daemon file routes, live reload, and a
   three-way merge.
3. **Push.** Auto-send after 10 s without edits while the agent is idle, the
   Hold chip, callout styles, and the open-question count.

See `technical.md`, `api.md`, `ui.md` and `future.md`.
