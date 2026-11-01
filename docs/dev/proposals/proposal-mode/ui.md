# Proposal mode: UI design

The web UI only gets this tab, in step 2 and step 3.

## Layout

```
┌───────────────────────────┬───────────────────────────────────────────┐
│ Agent                     │ Docs · auth-rework                        │
│                           │ goals  technical  api  +            ● 2?  │
│  (live terminal)          ├───────────────────────────────────────────┤
│                           │ # API design                              │
│                           │                                           │
│                           │ > [!question] Should tokens expire?       │
│                           │                                           │
│                           │ ## Endpoints                              │
│                           │ …                                         │
│                           │                     Sending in 4s · Hold  │
└───────────────────────────┴───────────────────────────────────────────┘
```

- The Docs tab is a normal tab. When `start` or `attach` creates it, it opens
  split to the right of the Agent tab. It uses the existing pane-split code.
- **Doc strip:** one sub-tab for each file, in template order and then
  alphabetical order. The **+** menu lists the templates that are not used
  yet, and **Blank…**.
- **Header right:** a count of open `[!question]` callouts. Click the count to
  go to the next question.

## Editor

- CodeMirror 6 with live-preview decorations. Headings, emphasis, links,
  lists, tables and callouts show rendered. The line that has the cursor
  shows its markdown source.
- Do not use a WYSIWYG editor that converts markdown to a document tree and
  back (TipTap, Milkdown). It changes formatting, and the agent then sees
  edits that nobody made.
- Callout colours come from design tokens: question, todo and decision each
  get a colour.
- Load CodeMirror only when a Docs tab first opens, so the main web bundle
  stays small.

## Saving and live reload

- Autosave runs 500 ms after the last keystroke. A save status shows in the
  header: "Saved", "Saving…", or "Merged agent edits".
- When the agent changes a doc that you have no unsaved edits in, the tab
  reloads it. The cursor and scroll position stay where they were.
- When a merge has a conflict, a banner shows "The agent changed this section
  too · Keep mine · Take theirs · Show both".

## Push chip (step 3)

The chip renders the `push` status from `GetProposal` and then each
`proposal.push` event for the tab's session (`api.md`, Push).

| `state` | Chip |
|---|---|
| `countdown` | "Sending to agent in 4s · Hold". The tab counts down from `due_in_ms`, starting when it receives the status, not to `due_at`: the browser clock can be different from the daemon clock. |
| `waiting_agent` | "Waits for agent". It sends when the agent becomes idle and 10 s have passed since your last edit. |
| `held` | "Held · Send now". The next edit starts the countdown again. |
| `sent` | "Sent to agent". |
| `idle` | Nothing in auto mode. |

- **Hold** calls `HoldProposalPush`.
- **Send now** calls `SendProposalChanges`. In Manual mode the chip is always
  a **Send now** button, and it is active when `unsent` is more than 0.

## Copy

Follow the repo copy rules: sentence case, `…`, and ` · ` as the separator.
