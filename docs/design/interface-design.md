# Interface design

This is P0 of [the polish program](https://github.com/sachiniyer/agent-factory/issues/3906),
implemented by [#3907](https://github.com/sachiniyer/agent-factory/issues/3907).
Web is the primary surface. The TUI is a smaller operator surface, not a parity target.
The web remains vanilla TypeScript with no new runtime dependencies.
The only user-facing theme choice will be **Light / Dark / System**: two fixed
product palettes, with System selecting between them. No per-token overrides,
custom palettes or colour configuration keys. This document
sets the contract for P2 and P5. The generated tokens now supply the live web,
TUI, browser chrome and docs palettes. The [style guide](style-guide.md) displays
both twins together.

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
| Web review | The diff text, then the underlined diff tab | The review context is useful, but the surrounding rail repeats non-review details and the header still advertises Handoff. The uncoloured recorder diff is agent output, not a shell colour defect. | Keep the review context visible and do not recolour terminal bytes. |
| Web Tasks | Bold task names, then the far-right action cluster, particularly red Remove | Two tasks each repeat Disable, Edit, Trigger and Remove; the destructive control receives emphasis before there is any intent to delete. Raw cron and timestamps are less scannable than the next occurrence. | Name, next run and failure lead. Secondary actions belong to task selection or a menu. |
| Web Config and accounts | The page-wide fields and Save buttons, then Accounts | A serialized theme fills a narrow input; registration fields for several agents fill most of the lower viewport. Paths and explanatory text compete with the state being managed. | Keep purpose beside each key; show account identity and login state first, disclose registration. Preserve full actionable paths. |
| TUI sessions and preview | The bright green agent diff and long preview title, then the cyan brand chip and selected child tab | Automations with zero tasks and Projects with one project reserve large rail sections. The poster’s long preview title repeats origin/tab identity. Muted hints are difficult to read; focus, preview, selection and keyboard ownership have separate border colours. | Give cells back to sessions and terminal output; one frame vocabulary with explicit keyboard-owner text. |
| TUI Tasks | The selected task and highlighted shortcut hints in the existing Tasks SVG | The source puts trigger and delivery in every header; long rows truncate. Selected details expand, which is useful, but selection uses warning colour, competing with task failures. | Keep selected expansion and always-visible failures; selection uses surface-raised and a cursor, never warning. |
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
and `ui/menu.go` establish the component inventory. The pre-retirement
`ui/theme.go` rebuilt styles from a configurable palette; P5 now uses fixed roles.
`ui/tree/render.go` and web `status.ts` already agree on the no-glyph running
state. P2/P5 replace local colour literals with fixed roles. The pre-retirement
`config/theme.go` exposed Nord/Zenburn presets and a 19-slot custom table; the
browser also derived colours from that shared palette. Both clients now select
Light/Dark/System locally. The [#3936 contract](tui-theme-daemon-follow-up.md)
covers the remaining shared config migration and RPC retirement.

The [recorder documentation](../dev/demo-assets.md) identifies real UI and seeded
agent stand-ins. The [style guide](style-guide.md) pairs specimens with those
stills and marks missing capture coverage. The TUI Sessions/Tasks SVGs are
supplementary evidence of an older palette, not recoloured recorder outputs.

## Browser and installed-app chrome

[The design generator](https://github.com/sachiniyer/agent-factory/tree/master/internal/designtokens)
stamps the light and dark `theme-color` metas in
[the HTML shell](https://github.com/sachiniyer/agent-factory/blob/master/web/src/index.html)
from the corresponding `surface` tokens. The
[manifest](https://github.com/sachiniyer/agent-factory/blob/master/web/src/manifest.webmanifest)
can carry only one `theme_color`: it uses the **light surface**, matching the
light meta and prefers-color-scheme defaults. Its `background_color` is the light
surface too. Dark browser chrome comes from the dark meta; at runtime `theme.ts`
rewrites both metas to follow an explicit Light/Dark choice. The installed-app
splash retains the manifest's light convention.

`go run ./scripts/gen-design --check` checks both source files for drift.
`TestDesignWebChromeServedBytes` independently reads the committed `web/dist`
HTML and manifest before any JavaScript can rewrite them. The browser selftest
separately proves the post-boot theme rewrite. Docs chrome consumes the generated
palette through Material's scheme selector; embedded tab notices use the generated
Go dark surface, ink and accent roles.

## Principles

1. **Work owns the screen.** On Sessions, the reading order is selected session,
   state requiring action, active terminal. On Tasks it is task, next occurrence,
   failure. On Config it is key or account, current value or state, applicable action.
   In a dialog it is purpose, input, submit. Empty and error states lead with the
   condition and one next step. Context must remain available without competing.
2. **Selection, keyboard ownership and liveness are independent.** A selected row
   uses surface-raised and a cursor/active marker in accent. Keyboard ownership has a focus
   outline and explicit text. Green means ready for input, not “this pane has focus”.
   Enter hands input to the terminal; ctrl+] returns it. Escape continues to reach
   the agent when it owns input. A modal owns input until it closes.
3. **Density follows the task.** A session gets a name line and at most one useful
   summary line on web; TUI starts with one row and expands the selected item.
   Long titles truncate with `…` and have a full-name route through selection or
   search. Errors and paths needed for an action wrap or have a full-detail view.
   Do not spend a row on an empty section or a repeated default explanation.
4. **Colour has one prescribed job.** Every state has a readable label.
   Ink-muted is secondary metadata only, never body text, field labels or actions. A warning colour never means
   ordinary selection. Focus remains visible without colour perception.
5. **Stable structure is part of speed.** Preserve focused inputs, terminal nodes,
   selected identities and pane sizes during updates. The phone drawer overlays
   the terminal instead of resizing it. Avoid status-driven layout movement.
   P1 measures these constraints; P0 does not claim latency improvements.
6. **A smaller TUI and a fixed theme are deliberate.** Preserve navigation, attach, prompt, lifecycle,
   task operations and configuration access; reduce persistent secondary chrome.
   Web retains richer forms, split manipulation and account setup. No cut removes
   an operation without a documented keyboard or web/CLI route. Appearance has
   exactly Light, Dark and System; there is no colour editor on either surface.

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

## Component usage rules

Use these recipes on both surfaces. They are decisions, not suggestions or a
menu of variants. Every background is either surface or surface-raised; every
non-state text span is ink or, only for secondary metadata, ink-muted. There is
no third surface, selection colour, danger colour, on-accent colour, preview
colour, hover colour, shadow palette or per-component override. The [style guide](style-guide.md#rules-applied)
shows these rules beside the current screen in both themes.

### Rail and session rows

Use surface behind the whole rail, body-sized ink for names and caption-sized
ink-muted only for branch and timestamp metadata. Give rows space-2 insets and
glyphs a space-1 gap. Selection uses surface-raised, a leading accent marker and
a bold name; retain the same liveness glyph/colour inside that row. TUI selection
uses a cursor and raised row with no extra frame. Do not colour a whole session
name by state. Keep one state label, and disclose mechanical churn detail when
selected. The state glyph never animates or changes the name's starting column.

### Header and view navigation

Use surface, heading-sized ink project context and body-sized ink view labels.
Only the active view gets an accent underline and bold text. Space-3 is the
header inset; space-2 separates controls. Connection status is static ink; only
its optional detail uses ink-muted. No raised toolbar, count pills or decorative
brand colour. On a phone, the existing drawer holds project and view context.
Appearance offers exactly Light / Dark / System; the current Auto label becomes
System. System follows OS appearance on web and terminal background detection
on TUI, with dark as the fallback when detection is unavailable. It selects the
same fixed light/dark values, never a derived or user-supplied palette.

### Terminal chrome

Use surface and a border outline, with heading-sized ink for the one-line
Session · Tab title. Keyboard ownership replaces that outline with accent and
adds the word Keyboard. Green is never focus. Preview is a word in the same
title, not another frame colour. Chrome uses space-2 insets; the agent terminal
grid gets no decorative inset and retains its own ANSI output. Enter and ctrl+]
continue to transfer keyboard ownership; Escape still reaches the attached agent.

### Tabs and review

Use body-sized ink labels on surface, space-2 between tabs and no pill radius.
The active tab is bold with an accent underline. The PR link uses accent beside
the review context. In the TUI, the selected child tab uses the rail's raised
row and cursor recipe. No duplicate active-tab colour. Close, rename, tab jump
and split keep their capability-aware keyboard routes.

### Dialogs and overlays

Use surface-raised, border and radius-dialog with space-3 insets. Titles use
type-title and ink; body and field labels use type-body and ink. Space-2 groups
fields, and space-4 separates the footer. Put an inline dead-coloured failure
next to the field or operation that failed. One primary button uses accent fill
and surface text; secondary buttons use the standard control recipe below.
TUI dialogs use a rounded border and two horizontal cells of inset. Search,
project, account and directory pickers, confirmations, help and assistant/login
containers all use this frame. Preserve their specific content and terminal
ownership; preserve entered text after failure and return focus when dismissed.

### Tasks

Use surface, with body-sized ink for task names, next occurrence and action
labels. Raw cron, previous-run time and delivery detail are caption-sized
ink-muted metadata. The selected task uses the rail's raised row/accent marker;
failures use dead and remain visible even when unselected. Use space-2 row
insets and space-4 between task groups. Edit is the primary task action; disclose
run/toggle/delete. Do not offer run now for a watch. TUI selected details expand
under the task, with one blank row between groups, never a warning-coloured selection.

### Config and accounts

Use surface, heading-sized ink section titles and body-sized ink keys, values,
purposes and labels. Only paths and secondary timestamps use caption-sized
ink-muted. Fields use surface-raised, border and radius-control. Use space-3
panel insets and space-4 between Config and Accounts. Save/restart notices use
body-sized ink, not ready green; failure text uses dead. Show daemon identity
before a path and wrap paths needed for an action. Accounts get identity and
plain ink login-state text, never session liveness dots. There is one Appearance
choice, Light / Dark / System. No preset picker, serialized theme object, colour
keys, palette editor or assistant route to editing colours.

### Buttons, fields and menus

Primary buttons are accent fill with surface text. Secondary buttons and fields
are surface-raised with ink text. Use border, body type, radius-control and
space-2 padding for all; never dim labels to ink-muted. Disabled controls keep
readable ink and a dashed outline. The only destructive emphasis is dead text
in the target-specific confirmation, with the consequence written out. Focus
always adds a 2px accent outline and does not depend on hover. Web controls
have at least 44px touch height. Menus use surface-raised, border, radius-dialog
and space-2 between items. No separate hover/pressed palette: hover may underline
the label, and pressing changes the action's text if it becomes busy.

### Empty states

Use surface, type-display ink for the condition and body-sized ink for one next
step. Space-4 separates the explanation from the primary action. No illustration,
card, muted instruction or new colour. Distinguish no sessions from no project,
and empty data from unavailable data. TUI uses one bold heading row and the same
plain-language instruction; empty Automations/Projects sections do not reserve rows.

### Errors and notices

Use surface with body-sized ink for consequence and recovery instructions. Only
the failure heading uses dead; an unavailable full-screen heading uses type-display.
Space-2 groups the message and space-4 precedes the single recovery action.
“Cannot reach the daemon” leads to retry after checking it; “Login expired” leads
to sign-in. Save/restart notices use ink. Wrap actionable text and retain input;
details may be disclosed but are never the only explanation. Do not invent a
success/info/warning colour vocabulary for messages.

### Help and status bar

Use surface and body-sized ink for shortcuts and exit instructions. Ink-muted at
caption size is only for supplemental annotations. Use space-2 between fragments;
no individual key boxes, pills or liveness colours. Keep the current keyboard
owner's exit first and full help reachable. On narrow layouts, remove secondary
hints before the exit route. Connecting… is static body text, not a spinner.

## Fixed colour and liveness contract

TUI colours follow tokens within termenv's rounding (at most one step per RGB channel); the web is exact.

There are **12 colour roles per theme**: surface, surface-raised, ink, ink-muted,
border, accent, running, ready, lost, dead, archived and limit-reached. Accent
also provides focus; dead also provides failed-operation/confirmation text.
These are the only intentional reuse rules. Selection reuses surface-raised;
primary-button text reuses surface. Do not derive extra shades or allow overrides.

The dark twin follows VS Code Dark Modern with near-black surfaces and grey ink (#3971).
JSON is the source for exact values; the guide's [implementation reference](style-guide.md#implementation-reference)
contains them for auditing. Users choose a mode, not these values. Generator
validation rejects extra roles and checks ink, ink-muted, accent and all state
labels at 4.5:1 on both backgrounds, body ink at 7:1 on surface, light border
at 3:1 on both backgrounds, dark border at 1.5:1 on surface, and surface text on accent
at 4.5:1. Agent-owned ANSI output is outside this shell contract.

Light surface is near-white `#f8f9fc`; surface-raised remains the slightly darker
`#eceff4` plane, with the existing border defining dialogs and controls. Dark
planes are `#1f1f1f` and `#2b2b2b`. Ink-muted must have **at least 1.5:1 luminance
contrast against ink in both themes**, using `(Llighter + 0.05) / (Ldarker + 0.05)`;
it must also have less contrast than ink against each background, while still
meeting 4.5:1 readability. This is a product hierarchy floor, not a text-on-text
accessibility claim. A liveness role equal to ink-muted requires a nonempty,
per-theme `sharesMuted` rationale in the source. Missing or stale sharing marks
fail generation. Running and archived intentionally share muted in light only;
their labels/glyphs preserve meaning. This metadata adds no tokens or settings.

The blue accent is brightened to `#2296f3` so it clears 4.5:1 on both
backgrounds and with surface-coloured primary-button text. Every state colour
clears 4.5:1 on the new surface and remains unchanged. The light twin is unchanged.
The requested `#3c3c3c` border measures 1.494240:1 against surface, below
the unrounded 1.5:1 floor. `#3d3d3d` is the smallest neutral adjustment that passes.
The dark outline is deliberately quieter than the old 3:1 rule; its contrast
on raised surfaces is reported below without a separate threshold.

Measurements below are generated by
[the WCAG contrast report](https://github.com/sachiniyer/agent-factory/blob/master/internal/designtokens/contrast_report.go)
from [the token source](https://github.com/sachiniyer/agent-factory/blob/master/design/tokens.json).
Run `go run ./scripts/gen-design` to refresh; `--check` rejects drift. Ratios
are displayed to three decimals; validation uses unrounded values. Surface rows
show plane separation, not text readability claims.

<!-- generated contrast: start -->

| Role | Light | Dark | Light on surface | Dark on surface | Light on raised | Dark on raised |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| surface | `#f8f9fc` | `#1f1f1f` | 1.000:1 | 1.000:1 | 1.095:1 | 1.164:1 |
| surface-raised | `#eceff4` | `#2b2b2b` | 1.095:1 | 1.164:1 | 1.000:1 | 1.000:1 |
| ink | `#2e3440` | `#cccccc` | 11.863:1 | 10.264:1 | 10.836:1 | 8.817:1 |
| ink-muted | `#4c566a` | `#9d9d9d` | 7.008:1 | 6.078:1 | 6.401:1 | 5.221:1 |
| border | `#657084` | `#3d3d3d` | 4.745:1 | 1.517:1 | 4.334:1 | 1.304:1 |
| accent | `#2d6271` | `#2296f3` | 6.441:1 | 5.278:1 | 5.883:1 | 4.534:1 |
| running | `#4c566a` | `#d8dee9` | 7.008:1 | 12.201:1 | 6.401:1 | 10.481:1 |
| ready | `#405430` | `#d5e2cc` | 7.888:1 | 12.229:1 | 7.205:1 | 10.504:1 |
| lost | `#705014` | `#ebcb8b` | 7.008:1 | 10.555:1 | 6.401:1 | 9.067:1 |
| dead | `#883b43` | `#e4c8cd` | 7.244:1 | 10.551:1 | 6.617:1 | 9.063:1 |
| archived | `#4c566a` | `#d8dee9` | 7.008:1 | 12.201:1 | 6.401:1 | 10.481:1 |
| limit-reached | `#73436b` | `#dbb9d5` | 7.269:1 | 9.350:1 | 6.639:1 | 8.032:1 |

Surface text on accent: light **6.441:1**, dark **5.278:1**.
Ink/muted hierarchy: light **1.693:1**, dark **1.689:1**.

<!-- generated contrast: end -->

| State | Fixed glyph | Required colour and meaning |
| --- | --- | --- |
| Running | Empty string | running text only; no indicator. In-flight operations and unset liveness also suppress the glyph. |
| Ready | `●` | ready glyph and label; the only green liveness indicator. |
| Lost | `◌` | lost glyph and label; cannot locate the process, not merely a slow response. |
| Dead | `○` | dead glyph and label; process exited. |
| Archived | `▧` | archived glyph and label; retained history. |
| Limit reached | `◆` | limit-reached glyph and label; wait or choose an eligible account. |

The six bindings are fixed semantics, not six extra appearance controls.
[#1766](https://github.com/sachiniyer/agent-factory/issues/1766) explicitly requires
an empty running glyph. Preserve the deleting/lost/limit/remote qualifiers and
existing liveness/in-flight-op resolution. Shapes and labels carry the meaning
without colour; reserve blank status cells in the TUI for alignment.

## Type, spacing and radii

There are **five type steps, four spacing steps and two radii**, shared by every
component. These eleven metrics plus the twelve colours are the entire **23-token**
contract. Fonts, weights, line height, touch height, focus width and motion are
fixed implementation rules, not more tokens or user configuration.

| Type step | Web at default root size | Only use | TUI |
| --- | --- | --- | --- |
| type-caption | 12px | Secondary metadata | One ordinary row |
| type-body | 14px | Names, body, fields, buttons and instructions | One ordinary row; selected name bold |
| type-heading | 16px | Section and pane headings | One bold row |
| type-title | 20px | Dialog titles | One bold row |
| type-display | 24px | Empty or unavailable full-screen condition | One bold row |

Use system UI fonts for chrome and system monospace for code and terminal
examples. Weights are 400 for ordinary text and 600 for headings/selection;
web line height is 1.5. The TUI inherits the terminal font and uses ordinary/bold
text at the same cell size. These rules have no settings controls.

| Spacing step | Web | TUI mapping |
| --- | --- | --- |
| space-1 | 4px glyph gap | One horizontal cell |
| space-2 | 8px row/control inset and related-control gap | One horizontal cell |
| space-3 | 16px panel/dialog inset | Two horizontal cells |
| space-4 | 24px section separation | One vertical blank row |

No inset is simply zero, not another token. Unframed structural surfaces stay
square. Radius-control is 4px for web buttons/inputs and square for terminal
controls; radius-dialog is 8px for web dialogs/menus and a rounded terminal frame.
No pills or separate pane radius. Web focus is always a 2px accent outline;
TUI focus is an accent frame plus text. Web touch targets are at least 44px high.

## Motion and copy

No animated indicators: no spinner, blink, pulse or cycling glyph. Connecting…,
Creating… and Running are static. This includes connection chrome; agent-owned
terminal output is not an af indicator. User-caused web disclosure may take at
most 120ms, with 0ms under reduced motion. TUI transitions are immediate.
Background events never start motion or resize content. There is no motion setting
beyond respecting the platform's reduced-motion preference.

Use sentence case, `…` for truncation/unfinished action, and ` · ` between fragments.
No caps-shouting or uppercase section headings. Preserve literal identifiers and
proper names. The appearance labels are exactly “Light”, “Dark” and “System”.

## What we cut

These are application decisions for P2/P5, not removals in P0. The theme cuts
are mandated by Sachin’s direction: simplicity wins over palette compatibility. Evidence is the
visible space cost and the existing operation path, not invented frequency data.
P1 and the cold-reader walkthrough must check that disclosure remains discoverable.

### Web cuts

| Cut | Evidence and argument | Retained route and acceptance |
| --- | --- | --- |
| Palette presets, custom colour table and serialized theme field | Before retirement, `config/theme.go` exposed Nord/Zenburn presets and a custom 19-colour table with a Config editor row. These are legacy migration inputs only under the [#3936 contract](tui-theme-daemon-follow-up.md). That asked users to maintain design decisions the product should own. | Replace with Light / Dark / System in P2/P5. No colour keys or alternate route through CLI/assistant. Old colour settings are retired, not projected into the new palette. |
| Daemon-derived per-token browser palette | Before retirement, `web/src/theme.ts` transformed the configurable daemon palette into many CSS variables, adding overrides, contrast repair and extra shades to a mode choice. | Fixed generated light/dark values. System chooses one of them; remove palette projection and rename Auto to System. |
| Mechanical churn detail on every unselected session | Every demo row repeats “pane changed” and truncates the useful tail. It spends a line without explaining the next action. | Selected row/detail view retains reason, time and full failure. Scanning still distinguishes all six states. |
| Persistent row-level destructive buttons | Tasks repeats Remove beside every row; session archive/kill icons occupy the selected row. Destructive emphasis interrupts ordinary selection. | Selected-item actions menu, keyboard shortcuts and target-specific confirmation; no lifecycle operation removed. |
| Equal-weight default fields in create | Program/backend/account inherit defaults but receive three full input rows in the still. Prompt is the actual new work. | Compact explicit default summary with edit disclosure; account ambiguity and non-default choices always visible. |
| Permanent registration field for each agent | Config still shows empty registration inputs across the lower screen before the user chooses to register anything. | One Add account action opens agent/name fields; existing account list and login controls remain. |
| Redundant single-tab and rare-action chrome | Agent/New tab/Handoff sit beside title for a one-tab session; output is the primary reading task. | One compact tab/title row with an actions disclosure. Tab creation, handoff and split remain keyboard and pointer reachable. PR context stays visible in review. |
| Decorative count pills and repeated heavy rules | Counts are useful but their capsules and page-width rules give context the weight of controls. | Plain counts beside section labels; borders reserved for controls, focus and needed separation. |

### TUI cuts

| Cut | Evidence and argument | Retained route and acceptance |
| --- | --- | --- |
| Custom theme slots and preset selection | The pre-retirement `ThemeConfig` and `ui.ApplyTheme` allowed background/foreground variants, accent, success, warning, error, info, purple, selection and four pane-border overrides. A smaller operator surface does not need a colour configuration language. | Remove the custom table and Nord/Zenburn choices in P5; expose only Light / Dark / System. Use terminal detection for System and dark if unavailable. No colour config keys remain. |
| Always-reserved empty Automations block | The poster spends several rows on zero tasks and a clipped creation hint while sessions are the operator's work. | Tasks remains reachable through its current management key and help; nonempty/failing task summaries can be disclosed. |
| Always-reserved single-project block | One project in the poster occupies another bottom rail section. It adds no choice until switching is requested. | Project name in context header and project picker; multiple projects remain searchable and switchable. |
| Repeated preview/origin/tab prose | Poster header repeats Agent and original-session identity across a long title. | Session · Tab · Preview in the common frame; origin available in details when different. |
| Separate colours for preview, selection and interactive success | Existing frame styles add purple preview and green interactive borders to liveness colours. The operator must decode colour to know where typing goes. | Surface-raised/cursor and accent outline/text; preview labelled in words. Keyboard ownership always visible. |
| Long global shortcut strip | Poster footer compresses many commands into dim text; width-aware menu code already knows context and priority. | Keep current-context essentials and exit route; full `?` help retains every operation. |
| Distinct picker/confirmation/help chrome | `home_view.go` switches among many overlay renderers with the same basic focus and escape needs. | One frame/heading/hint pattern; keep distinct content, confirmation consequences and search behaviour. |
| Uppercase Config headings and private ANSI palette | Source calls `strings.ToUpper` and embeds colour indexes separately from the theme. | Sentence case and shared semantic styles; editable keys and remote/account capabilities remain. |

## Generation and ownership

JSON uses Go's standard `encoding/json`, so this repo's existing Go generation
pipeline needs no parser dependency or Node runtime. The source has twelve
light/dark colour pairs, eleven explicitly mapped web/TUI metrics and six fixed
liveness bindings. Validation enforces the 12/5/4/2 budget and rejects extra
roles/steps. Treat changing that budget as a design decision, not a customization
feature. No config, API or end-user editor exposes token values.

`go run ./scripts/gen-design` produces `web/src/tokens.css`, `ui/theme/theme.go`,
`docs/stylesheets/tokens.css` and `docs/design/style-guide.md`; it also stamps
this page's contrast report, the HTML shell metas and the manifest colors. CSS has
exactly 23 custom properties per theme, scoped to `[data-af-theme="light"]` and
`[data-af-theme="dark"]`. The docs CSS adds Material's default/slate scheme
selectors to the same declarations. System selects one of the two palettes. Go
supplies the same adaptive light/dark roles and prescribed lipgloss component
styles. The maps are internal implementation values, not user override facilities.

`make docs` calls the same `scripts/gen-docs.sh` entry point as reference/plugin
generation. `go run ./scripts/gen-design --check` compares outputs without writing.
Focused tests cover committed drift, every missing/corrupted output, deterministic
rendering, liveness invariants, contrast and extra-token rejection. CI regenerates
all artifacts, checks `git status --porcelain` including untracked outputs, and
builds MkDocs strictly. Edit source tokens and component rules, never generated output.

P1 supplies the missing recorder matrix and budgets. P2/P5 apply these component
recipes and retire theme customization; they must document migration away from
old colour settings without silently offering a compatibility palette. P4 applies
the recovery recipes; P6 refreshes public stills. Every visual PR cites its rule
or measured motivation and includes corresponding recorder evidence.
