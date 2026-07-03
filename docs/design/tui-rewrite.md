# RFC: Multi-pane TUI rewrite (#1024)

Status: **Proposed** · Author: Captain Claude · Epic: [#1024](https://github.com/sachiniyer/agent-factory/issues/1024) · Folds in: [#1025](https://github.com/sachiniyer/agent-factory/issues/1025) (mouse)

## 0. Summary

Rewrite the TUI as a multi-pane workspace that uses the full window:

- **Left rail** — instances **and their tabs** always visible as a tree; fast navigation between them.
- **Main workspace** — one or **two** focusable content panes (split view), each showing a live view of any instance tab.
- **Bottom strip** — automations (tasks) always visible.
- **Mouse support** — click to select, focus, attach, and act (#1025).

The rewrite is a new *rendering client* over the exact same daemon RPC surface established by #960: the daemon remains the sole owner/writer of session+tab state; the TUI renders a read-only projection of the `Snapshot` RPC and mutates only via daemon RPCs. Nothing in this RFC touches the daemon, `session/`, or `session/tmux/` attach machinery except where explicitly called out.

**Migration constraint (Sachin, explicit in #1024):** migrate all at once — no old/new TUI side-by-side, no toggle. This RFC stages the work as in-place refactors across ~8 PRs; every PR keeps `master` green and ships exactly one TUI, which morphs into the target layout. There is never a user-facing choice between two TUIs.

### Goals

1. Full-window, multi-pane layout: instances+tabs tree left, automations bottom, 1–2 content panes center.
2. Focus model: focus any single tab/instance, or two at once; navigate between everything with a handful of keys.
3. First-class mouse: click/scroll everywhere a key works today (#1025).
4. Preserve the #960 architecture: pure Snapshot projection + RPC mutations, zero TUI disk writes for session state.
5. Preserve the hardened attach/detach path (#598 → #601/#602 SIGKILL-bounded detach) — do not re-litigate it.

### Non-goals

- Embedded interactive terminal emulation inside panes (a real terminal multiplexer). Attach stays full-screen; panes show live read-only views. See §3.4 and Open Question 1.
- Daemon/RPC surface changes, except an optional tasks RPC (Open Question 2).
- Configurable keymaps (#1026) and hotkey ergonomics (#1027) — the new focus model should not *block* them (all bindings keep going through `keys/keys.go`), but they are separate issues.
- bubbletea v2 migration (§4.3).

---

## 1. Current architecture

### 1.1 Model and state machine

One bubbletea program (`tea.NewProgram(newHome(...), tea.WithAltScreen(), tea.WithMouseCellMotion())`, `app/app.go:32-40`) with a single god-model `home` (`app/app.go:58-134`). A 6-value `state` enum (`app/app.go:42-56`: `stateDefault`, `stateNew`, `stateHelp`, `stateConfirm`, `stateSearch`, `stateSelectProgram`) selects which overlay owns the keyboard. "Attached to tmux" is *not* a state — it is an orthogonal `attached atomic.Bool` (`app/app.go:133`) that pauses all background tmux work while the user is inside a tmux client (the #598 contention fix).

`Update` is a single large type-switch (`app/app.go:256-536`). `View` composes: sidebar ⟷ content pane via `lipgloss.JoinHorizontal`, then menu + error box via `JoinVertical`, then modal overlays via a custom `overlay.PlaceOverlay` compositor with SGR background-fade (`app/app.go:1236-1271`, `ui/overlay/overlay.go:162-254`).

### 1.2 Layout

`updateHandleWindowSizeEvent` (`app/app.go:216-242`) is the single layout authority, with hardcoded ratios: sidebar = 30 % width, content = 70 % (`app/app.go:218-219`); content height = 90 %, menu = remainder − 2 (`app/app.go:222-223`). A second 0.9 factor (`AdjustPreviewWidth`, `ui/tabbed_window.go:170-172`) carves a right buffer inside each pane, so the effective content column is ≈ `0.7·W·0.9`. Because `lipgloss.Place` never clips, every component re-implements manual truncation and a final hard line-clamp (`ui/sidebar.go:649-653`, `ui/content_pane.go:191-194`, `ui/tab_pane.go:325-332`, `ui/list.go:136-151`) — pervasive and load-bearing.

### 1.3 Panes

- **Sidebar** (`ui/sidebar.go`, 804 lines) — hand-rolled flat windowed list (no `bubbles/list`), three sections: Instances (expanded, children = instance rows), Tasks and Hooks (leaf headers with counts only, `ui/sidebar.go:762-772`). Instance *tabs* are **not** in the sidebar. Crucially, the sidebar **owns the instance data** (`instances []*session.Instance`, `ui/sidebar.go:66`) plus repo bookkeeping — it is a model, not a view.
- **ContentPane** (`ui/content_pane.go`) — mode switch (Instance/Tasks/Hooks/Empty) wrapping:
  - **TabbedWindow** (`ui/tabbed_window.go`) — tab bar sourced from the instance's real tabs (`tabLabels()`, `ui/tabbed_window.go:122-147`), one `TabPane`;
  - **TabPane** (`ui/tab_pane.go`) — renders `tmux capture-pane` content, mutex-guarded against the background refresh goroutine (`ui/tab_pane.go:59`), with a viewport-based scroll mode;
  - **TaskPane** (`ui/task_pane.go`, 905 lines) — full task manager: list + create/edit form (textinput/textarea, cron/watch validation);
  - **HooksPane** (`ui/hooks_pane.go`).
- **Menu** (`ui/menu.go`) — bottom keybinding bar; **ErrBox** (`ui/err.go`) — bottom error line.

### 1.4 Key handling

`handleKeyPress` (`app/app.go:842-908`) routes: menu-highlight animation → per-state overlay handlers (`app/handle_overlay.go`, `app/handle_input.go`, `app/help.go`) → content-pane focus (task/hook editing, `app/handle_overlay.go:84-123`) → number keys 1-9 tab jump → `keys.GlobalKeyStringsMap` (`keys/keys.go:52-83`) → `handleDefaultKeyPress` (`app/handle_actions.go:18-142`), the stateDefault action table.

### 1.5 Daemon data flow (post-#960)

- **Read**: the TUI polls `Snapshot` every 750 ms (`snapshotRefreshInterval`, `app/sync.go:70`), one fetch in flight at a time (`app/app.go:354-377`). The daemon RPC is Go `net/rpc` + gob over a unix socket (`daemon/control.go:843-879`, socket `<configDir>/daemon.sock`), strictly request/response — **no push/subscribe channel exists**. Snapshot payload = `[]session.InstanceData` (`session/storage.go:12`): title, path, branch, status, tabs (`TabData{Name,Kind,Command,TmuxName}`, `session/storage.go:38`), PR info, worktree, remote meta. `reconcileSnapshot` (`app/sync.go:282-362`) mirrors it into the sidebar's instance list: add / swap (same title, different CreatedAt) / update-in-place / remove; selection re-pinned by title. Cold start blocks on `coldStartFromSnapshot` (`app/sync.go:119-136`) with a 2-minute daemon warm-up budget.
- **Write**: all session/tab mutations are daemon RPCs via swappable seams in `app/session_control.go`: `CreateSession` (`:18`), `KillSession` (`:34`), `CreateTab` (`:46`), `CloseTab` (`:53`), `SetPRInfo` (`:62`), `ImportRemoteHookSessions` (`:38`). Mutations run in `tea.Cmd` goroutines with the seam captured on the event loop first (#960 race pattern, e.g. `app/handle_actions.go:205`).
- **Exception — tasks**: automations are *not* in the Snapshot and there is no `ListTasks` RPC. The TUI reads/writes `tasks.json` directly (`task.LoadTasksForCurrentRepo` at `app/app.go:190,629,712`; `AddTask`/`UpdateTask`/`RemoveTask` at `app/app.go:707,613,619`) and pokes `daemon.ReloadTasks` after writes (`app/app.go:641`). The #960 single-writer model covers sessions only.

### 1.6 Attach / PTY passthrough

Attach is a hand-rolled tmux-client passthrough, **not** `tea.Exec`/`tea.ReleaseTerminal` (neither appears anywhere in the tree):

1. `handleEnter` (`app/handle_actions.go:274-334`) → first-time help overlay → `attachOverlayCallback` (`app/app.go:979-1016`).
2. The callback calls `Instance.Attach`/`AttachTab` → `TmuxSession.Attach()` (`session/tmux/tmux.go:688`), which spawns `tmux attach-session -t =<name>` under a creack/pty PTY (`session/tmux/tmux.go:336-372`, `session/tmux/pty.go:18`) and wires two goroutines: `io.Copy(os.Stdout, ptmx)` and a stdin pump that scans for the detach key (default Ctrl-W, byte 23; `session/tmux/tmux.go:707-778`), plus a SIGWINCH watcher.
3. The callback then sets `m.attached=true` and **blocks the bubbletea Update loop on `<-ch`** (`app/app.go:992`) for the entire attached duration — bubbletea stops rendering; tmux owns the terminal.
4. Detach: `Detach()` (`session/tmux/tmux.go:833`) cancels ctx, closes the PTY master, then `waitForAttachDrain` (`session/tmux/tmux.go:400-461`) — the #601/#602 hardening: 1 s graceful wait → SIGKILL the attach client (recorded pid, pgrep fallback) → 2 s → abandon the goroutine rather than freeze. The callback unblocks, forces `stateDefault`, arms the slow-repaint watchdog (`app/detach_trace.go:84-187`), and emits `repaintAfterDetachMsg`; remote sessions additionally get a terminal-mode reassert escape string + `tea.ClearScreen` (`app/app.go:934-938,1005-1014`).

One tmux session **per tab** (`session/tab.go:47`), named `af_<repoHash8>_<title>[__shell|__<name>]` (`session/tmux/tmux.go:113-160`). Previews come from `tmux capture-pane -p -e -J` (`session/tmux/tmux.go:1179-1201`) driven by a 100 ms `previewTickMsg` → `selectionChanged` → off-loop `refreshPanesCmd` (`app/app.go:260-278,1053-1136`), all skipped while attached.

### 1.7 Mouse today

Wheel scroll only: `tea.MouseMsg` handling routes WheelUp/WheelDown to the content pane (`app/app.go:378-394`). No click handling, no hit-testing.

### 1.8 Tests

- `ui/` — hermetic unit tests: render `.String()` and assert with testify + `lipgloss.Width`; no golden files, no teatest; sandboxed home + config tripwire via `TestMain` (`ui/main_test.go:11-26`).
- `app/` — model-level tests plus teatest e2e (`app/e2e_test.go`, `app/real_tui_e2e_test.go`).
- `integration/` — black-box tests that build the real binary and run a real daemon + a **private isolated tmux server** (`testguard.IsolateTmux`, `integration/cli_daemon_test.go:288-343`); real-tmux attach coverage also in `session/tmux/` and `session/backend_e2e_test.go`.

---

## 2. Target design

### 2.1 Layout regions

```
┌──────────────────┬──────────────────────────┬──────────────────────────┐
│ INSTANCES        │ pane A (focused)         │ pane B (optional split)  │
│ ▾ ● api-fix      │ ┌ api-fix · agent ─────┐ │ ┌ docs-pass · shell ───┐ │
│   ├ agent      ⬤ │ │ live capture-pane    │ │ │ live capture-pane    │ │
│   ├ shell        │ │ view of the tab      │ │ │ view of the tab      │ │
│   └ btop         │ │ (read-only; Enter    │ │ │                      │ │
│ ▸ ○ docs-pass    │ │  attaches full-      │ │ │                      │ │
│ ▸ ● big-refactor │ │  screen)             │ │ │                      │ │
│                  │ └──────────────────────┘ │ └──────────────────────┘ │
├──────────────────┴──────────────────────────┴──────────────────────────┤
│ AUTOMATIONS  [✓] nightly-sweep cron 0 3 * * * · next 03:00   [✗] watch…│
├─────────────────────────────────────────────────────────────────────────┤
│ n new · N remote · t tab · Enter attach · s split · Tab focus │ q quit │
└─────────────────────────────────────────────────────────────────────────┘
```

Four regions, all always visible (subject to §2.6 minimums):

| Region | Content | Replaces |
|---|---|---|
| **Left rail** | Tree: instance rows with their tabs as expandable children. Status glyphs as today (`ui/list.go:109-119`). | Sidebar instance section (`ui/sidebar.go`) |
| **Workspace** | 1 or 2 content panes. Each pane is bound to one (instance, tab) and renders its live capture-pane view; header shows `title · tab`. | ContentPane + TabbedWindow (`ui/content_pane.go`, `ui/tabbed_window.go`) |
| **Automations strip** | Compact task rows: enabled glyph, name, trigger (cron/watch), next/last run. Focusing it expands to the full TaskPane manager (list + edit form) in place. | Sidebar Tasks/Hooks headers + ContentPane task/hooks modes |
| **Status bar** | Context-sensitive key hints (driven by focus) + error line. 1–2 rows. | Menu (`ui/menu.go`) + ErrBox (`ui/err.go`) |

The tab bar disappears: tabs live in the tree (and in the pane header), so `TabbedWindow`'s even-split tab row (`ui/tabbed_window.go:282-345`) is no longer needed. Number keys 1-9 keep jumping tabs of the selected instance (preserving the #930 muscle memory); `t`/`w` keep creating/closing tabs.

Hooks lose their persistent sidebar slot and move behind a key/click from the automations strip (they are set-and-forget; a persistent row is not warranted). The full `HooksPane` editor is kept, shown as an overlay.

### 2.2 Component tree

```
workspace (root model, app/)
├── layout.Grid          — pure region solver: W×H → []Rect          (new, ui/layout)
├── focus.Ring           — ordered focusables + active index          (new, ui/layout)
├── zones.Registry       — Rect → zoneID hit-test map, rebuilt per View  (new, ui/layout)
├── store.Projection     — THE data model (read-only projection)      (new, app/ or ui/store)
│     instances []*session.Instance   ← reconcileSnapshot (moved from Sidebar)
│     tasks []task.Task               ← tasks.json load (as today)
│     selection {instanceTitle, tabIdx}, split state, focus state
├── panes:
│   ├── tree.Pane        — left rail (renders store; owns nothing)
│   ├── content.Pane ×2  — capture-pane view (adapted TabPane)
│   ├── automations.Pane — strip + expanded TaskPane
│   └── statusbar.Pane   — hints + errors (adapted Menu/ErrBox)
└── overlays (unchanged): text, confirm, selection, search, hooks
```

Every pane implements one interface (new, `ui/layout`):

```go
type Pane interface {
    SetRect(r Rect)                  // layout tells the pane where it lives
    Focused() bool; Focus(); Blur()
    HandleKey(tea.KeyMsg) (tea.Cmd, bool)   // bool = consumed
    HandleMouse(tea.MouseMsg, Point) tea.Cmd // Point = pane-local coords
    View() string                    // exactly Rect-sized (hard-clamped)
}
```

The root model shrinks to: dispatch messages → store, route input → focused pane (or hit-tested pane for mouse), ask `layout.Grid` for rects on resize, join pane views. The 6-state overlay enum survives unchanged — overlays are modal and orthogonal to pane focus, exactly as today.

**Data-ownership fix**: today `Sidebar` owns `[]*session.Instance` (`ui/sidebar.go:66`) and `TabbedWindow`/`TabPane` hold instance pointers. The rewrite moves ownership into a single `store.Projection` that `reconcileSnapshot` (`app/sync.go:282`) writes and every pane reads. Panes become stateless views + local UI state (scroll offset, expansion). The sync loop, cold start, and mutation seams (`app/sync.go`, `app/session_control.go`) carry over **unchanged** — this rewrite deliberately does not touch the #960 data path.

### 2.3 Focus model

- A **focus ring**: `tree → pane A → pane B (if split) → automations`. `Tab`/`Shift-Tab` cycles; direct jumps via `ctrl+h/l` (tree/workspace) — final defaults deferred to #1026/#1027. The status bar re-renders per focus (the existing menu already does per-state hints, `ui/menu.go:226-283`).
- **Selection vs focus**: tree selection (which instance/tab is highlighted) is separate from pane focus (which region gets keys). Moving the tree selection retargets **pane A** live — this is what makes "navigating between instances fast": j/k in the tree flips pane A's view instantly, since capture-pane refresh is already selection-driven (`app/app.go:1053-1110`).
- **Split**: `s` on a tree row (or pane header click) opens it in **pane B**; `s` with focus in a pane swaps A↔B; `x`/`w` on pane B closes the split. Pane B is **pinned** — it keeps its (instance, tab) binding while the tree drives pane A. Split is vertical (side-by-side) only; horizontal split is cut for scope (Open Question 4).
- **Enter** on a tree row or focused pane attaches (full-screen, §2.4). On detach, focus returns exactly where it was.
- All existing stateDefault actions (`app/handle_actions.go:18-142`) keep working and become selection-relative: kill, PR open/copy, new/close tab, scroll.

### 2.4 Attach / PTY in a multi-pane world

**Decision: attach stays full-screen takeover.** Embedding a live interactive tmux client *inside* a pane requires a client-side terminal-state machine (cursor, scroll regions, SGR, alternate screen) — i.e. a vt100 emulator — plus key forwarding and per-pane tmux resize. bubbletea v1 has no such widget; the candidates (`hinshun/vt10x`, `charmbracelet/x/vt` — experimental) add a heavy dependency to render what tmux already renders better, and every keystroke would round-trip through the contended tmux server that caused the #598 saga. The repo norm is dependency-lean; the payoff does not justify the risk. Instead:

- Panes show **live read-only views** at the existing 100 ms cadence — a two-pane split is two live terminals side by side, which covers the "watch two agents at once" use case.
- **Enter attaches full-screen**, exactly as today: same `TmuxSession.Attach()` PTY passthrough, same Update-loop block on `<-ch` (`app/app.go:992`), same `attached` pause of all background capture work, same SIGKILL-bounded detach (`session/tmux/tmux.go:400-461`) and slow-detach watchdog. **Zero changes to `session/tmux/`.** This path survived three rounds of production hardening (#598 → #601/#602, #683, #845, #975, #1065); the rewrite treats it as a black box behind one narrow seam (`attachOverlayCallbackFn`, `app/app.go:952`).
- On detach the workspace repaints via the existing `repaintAfterDetachMsg` flow, restoring the pre-attach focus/split state (stored in `store.Projection`, so it survives trivially).
- The remote-session terminal reassert (`app/app.go:934-938`) carries over verbatim.

If Sachin wants true embedded interaction later, it slots in as a *new pane kind* behind the same `Pane` interface without disturbing this design (Open Question 1).

### 2.5 Mouse model (#1025)

bubbletea v1.3.5 delivers `tea.MouseMsg` with cell coordinates (cell-motion mode is already enabled, `app/app.go:36`). The missing piece is hit-testing, solved by the `zones.Registry`: during `View()`, each pane registers rectangles for its interactive rows/targets (`zoneID` = e.g. `tree:instance:api-fix`, `tree:tab:api-fix:2`, `pane:A:header`, `auto:task:nightly-sweep`, `status:key:quit`). The root model resolves every `MouseMsg` to (pane, local point) and dispatches. Hand-rolled (~150 lines + tests) rather than `bubblezone` — it fits the repo's dependency-lean norm and our Rect model, and avoids ANSI-marker post-processing of every frame.

Interactions:

| Gesture | Effect |
|---|---|
| Click tree instance/tab row | Select (retargets pane A) |
| Double-click tree row / click focused pane body | Attach |
| Click `▸`/`▾` glyph | Expand/collapse instance's tabs |
| Click pane header | Focus that pane; click swap/close glyphs in header for split ops |
| Click task row | Focus automations, select task; double-click opens editor |
| Click status-bar hint | Runs that action (menu already knows its bindings, `ui/menu.go:234`) |
| Wheel | Scrolls the region under the cursor (today it scrolls the content pane regardless of position, `app/app.go:378-394`) |
| Click overlay buttons (y/n, list rows) | Equivalent key |

Terminal-level mouse reporting inside an *attached* session is unaffected: while attached, bubbletea is blocked and tmux owns stdin, so tmux's own `mouse on` (`session/tmux/tmux.go:289`) applies — same as today.

### 2.6 Resize handling

`layout.Grid` becomes the single sizing authority (replacing the scattered 0.3/0.9/`AdjustPreviewWidth` math, `app/app.go:216-242`, `ui/tabbed_window.go:170-172`):

- Left rail: `clamp(24, 30 %·W, 44)` cols. Automations strip: 3 rows (1 when W or H is tight). Status bar: 2 rows. Workspace: remainder; split divides it evenly with a 1-col divider.
- Every pane hard-clamps its own output to its Rect (the existing per-pane clamp discipline, §1.2, becomes a tested contract of the `Pane` interface — a shared `layout.ClampToRect` helper replaces the five ad-hoc implementations).
- **Degradation ladder** as the terminal shrinks: < ~110 cols → split collapses to pane A (B's binding retained, restored on grow); < ~80 cols → automations strip collapses to a 1-line summary; < ~60×15 → single-pane + tree only; below hard minimum → the existing fallback banner (`ui/fallback.go:25`).
- Attach resize is already handled by the SIGWINCH watcher in `session/tmux` (§1.6) — untouched.

---

## 3. Bubbletea approach & libraries

### 3.1 Single model, restructured — not a framework

Stay with **one `tea.Program`, one root model**. What changes is internal decomposition: the `home` god-model (1271-line `app.go`, layout + key routing + attach + selection + overlay plumbing) becomes the thin root described in §2.2. No compositor framework is needed — `lipgloss.JoinHorizontal/JoinVertical` composition (as `View()` does today, `app/app.go:1236-1271`) is sufficient when every pane emits an exactly-Rect-sized block. The custom `overlay.PlaceOverlay` compositor (`ui/overlay/overlay.go:162`) is kept as-is for modals.

The blocking-attach trick (§1.6) *requires* everything to live in one Update loop we can deliberately block — a strong reason not to adopt multi-program or goroutine-per-pane architectures.

### 3.2 Dependencies

**No new dependencies.** bubbletea v1.3.5, lipgloss v1.1.0, bubbles v0.20.0 (spinner, viewport, textinput, textarea, key — all already in use) cover everything. Hit-testing, layout grid, and the tree are hand-rolled (~500 lines total, all pure and unit-testable). Explicitly rejected: `bubblezone` (ANSI-marker scanning per frame; our Rect registry is simpler), `bubbles/list` (the windowed tree with multi-line rows and section headers doesn't fit its model — same conclusion the current sidebar reached), vt10x/x-vt (§2.4).

### 3.3 bubbletea v2 — considered, rejected for this rewrite

v2 improves the mouse/keyboard API but is a breaking migration across every Update signature and the teatest suite, orthogonal to the layout goals. Doing both at once doubles the risk of the cutover. Revisit after the rewrite settles; the `Pane` interface localizes a future v2 migration to the root model and message types.

---

## 4. Phased PR plan

Strategy: **in-place morph**. `master` always contains exactly one TUI, always usable, always green (`go build`, `go test`, lint, deadcode). Early PRs land pure, unit-tested infrastructure and data-ownership refactors with no visual change; the middle PRs each change one visible region; one PR is the layout cutover; the last deletes the leftovers. This satisfies "migrate all at once" (no dual TUI, no toggle — users just see the TUI evolve over ~3 releases) while keeping each PR reviewable.

Sizes are rough production-code deltas excluding tests.

| # | PR | Scope & delivery | Files touched | Risk |
|---|---|---|---|---|
| 1 | `tui: layout engine, Pane interface, focus ring, zone registry` | New `ui/layout` package: `Rect`, `Grid` solver + degradation ladder, `Pane` interface, `focus.Ring`, `zones.Registry`, `ClampToRect`. Pure logic + exhaustive unit tests (property-style: no overlap, exact cover, min sizes). **Not yet wired** — kept alive by its own tests (`deadcode -test` treats tests as roots). ~600 lines. | New: `ui/layout/*` | **Low** — no behavior change. |
| 2 | `tui: extract snapshot projection store; panes become views` | New `store.Projection` owning instances/tasks/selection; `reconcileSnapshot` (`app/sync.go:282-427`) writes the store; `Sidebar`, `TabbedWindow`, `TabPane`, `ContentPane` read it instead of owning `[]*session.Instance` / instance pointers. Visuals identical. | `app/sync.go`, `app/app.go`, `ui/sidebar.go`, `ui/content_pane.go`, `ui/tabbed_window.go`, `ui/tab_pane.go` + their tests | **Medium-high** — touches the #960 reconcile path and the `TabPane.mu` concurrency contract (`ui/tab_pane.go:59`); mitigated by the existing dense test suite (`app/sync_test.go`, `app/snapshot_test.go`, `ui/preview_test.go`) which must pass unmodified in assertion content. |
| 3 | `tui: left rail becomes an instances+tabs tree` | Rewrite sidebar instance section as the tree (tabs as children from `TabData`, expand/collapse, selection can rest on a tab). Selecting a tab in the tree drives the existing tabbed window's active tab. Still the old 30/70 layout. First visible change; useful on its own. ~New `ui/tree/` (adapting `ui/list.go` row rendering + `ui/sidebar.go` windowing); `app/handle_actions.go` nav keys. | New `ui/tree/*`; `ui/sidebar.go`, `ui/list.go`, `app/handle_actions.go` | **Medium** — selection semantics (re-pin by title, `app/sync.go:355-359`) gain a tab dimension; transient-status rows (#765 swap) need tree equivalents. |
| 4 | `tui: workspace layout cutover — full real estate, automations strip, status bar` | The big one, kept as small as possible because 1–3 pre-staged everything: root model swaps `updateHandleWindowSizeEvent` + `View()` for `layout.Grid` + `Pane` composition; tab bar removed (tree/header carry tabs); menu+errbox become `statusbar.Pane`; automations strip (compact rows; focus expands to existing `TaskPane`); hooks move behind an overlay. Single pane A only (no split yet). Attach seam untouched. | `app/app.go`, `app/handle_actions.go`, `app/handle_overlay.go`, `ui/content_pane.go` (→ `ui/pane/`), `ui/menu.go`, `ui/err.go`, `ui/task_pane.go`, `ui/hooks_pane.go`, `ui/layout_height_test.go` | **High** — the user-facing cutover. Mitigations: teatest e2e (`app/e2e_test.go`, `app/real_tui_e2e_test.go`) updated in the same PR; manual verification of the full flow matrix (§5.4) before merge; degradation ladder tested at 80×24. |
| 5 | `tui: two-pane split + focus ring navigation` | Pane B: open/swap/close, pinned binding, focus ring `tree→A→B→automations`, per-focus status-bar hints, split-aware degradation. Second capture-pane refresh slot (both panes refresh off-loop per tick; both paused while attached). | `app/app.go` (root routing), `ui/layout`, `ui/pane/*`, `keys/keys.go` | **Medium** — doubles capture traffic (bounded, §5.2); focus/selection interplay needs careful teatest coverage. |
| 6 | `tui: mouse — click selection, focus, attach, actions (#1025)` | Zone registration in every pane's `View()`; root `MouseMsg` router (replacing `app/app.go:378-394`); the §2.5 gesture table; wheel routed by hit test. Closes #1025. | `app/app.go`, all `ui/pane`/`tree`/`statusbar` views, `ui/overlay/*` (clickable buttons) | **Medium** — pure additive input path; keyboard remains fully sufficient. Risk is coordinate drift vs rendered output, caught by zone-vs-render unit tests. |
| 7 | `tui: attach polish + focus restore + first-run help refresh` | Attach from either pane / tree tab rows; detach restores focus+split; help overlay (`app/help.go`) and README/docs screenshots rewritten for the new layout; remote-session paths re-verified (`app/remote_detach_reset_test.go`). | `app/handle_actions.go`, `app/help.go`, `app/app.go`, `docs/`, `README.md` | **Low-medium**. |
| 8 | `tui: delete dead old-TUI code + final sweep` | Remove `ui/tabbed_window.go` (tab bar + `AdjustPreviewWidth`), `ui/menu.go`, `ui/err.go`, `ui/sidebar.go`, `ui/content_pane.go` remnants and their superseded tests; `deadcode -test ./...` clean; CLAUDE.md project-structure section updated. Epic #1024 closes here. | Deletions across `ui/`; `CLAUDE.md` | **Low** — pure deletion once 4–7 are stable. |

Deleted at the end: `ui/sidebar.go`, `ui/menu.go`, `ui/err.go`, `ui/content_pane.go`, `ui/tabbed_window.go` (their logic reborn in `ui/tree`, `ui/pane`, `ui/statusbar`, `ui/layout`), plus the 0.3/0.9 layout math in `app/app.go`. Surviving mostly untouched: `app/sync.go`, `app/session_control.go`, `app/detach_trace.go`, all of `session/`, `session/tmux/`, `daemon/`, `ui/overlay/`, `ui/task_pane.go` (re-hosted), `keys/keys.go` (extended).

Sequencing notes: 1→2→3→4→5 is a strict chain; 6 and 7 can land in either order after 5; 8 is last. If PR 4 review reveals it is still too big, the automations strip can split out as 4b — the strip degrades to today's sidebar Tasks header until then.

---

## 5. Risks & mitigations

### 5.1 Attach/PTY regressions — the top risk

The attach/detach path is the most production-hardened code in the repo (#598, #601/#602, #683, #716, #845, #975, #1006, #1065). Mitigations: (a) `session/tmux/` is out of scope — zero diffs; (b) the app-side seam stays the narrow `attachOverlayCallbackFn` + `<-ch` block + `attached` gate, moved but not redesigned; (c) `app/attached_pause_test.go`, `app/detach_paint_test.go`, `app/remote_detach_reset_test.go`, `app/detach_watchdog_test.go` must pass with assertions intact at every phase; (d) real-tmux verification (attach → interact → detach ×10 with 5+ instances) before merging PRs 4–7.

### 5.2 tmux-server contention with two live panes

#598's root cause was capture-pane traffic contending with the interactive client. The split adds at most **one** extra capture per 100 ms tick (pane B), only while a split is open and not attached; the `attached` pause covers both panes. This is far below the pre-#598 load (which captured 2× per instance per 500 ms across *all* instances). Watchdog (`detach-slow.log`) stays armed; if contention resurfaces, pane B's cadence degrades to 250 ms — a one-constant change.

### 5.3 Performance with many instances

The tree renders more rows (instances × tabs) than the flat list. Mitigation: the tree keeps the sidebar's lazy windowing (`ui/sidebar.go:660-742`) — render only visible rows; collapse-by-default for non-selected instances keeps row count ≈ instances + selected-instance tabs. Snapshot reconcile is unchanged (already O(instances) at 750 ms). Target: 50 instances × 9 tabs with no visible jank at the 100 ms tick, verified with a synthetic-store benchmark test in PR 3.

### 5.4 Keeping the TUI usable through the cutover

Every phase ships a complete, keyboard-operable TUI. The flow matrix verified manually (dev-install on the dev box) before merging each visible-change PR: create (local+remote), name-collision, attach/detach (agent tab, shell tab, remote), tab create/close/jump, kill, search, task create/edit/run-now, hooks edit, PR open/copy, daemon restart mid-session, cold start with daemon warm-up, 80×24 terminal. This matrix becomes a checklist in each PR description.

### 5.5 Terminal-size edge cases

Historically a bug farm (`ui/layout_height_test.go`, hard clamps everywhere). Mitigations: single sizing authority (`layout.Grid`) with property tests (regions exactly tile W×H, no negative dims, ladder monotonic); the `Pane` contract "View() is exactly Rect-sized" enforced by a shared test helper run against every pane; fallback banner below hard minimum (existing `ui/fallback.go`).

### 5.6 Test strategy

- **Unit (hermetic)**: `ui/layout`, `ui/tree`, zone registry — pure-function tests, no tmux. Existing `ui/` string-assertion style carries over; the per-pane Rect-contract helper is new shared infrastructure. Race verification per dev-box constraints (`-p=1 -parallel 2`).
- **Model-level**: `app/` tests keep driving `home.Update` with synthetic messages; mouse tests inject `tea.MouseMsg` with coordinates derived from the zone registry (not hardcoded), so layout changes can't silently break them.
- **e2e**: teatest flows (`app/e2e_test.go`) updated per phase; `integration/` black-box daemon+tmux tests are layout-agnostic and must stay green untouched. A new teatest scenario per feature: split open/close, focus ring cycle, click-to-attach.
- **Mouse caveat**: real-terminal mouse reporting can't be exercised by teatest; hit-testing is covered hermetically (zone registry unit tests + injected MouseMsg), and click-to-attach is on the manual matrix.

### 5.7 Release blast radius

Users `af upgrade` into the new layout with no warning. Mitigations: PRs 4 and 5 update README + `docs/` in the same release; the first-run help overlay (seen-bitmask already exists, `app/help.go:137-169`) gets a one-time "the TUI changed" screen at the PR-4 release; keep every existing default keybinding working (additions only until #1026/#1027).

---

## 6. Open questions for Sachin

1. **Embedded interactive panes** — the RFC's end state is *live read-only* split panes with full-screen attach on Enter (§2.4). A true in-pane interactive terminal (type into two agents side-by-side without attaching) is a large follow-on (vt emulator dependency, key routing, per-pane tmux resize). Is read-only + fast attach acceptable as the #1024 end state, with embedded interaction as a possible later issue?
2. **Tasks over RPC** — automations stay disk-read (`tasks.json` + `ReloadTasks` poke) in this rewrite, matching today. Moving them behind daemon RPCs (`ListTasks`/`AddTask`/…) would complete the #960 single-writer story and fits #1029 ("daemon is the core; everything else a client"). Do that as part of this epic (one extra PR between 3 and 4), or as a separate #1029 work item? RFC recommends: separate, to keep this epic presentation-only.
3. **Automations strip scope** — current-repo tasks only (matches today's `LoadTasksForCurrentRepo`), or all repos with a repo column? RFC assumes current-repo.
4. **Split orientation** — side-by-side only (RFC), or also stacked (horizontal split)? Stacked halves the already-scarce pane height; recommend side-by-side only.
5. **Hooks placement** — RFC demotes hooks from a persistent sidebar section to an overlay reachable from the automations strip + hotkey. Any objection?
6. **Default keys for the new verbs** (`s` split, `Tab` focus ring, `ctrl+h/l` region jumps) — fine to ship as proposed and revisit wholesale in #1026/#1027?

---

## Appendix A — current file inventory (production code)

| File | Lines | Fate |
|---|---|---|
| `app/app.go` | 1271 | Shrinks to thin root model (PR 4); layout math deleted |
| `app/sync.go` | 521 | Kept; writes `store.Projection` instead of Sidebar (PR 2) |
| `app/handle_actions.go` | 491 | Rewritten incrementally (PRs 3–7): actions become selection-relative |
| `app/handle_input.go` | 186 | Kept (naming flow unchanged) |
| `app/handle_overlay.go` | 138 | Kept; content-pane focus routing replaced by focus ring (PR 4) |
| `app/help.go` | 188 | Content rewritten (PR 7) |
| `app/session_control.go` | 120 | Kept verbatim |
| `app/detach_trace.go` | 187 | Kept verbatim |
| `ui/sidebar.go` | 804 | Replaced by `ui/tree` (PR 3), deleted (PR 8) |
| `ui/list.go` | 234 | Row rendering absorbed into `ui/tree` (PR 3) |
| `ui/menu.go` | 283 | Replaced by `ui/statusbar` (PR 4), deleted (PR 8) |
| `ui/err.go` | 77 | Absorbed into `ui/statusbar` (PR 4) |
| `ui/content_pane.go` | 215 | Replaced by workspace panes (PR 4), deleted (PR 8) |
| `ui/tabbed_window.go` | 345 | Tab bar deleted; active-tab logic moves to store (PRs 4, 8) |
| `ui/tab_pane.go` | 468 | Adapted into `ui/pane` content view (PR 4) |
| `ui/task_pane.go` | 905 | Kept; re-hosted in automations strip (PR 4) |
| `ui/hooks_pane.go` | 221 | Kept; shown as overlay (PR 4) |
| `ui/overlay/*` | 849 | Kept verbatim (+ clickable zones, PR 6) |
| `keys/keys.go` | 202 | Extended (split/focus verbs) |
| `ui/consts.go`, `ui/theme.go`, `ui/fallback.go` | 78 | Kept |
