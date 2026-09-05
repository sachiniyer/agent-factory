# Interface design

This is P0 of [the polish program](https://github.com/sachiniyer/agent-factory/issues/3906),
implemented by [#3907](https://github.com/sachiniyer/agent-factory/issues/3907).
Web is the primary surface. The TUI is a smaller operator surface, not a parity target.
The web remains vanilla TypeScript with no new runtime dependencies. This document
sets the contract for P2 and P5; this slice changes no live screen, user palette,
terminal output, shortcut or session behaviour. The [style guide](style-guide.md)
is the only consumer of the new CSS. The new Go theme is not installed in `ui`.

## Evidence and reading order today

Audit baseline: the checkout from master used for this issue, September 5, 2026.
The observations below come from committed stills and source, not eye tracking,
usage telemetry or timing measurements. P1 supplies measurements before application.
The six web recorder beats exist in light and dark; the TUI poster and two older
SVG stills do not provide the same coverage. We preserve that distinction in the guide.

| Screen | What the eye reads first today | Where hierarchy, density and colour fail | Rule for the next slice |
| --- | --- | --- | --- |
| Web dashboard | The bold selected session and cyan pane outline, then the product wordmark and terminal transcript | Two top rows of chrome compete with a very sparse transcript. All three demo rows say “Needs you · pane changed · …”; the clipped detail costs a line without giving a complete reason. Grey surfaces and strong rules give every region similar weight. | Session name and actionable state lead; disclose mechanical detail. Context stays quiet. |
| Web new session | The modal title and focused prompt; dimming successfully separates the form | Six full-width fields give inherited program, backend and account defaults the same weight as the prompt. The user must scan all fields before Create. | Prompt and title lead; inherited choices remain visible as a compact summary with an explicit edit disclosure. Never hide an override or account ambiguity. |
| Web agent tab | The selected rail row and outlined terminal, then the transcript | Session title, Agent tab, New tab and Handoff occupy permanent chrome even for a single tab. Dark outlines remain prominent around a large empty terminal area. | One line identifies session, tab and keyboard owner. Keep controls reachable without making them peers of output. |
| Web review | The diff text, then the underlined diff tab and PR badge | The review context is useful, but the surrounding rail repeats non-review details and the header still advertises Handoff. The uncoloured recorder diff is agent output, not a shell colour defect. | Preserve the PR link next to review context and do not recolour terminal bytes. |
| Web Tasks | Bold task names, then the far-right action cluster, particularly red Remove | Two tasks each repeat Disable, Edit, Trigger and Remove; the destructive control receives emphasis before there is any intent to delete. Raw cron and timestamps are less scannable than the next occurrence. | Name, next run and failure lead. Secondary actions belong to task selection or a menu. |
| Web Config and accounts | The page-wide fields and Save buttons, then Accounts | A serialized theme fills a narrow input; registration fields for several agents fill most of the lower viewport. Paths and explanatory text compete with the state being managed. | Keep purpose beside each key; show account identity and login state first, disclose registration. Preserve full actionable paths. |
| TUI sessions and preview | The bright green agent diff and long preview title, then the cyan brand chip and selected child tab | Automations with zero tasks and Projects with one project reserve large rail sections. The poster’s long preview title repeats origin/tab identity. Muted hints are difficult to read; focus, preview, selection and keyboard ownership have separate border colours. | Give cells back to sessions and terminal output; one frame vocabulary with explicit keyboard-owner text. |
| TUI Tasks | The selected task and highlighted shortcut hints in the existing Tasks SVG | The source puts trigger and delivery in every header; long rows truncate. Selected details expand, which is useful, but selection uses warning colour, competing with task failures. | Keep selected expansion and always-visible failures; selection uses selection fill and a cursor, never warning. |
| TUI Config | Source-defined title, uppercase section headings and coloured selected key | No recorder still is committed. `config_pane.go` defines ANSI colours independently and uppercases headings; this can disagree with the active theme and copy contract. | Sentence case sections, semantic colour roles, full wrapped errors and daemon identity before path. |
| Overlays, help and recovery | Web modals foreground their title; TUI overlays replace the keyboard owner | Source inspection finds text, search, project, selection, prompt, confirmation and help variants. There are no committed recovery-screen recorder beats. Their frame and hint variations require re-learning the same exit action. | One overlay frame and one focus-return contract; preserve operation-specific content and confirmations. |

### Source trail

The web audit follows `web/src/index.ts` (event, focus and modal ownership),
`layout.ts` (binary splits, one tab per pane), `frame.ts` (PTY codec, not visual
chrome), `nav.ts` (rail/terminal/modal precedence), `modals.ts` (persistent form
nodes and inline errors), `styles.css` (three copies of theme declarations,
4px spacing, phone drawer and selected-session header), `status.ts` (liveness
and operator labels), and `ui.ts` (shell and rail). Transport and split identity
are constraints to preserve, not things a design system should replace.

On the TUI, `app/home_view.go` composes rail, Automations, Projects, workspace,
status bar and overlays; `app/render.go` places overlays and dividers;
`app/help.go` provides the full keyboard reference. `ui/sidebar_render.go`,
`ui/tabbed_window.go`, `ui/task_pane.go`, `ui/config_pane.go`, `ui/statusbar.go`
and `ui/menu.go` establish the component inventory. `ui/theme.go` rebuilds
lipgloss styles from the configured palette; `app/theme.go` propagates it.
`ui/tree/render.go` and web `status.ts` already agree on the no-glyph running
state. P5 must preserve configured-theme compatibility while replacing local
colour literals; the new package is a foundation, not a second active theme.

The [recorder documentation](../dev/demo-assets.md) identifies real UI and seeded
agent stand-ins. The [style guide](style-guide.md) pairs specimens with those
stills and marks missing capture coverage. The TUI Sessions/Tasks SVGs are
supplementary evidence of an older palette, not recoloured recorder outputs.

## Principles

1. **Work owns the screen.** On Sessions, the reading order is selected session,
   state requiring action, active terminal. On Tasks it is task, next occurrence,
   failure. On Config it is key or account, current value or state, applicable action.
   In a dialog it is purpose, input, submit. Empty and error states lead with the
   condition and one next step. Context must remain available without competing.
2. **Selection, keyboard ownership and liveness are independent.** A selected row
   has selection fill and a cursor/active marker. Keyboard ownership has a focus
   outline and explicit text. Green means ready for input, not “this pane has focus”.
   Enter hands input to the terminal; ctrl+] returns it. Escape continues to reach
   the agent when it owns input. A modal owns input until it closes.
3. **Density follows the task.** A session gets a name line and at most one useful
   summary line on web; TUI starts with one row and expands the selected item.
   Long titles truncate with `…` and have a full-name route through selection or
   search. Errors and paths needed for an action wrap or have a full-detail view.
   Do not spend a row on an empty section or a repeated default explanation.
4. **Colour reinforces words and shapes.** Every state has a readable label.
   Muted text still meets contrast requirements. A warning colour never means
   ordinary selection. Focus remains visible without colour perception.
5. **Stable structure is part of speed.** Preserve focused inputs, terminal nodes,
   selected identities and pane sizes during updates. The phone drawer overlays
   the terminal instead of resizing it. Avoid status-driven layout movement.
   P1 measures these constraints; P0 does not claim latency improvements.
6. **A smaller TUI is deliberate.** Preserve navigation, attach, prompt, lifecycle,
   task operations and configuration access; reduce persistent secondary chrome.
   Web retains richer forms, split manipulation and account setup. No cut removes
   an operation without a documented keyboard or web/CLI route.

## Component inventory

These are the complete component families covered by the guide. Variants share
these contracts rather than receiving separate palettes.

| Family | Web boundary and variants | TUI boundary and variants | Required behaviour |
| --- | --- | --- | --- |
| Rail | `ui.ts`, `filter.ts`, `project.ts`: session row, selected row, archived group, filter, project switcher, phone drawer | `sidebar_render.go`, `tree/`: section, session, child tab, selected and archived rows | Stable identity; name before detail; one state treatment; full name available. |
| Header | AppShell view navigation, project context, connection, install/theme/disconnect tools | Rail header, project context, workspace title | Active view and project always recoverable; secondary tools disclosed on narrow screens. |
| Terminal chrome | `split.ts`, `ui.ts`, `terminal.ts`: ordinary, selected, keyboard-owned pane, split divider | `tabbed_window.go`, `workspace.go`: ordinary, focused, interactive and preview | One title line and one focus contract; preserve PTY colours, geometry and input ownership. |
| Tabs and review | Agent, shell, editor/web tab, active tab, close, add, rename, split and PR link | Child tab tree, tab jump and pane header | Selected tab visible without colour; keyboard route to every tab; preserve tab-specific capabilities. |
| Dialogs and overlays | `modals.ts`, Tasks forms, directory picker, config assistant and account login | `ui/overlay/`, `app/handle_overlay.go`: create, prompt, search, pickers, confirmation, help | Title, fields, inline error, one primary action; busy copy static; preserve input and return focus. Destructive confirmation names the target and consequence. |
| Tasks | `tasks.ts`: enabled, disabled, error, selected, create/edit, trigger | `task_pane.go`, `task_pane_edit.go`, `automations.go` | Name and next run first; no run-now action for a watch; failure remains visible without selection. |
| Config and accounts | `ui.ts`, `config.ts`, accounts and assistant/login overlays | `config_pane.go`, `config_pane_accounts.go` and assistant | Key, purpose, value and feedback stay together. Accounts are identities, not precedence-chain keys. Preserve remote daemon identity. |
| Buttons, fields and menus | Primary, secondary, destructive, disabled, focus; text, select, checkbox, textarea; project/filter/tab menus | Cursor, editable field, checkbox, picker, action hints | Visible labels; focus independent of hover; disabled/busy readable; capability-aware actions. |
| Empty states | Zero sessions, no project, zero tasks/accounts | Empty workspace/list and too-small-terminal fallback | One next action; distinguish empty data from a failed load. No dead controls. |
| Errors and notices | No daemon, expired login, failed mutation, saved/restart notice | Error box, pane notices, failed action, unavailable accounts | Explain consequence and next action; preserve input; wrap the actionable text; details remain available. |
| Help and status bar | Keyboard guidance and connection text | `menu.go`, `statusbar.go`, `app/help.go` | Context-valid shortcuts; escape route survives narrow widths; full help discoverable. |

P2 should extract shared chrome primitives from `ui.ts`/`modals.ts` into a small
component module boundary, still using `createElement` and event listeners. The
state/transport modules stay pure. P5 consumes shared lipgloss roles in existing
component boundaries instead of adding a parallel rendering tree.

## Colour system and liveness

`design/tokens.json` is the sole source for the staged contract. The existing Nord
family is retained to avoid an unsupported brand change; roles are reduced and
contrast is checked, including selected rows. Light/dark twins are generated,
never hand-maintained copies. All exact values appear in the [token catalogue](style-guide.md#tokens-in-both-themes).

| Role | Use |
| --- | --- |
| canvas · surface · raised | Workspace, grouped chrome, and dialogs/menus respectively. Separation comes from layout first. |
| ink · muted | Main content and secondary context. Muted is not an excuse for unreadable instructions. |
| accent · on-accent | Primary action and its label; accent can mark active navigation. |
| selection | Selected row fill paired with ink, plus a cursor or active marker. |
| border · focus | Control outline and keyboard-owner outline. Do not frame every content row. |
| danger | Failed operation or destructive confirmation. Routine destructive menu entries do not dominate the screen. |
| running · ready · lost · dead · archived · limit-reached | State-specific label and glyph colours; equal semantics on both surfaces. |

| State | Canonical glyph | Light | Dark | Operator meaning |
| --- | --- | --- | --- | --- |
| Running | Empty string | running | running | Working; no indicator. In-flight operations and unset liveness also suppress the glyph. |
| Ready | `●` | ready | ready | Ready for input; the only green liveness indicator. |
| Lost | `◌` | lost | lost | Cannot locate the process; inspect connection/restore status. |
| Dead | `○` | dead | dead | Process exited; inspect before restarting. |
| Archived | `▧` | archived | archived | Retained history; restore to resume. |
| Limit reached | `◆` | limit-reached | limit-reached | Usage limit; wait or choose an eligible account. |

The table references token names, so hex values cannot drift from the generated
catalogue. “One glyph per state” includes the deliberate empty running glyph:
[#1766](https://github.com/sachiniyer/agent-factory/issues/1766) explicitly forbids
any running dot, spinner or pulse. Reserve the TUI's blank status cells for
alignment. Web may use equivalent vector shapes when it applies the contract.
Keep labels accessible and preserve deleting/lost/limit/remote qualifiers; token
mapping does not replace the existing liveness/in-flight-op resolution logic.

The generator checks normal text and state labels at 4.5:1 on canvas, surface,
raised and selection; focus/control boundaries at 3:1; primary button text at
4.5:1. These are contract checks for the generated palette, not a certification
of existing screens or user-supplied colours. Terminal content remains owned by
the agent. P2/P5 must test low-colour terminals and custom palette overrides.

## Type, spacing and radii

Use system UI fonts for web chrome and the system monospace stack for paths,
code and terminals. No font download is necessary. The web scale is caption
12px, body 14px, title 16px, display 20px at the default root size, with 1.5 line
height and weights 400/600. Caption is metadata, not primary action text.
Headings use sentence case and ordinary tracking. The TUI inherits the user's
font and cell dimensions: size tokens become one row, with bold for headings.
It cannot implement pixel typography and must not pretend otherwise.

The web grid is 0, 4, 8, 12, 16, 24 and 32px. Small gaps group related controls;
16px is the normal panel inset; 24/32px separate sections. Minimum web control
height is 44px for touch; desktop compact rows can retain density by keeping
secondary actions in disclosures. At phone width preserve terminal space and
put context in the existing drawer. Never shrink tap targets to fit labels.

TUI spacing is explicitly mapped by role in the source: small horizontal gaps
are one cell, panel insets two cells, section gaps one or two blank rows. Do not
multiply web pixels into cells. Radius is 0 for structural frames, 4px for web
controls, 8px for dialogs; TUI uses square ordinary frames and rounded dialogs.
Focus is a 2px web outline or a one-cell TUI frame with a keyboard-owner label.
No pills are needed simply to display a count or label.

## Motion and copy

No animated indicators: no spinner, blink, pulse or cycling glyph. Connecting…,
Creating… and Running are static labels. This includes shell connection chrome;
agent-owned terminal output is not an af indicator. Motion is allowed only for
a user-caused disclosure, capped at 120ms, with 0ms under reduced motion. TUI
transitions are immediate. Background events never start motion or resize content.

Use sentence case for headings, buttons and states; use `…` for truncation and
unfinished action, and ` · ` between fragments. Explain emphasis in words instead
of caps-shouting. Preserve the literal case of identifiers, shortcuts and proper
names. Examples: “New session”, “Creating…”, “Ready · Review changes”.

## What we cut

These are application decisions for P2/P5, not removals in P0. Evidence is the
visible space cost and the existing operation path, not invented frequency data.
P1 and the cold-reader walkthrough must check that disclosure remains discoverable.

### Web cuts

| Cut | Evidence and argument | Retained route and acceptance |
| --- | --- | --- |
| Mechanical churn detail on every unselected session | Every demo row repeats “pane changed” and truncates the useful tail. It spends a line without explaining the next action. | Selected row/detail view retains reason, time and full failure. Scanning still distinguishes all six states. |
| Persistent row-level destructive buttons | Tasks repeats Remove beside every row; session archive/kill icons occupy the selected row. Destructive emphasis interrupts ordinary selection. | Selected-item actions menu, keyboard shortcuts and target-specific confirmation; no lifecycle operation removed. |
| Equal-weight default fields in create | Program/backend/account inherit defaults but receive three full input rows in the still. Prompt is the actual new work. | Compact explicit default summary with edit disclosure; account ambiguity and non-default choices always visible. |
| Permanent registration field for each agent | Config still shows empty registration inputs across the lower screen before the user chooses to register anything. | One Add account action opens agent/name fields; existing account list and login controls remain. |
| Redundant single-tab and rare-action chrome | Agent/New tab/Handoff sit beside title for a one-tab session; output is the primary reading task. | One compact tab/title row with an actions disclosure. Tab creation, handoff and split remain keyboard and pointer reachable. PR context stays visible in review. |
| Decorative count pills and repeated heavy rules | Counts are useful but their capsules and page-width rules give context the weight of controls. | Plain counts beside section labels; borders reserved for controls, focus and needed separation. |

### TUI cuts

| Cut | Evidence and argument | Retained route and acceptance |
| --- | --- | --- |
| Always-reserved empty Automations block | The poster spends several rows on zero tasks and a clipped creation hint while sessions are the operator's work. | Tasks remains reachable through its current management key and help; nonempty/failing task summaries can be disclosed. |
| Always-reserved single-project block | One project in the poster occupies another bottom rail section. It adds no choice until switching is requested. | Project name in context header and project picker; multiple projects remain searchable and switchable. |
| Repeated preview/origin/tab prose | Poster header repeats Agent and original-session identity across a long title. | Session · Tab · Preview in the common frame; origin available in details when different. |
| Separate colours for preview, selection and interactive success | Existing frame styles add purple preview and green interactive borders to liveness colours. The operator must decode colour to know where typing goes. | Selection fill/cursor and focus outline/text; preview labelled in words. Keyboard ownership always visible. |
| Long global shortcut strip | Poster footer compresses many commands into dim text; width-aware menu code already knows context and priority. | Keep current-context essentials and exit route; full `?` help retains every operation. |
| Distinct picker/confirmation/help chrome | `home_view.go` switches among many overlay renderers with the same basic focus and escape needs. | One frame/heading/hint pattern; keep distinct content, confirmation consequences and search behaviour. |
| Uppercase Config headings and private ANSI palette | Source calls `strings.ToUpper` and embeds colour indexes separately from the theme. | Sentence case and shared semantic styles; editable keys and remote/account capabilities remain. |

## Generation and ownership

JSON is chosen over TOML because the repository's Go toolchain can decode it with
`encoding/json`; the token source needs no new parser or Node runtime. Each colour
has two values and a role, each metric has a CSS value and an explicit TUI meaning,
and states bind labels/glyphs to named colours. This keeps the dimensional mismatch
between pixels and cells reviewable rather than silently converting it.

`go run ./scripts/gen-design` deterministically produces `web/src/tokens.css`,
`ui/theme/theme.go`, the identical docs CSS copy and `docs/design/style-guide.md`.
The CSS scopes variables to `[data-af-theme="light"]` and `[data-af-theme="dark"]`,
allowing both specimens on one page without restyling MkDocs. P2 can place this
attribute on its root when it adopts the contract. `theme.Colors`, `Metrics`,
`States` and `Styles` return fresh values; the live `ui.Theme` stays untouched.

`make docs` still invokes `scripts/gen-docs.sh`, now including the standalone
Go generator. `go run ./scripts/gen-design --check` compares every output without
writing. The focused `internal/designtokens` tests verify committed output,
corruption/missing-file detection, deterministic rendering, state invariants and
contrast rejection. The Docs workflow regenerates all artifacts and checks
`git status --porcelain`, including untracked outputs, before a strict MkDocs build.

Edit source tokens, `design/style-guide.tmpl` and the component specs in
`internal/designtokens/guide.go`; never edit generated files. A future component
must enter both this inventory and the guide. P1 supplies the missing recorder
matrix and budgets. P2/P5 apply this document after P1; P4 implements recovery;
P6 updates public stills. Each visual PR cites its rule or measured motivation,
retains custom-theme compatibility and includes the corresponding recorder evidence.
