# RFC: Multi-pane TUI rewrite (#1024)

Status: **Accepted — revised 2026-07-03 (mid-epic redesign)** · Author: Captain Claude · Epic: [#1024](https://github.com/sachiniyer/agent-factory/issues/1024) · Folds in: [#1025](https://github.com/sachiniyer/agent-factory/issues/1025) (mouse) · Redesign issues: [#1087](https://github.com/sachiniyer/agent-factory/issues/1087), [#1088](https://github.com/sachiniyer/agent-factory/issues/1088), [#1089](https://github.com/sachiniyer/agent-factory/issues/1089), [#1090](https://github.com/sachiniyer/agent-factory/issues/1090)

> **Revision 2026-07-03.** Epic PRs 1–5 ([#1079](https://github.com/sachiniyer/agent-factory/pull/1079), [#1080](https://github.com/sachiniyer/agent-factory/pull/1080), [#1081](https://github.com/sachiniyer/agent-factory/pull/1081), [#1083](https://github.com/sachiniyer/agent-factory/pull/1083), [#1085](https://github.com/sachiniyer/agent-factory/pull/1085)) are merged: the layout engine, projection store, tree rail, workspace cutover, and two-pane split are live on `master`. Mid-epic, Sachin confirmed a redesign of the end state, and a spike reversed this RFC's biggest architectural call. This revision supersedes the original in five ways:
>
> 1. **Interaction model reversed** — panes are **embedded interactive terminals**, proven by the #1089 spike; the original "read-only panes + full-screen attach" decision (old Open Question 1) is superseded (§2.4).
> 2. **Two interaction modes** — nav mode vs interactive mode; while interactive, everything (including `Tab`) forwards to the agent and only `Ctrl-]` is host-reserved (§2.3).
> 3. **N-pane model** — open/close/hide any tab as a vertical-split pane; replaces the fixed pane-A/pane-B split (§2.3, #1088).
> 4. **Automations move into the left rail**, bottom-aligned below a rule — the bottom strip is gone (§2.1, #1087).
> 5. **Narrower rail, full-height content** (§2.1, §2.6, #1090).
>
> §1 still documents the *pre-epic* TUI as the historical baseline. §4 reflects what has landed and the sequencing for the remainder.

> **Amendment (2026-07-03, [#1087](https://github.com/sachiniyer/agent-factory/issues/1087)/[#1090](https://github.com/sachiniyer/agent-factory/issues/1090), per Sachin):** the automations strip is NOT a bottom strip. Automations live at the **bottom of the left rail**, under the instances tree, separated by a horizontal rule; the left rail narrowed to `clamp(22, 25 %·W, 36)` cols; the workspace content panes take the full height above the status bar. And because the rail is narrow, the full TaskPane manager (list + create/edit form) does **not** expand in place — it opens as a **centered modal overlay** (like the hooks editor), from the task-manager key or Enter on the focused section; the in-rail section is always the compact summary. References to the "bottom strip", "expands in place", and the `clamp(24, 30 %·W, 44)` rail below are superseded.

## 0. Summary

Rewrite the TUI as a multi-pane workspace that uses the full window:

- **Left rail** (narrow) — instances **and their tabs** always visible as a tree; automations bottom-aligned in the same rail, below a horizontal rule.
- **Main workspace** — full-height, 1–**N** focusable content panes (vertical splits), each an **embedded interactive terminal** bound to an instance tab: type into an agent in place, no full-screen takeover, rail always visible.
- **Mouse support** — click to select, focus, interact, and act (#1025).

The rewrite is a new *rendering client* over the exact same daemon RPC surface established by #960: the daemon remains the sole owner/writer of session+tab state; the TUI renders a read-only projection of the `Snapshot` RPC and mutates only via daemon RPCs. Nothing in this RFC touches the daemon, `session/`, or `session/tmux/` attach machinery except where explicitly called out.

**Migration constraint (Sachin, explicit in #1024):** migrate all at once — no old/new TUI side-by-side, no toggle. The work is staged as in-place refactors (5 PRs landed, remainder in §4); every PR keeps `master` green and ships exactly one TUI, which morphs into the target layout. There is never a user-facing choice between two TUIs.

### Goals

1. Full-window, multi-pane layout: instances+tabs tree and automations in a narrow left rail, N full-height content panes right.
2. Focus model: open any set of tabs as panes, focus any one of them, hide panes back to the background; navigate between everything with a handful of keys.
3. **Embedded interaction**: type into the focused pane's agent/shell directly — no full-screen attach takeover; the instances rail stays visible at all times (#1089).
4. First-class mouse: click/scroll everywhere a key works today (#1025).
5. Preserve the #960 architecture: pure Snapshot projection + RPC mutations, zero TUI disk writes for session state.
6. Preserve the hardened attach/detach machinery (#598 → #601/#602 SIGKILL-bounded detach) — reused by the embedded panes, not re-litigated.

### Non-goals

- tmux control mode (`tmux -CC`) as the embedding architecture — evaluated and rejected by the #1089 spike (§2.4).
- Daemon/RPC surface changes, except an optional tasks RPC (Open Question 2).
- Configurable keymaps (#1026) and hotkey ergonomics (#1027) — the new focus model should not *block* them (all bindings keep going through `keys/keys.go`), but they are separate issues.
- bubbletea v2 migration (§3.3) — though the #1089 input long tail is a new data point in its favor.

---

## 1. Current architecture

### 1.1 Model and state machine

One bubbletea program (`tea.NewProgram(newHome(...), tea.WithAltScreen(), tea.WithMouseCellMotion())`, `app/app.go:32-40`) with a single god-model `home` (`app/app.go:58-134`). A 6-value `state` enum (`app/app.go:42-56`: `stateDefault`, `stateNew`, `stateHelp`, `stateConfirm`, `stateSearch`, `stateSelectProgram`) selects which overlay owns the keyboard. "Attached to tmux" is *not* a state — it is an orthogonal `attached atomic.Bool` (`app/app.go:133`) that pauses all background tmux work while the user is inside a tmux client (the #598 contention fix).

`Update` is a single large type-switch (`app/app.go:256-536`). `View` composes: sidebar ⟷ content pane via `lipgloss.JoinHorizontal`, then menu + error box via `JoinVertical`, then modal overlays via a custom `overlay.PlaceOverlay` compositor with SGR background-fade (`app/app.go:1236-1271`, `ui/overlay/overlay.go:162-254`).

### 1.2 Layout

`updateHandleWindowSizeEvent` (`app/app.go:216-242`) is the single layout authority, with hardcoded ratios: sidebar = 30 % width, content = 70 % (`app/app.go:218-219`); content height = 90 %, menu = remainder − 2 (`app/app.go:222-223`). A second 0.9 factor (`AdjustPreviewWidth`, `ui/tabbed_window.go:170-172`) carves a right buffer inside each pane, so the effective content column is ≈ `0.7·W·0.9`. Because `lipgloss.Place` never clips, every component re-implements manual truncation and a final hard line-clamp (`ui/sidebar.go:649-653`, `ui/content_pane.go:191-194`, `ui/tab_pane.go:325-332`, `ui/list.go:136-151`) — pervasive and load-bearing.

### 1.3 Panes

- **Sidebar** (`ui/sidebar.go`, 804 lines) — hand-rolled flat windowed list (no `bubbles/list`), three sections: Instances (expanded, children = instance rows), Tasks and Hooks (leaf headers with counts only, `ui/sidebar.go:762-772`). Instance *tabs* are **not** in the sidebar. The sidebar is a view that renders from `store.Projection` (§2.2); it does not own instance data or repo bookkeeping.
- **ContentPane** (`ui/content_pane.go`) — mode switch (Instance/Tasks/Hooks/Empty) wrapping:
  - **TabbedWindow** (`ui/tabbed_window.go`) — tab bar sourced from the instance's real tabs (`tabLabels()`, `ui/tabbed_window.go:122-147`), one `TabPane`;
  - **TabPane** (`ui/tab_pane.go`) — renders `tmux capture-pane` content, mutex-guarded against the background refresh goroutine (`ui/tab_pane.go:59`), with a viewport-based scroll mode;
  - **TaskPane** (`ui/task_pane.go`, 905 lines) — full task manager: list + create/edit form (textinput/textarea, cron/watch validation);
  - **HooksPane** (`ui/hooks_pane.go`).
- **Menu** (`ui/menu.go`) — bottom keybinding bar; **ErrBox** (`ui/err.go`) — bottom error line.

### 1.4 Key handling

`handleKeyPress` (`app/app.go:842-908`) routes: menu-highlight animation → per-state overlay handlers (`app/handle_overlay.go`, `app/handle_input.go`, `app/help.go`) → content-pane focus (task/hook editing, `app/handle_overlay.go:84-123`) → number keys 1-9 tab jump → `keys.GlobalKeyStringsMap` (`keys/keys.go:52-83`) → `handleDefaultKeyPress` (`app/handle_actions.go:18-142`), the stateDefault action table.

### 1.5 Daemon data flow (post-#960)

- **Read**: the TUI polls `Snapshot` every 750 ms (`snapshotRefreshInterval`, `app/sync.go:70`), one fetch in flight at a time (`app/app.go:354-377`). The TUI routes all control and read calls over HTTP (`daemon-http.sock`) via `apiclient.Client` (#1592 Phase 2 PR3); the `net/rpc` control socket (`daemon.sock`) stays only for CLI/internal callers. Snapshot payload = `[]session.InstanceData` (`session/storage.go:12`): title, path, branch, status, tabs (`TabData{Name,Kind,Command,TmuxName}`, `session/storage.go:38`), PR info, worktree, remote meta. `reconcileSnapshot` (`app/sync.go:282-362`) mirrors it into the sidebar's instance list: add / swap (same title, different CreatedAt) / update-in-place / remove; selection re-pinned by title. Cold start blocks on `coldStartFromSnapshot` (`app/sync.go:119-136`) with a 2-minute daemon warm-up budget.
- **Write**: all session/tab mutations are HTTP calls via swappable seams in `app/session_control.go` that route through `apiclient.Client` (#1592 Phase 2 PR3): `CreateSession`, `KillSession`, `ArchiveSession`, `RestoreSession`, `CreateTab`, `CloseTab`, `SetPRInfo`, `ImportRemoteHookSessions`, plus task and poll-pause verbs. Mutations run in `tea.Cmd` goroutines with the seam captured on the event loop first (#960 race pattern, e.g. `app/handle_actions.go:205`).
- **Exception — tasks**: automations are *not* in the Snapshot. The TUI reads/writes `tasks.json` directly (`task.LoadTasksForCurrentRepo` at `app/app.go:190,629,712`; `AddTask`/`UpdateTask`/`RemoveTask` at `app/app.go:707,613,619`) and pokes `daemon.ReloadTasks` after writes (`app/app.go:641`). The #960 single-writer model covers sessions only; task RPCs (`ListTasks`/`AddTask`/`UpdateTask`/`RemoveTask`/`TriggerTask`) were added in #1029 PR 3, completing the CLI single-writer path. TUI direct-write+poke is a tracked follow-up.

### 1.6 Attach / PTY passthrough

Attach is a hand-rolled tmux-client passthrough, **not** `tea.Exec`/`tea.ReleaseTerminal` (neither appears anywhere in the tree):

1. `handleEnter` (`app/handle_actions.go:548`) → first-time help overlay → `beginAttachTransition` (`app/home_attach.go:54`) blanks one View frame (`attachTransitioning` flag, `app/home_model.go:99`) to clear AF footer chrome before tmux takes over (#1448), then dispatches `beginAttachMsg` → `attachOverlayCallback` (`app/home_attach.go:96`).
2. The callback calls `Instance.Attach`/`AttachTab` → `TmuxSession.Attach()` (`session/tmux/tmux.go:688`), which spawns `tmux attach-session -t =<name>` under a creack/pty PTY (`session/tmux/tmux.go:336-372`, `session/tmux/pty.go:18`) and wires two goroutines: `io.Copy(os.Stdout, ptmx)` and a stdin pump that scans for the detach key (default Ctrl-W, byte 23; `session/tmux/tmux.go:707-778`), plus a SIGWINCH watcher.
3. The callback then sets `m.attached=true` and **blocks the bubbletea Update loop on `<-ch`** for the entire attached duration — bubbletea stops rendering; tmux owns the terminal.
4. Detach: `Detach()` (`session/tmux/tmux.go:833`) cancels ctx, closes the PTY master, then `waitForAttachDrain` (`session/tmux/tmux.go:400-461`) — the #601/#602 hardening: 1 s graceful wait → SIGKILL the attach client (recorded pid, pgrep fallback) → 2 s → abandon the goroutine rather than freeze. The callback unblocks, clears `attachTransitioning`, forces `stateDefault`, arms the slow-repaint watchdog (`app/detach_trace.go:84-187`), and emits `repaintAfterDetachMsg`; remote sessions additionally get a terminal-mode reassert escape string + `tea.ClearScreen` (`app/home_attach.go:158-162`).

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
┌───────────────┬─────────────────────────────┬─────────────────────────────┐
│ INSTANCES     │ pane 1 (focused)            │ pane 2                      │
│ ▾ ● api-fix   │ ┌ api-fix · agent ────────┐ │ ┌ docs-pass · shell ──────┐ │
│   ├ agent   ⬤ │ │ embedded interactive    │ │ │ embedded interactive    │ │
│   ├ shell     │ │ terminal — Enter to     │ │ │ terminal                │ │
│   └ btop      │ │ type into it, Ctrl-]    │ │ │                         │ │
│ ▸ ○ docs-pass │ │ back to nav; rail       │ │ │                         │ │
│ ▸ ● big-refac │ │ never disappears        │ │ │                         │ │
│               │ │                         │ │ │                         │ │
│───────────────│ │                         │ │ │                         │ │
│ AUTOMATIONS   │ │                         │ │ │                         │ │
│ [✓] nightly…  │ │                         │ │ │                         │ │
│ [✗] watch…    │ └─────────────────────────┘ │ └─────────────────────────┘ │
├───────────────┴─────────────────────────────┴─────────────────────────────┤
│ n new · t tab · Enter interact · s open pane · x hide · Tab focus │ q quit│
└────────────────────────────────────────────────────────────────────────────┘
```

Three regions, all always visible (subject to §2.6 minimums):

| Region | Content | Replaces |
|---|---|---|
| **Left rail** (narrower, #1090) | Top: tree — instance rows with their tabs as expandable children, status glyphs as today (`ui/list.go:109-119`). Bottom-aligned, separated by a horizontal rule (#1087): compact automation rows — enabled glyph, name, trigger (cron/watch), next/last run. Focusing an automation row and pressing Enter opens the full TaskPane manager (list + edit form) as an overlay. | Sidebar instance section (`ui/sidebar.go`) + the PR-4 bottom automations strip |
| **Workspace** (full height, #1090) | 1–N content panes, vertical splits (#1088). Each pane is bound to one (instance, tab) and hosts an embedded interactive terminal (§2.4); header shows `title · tab`. Tabs not open as a pane keep running in the background. | ContentPane + TabbedWindow (`ui/content_pane.go`, `ui/tabbed_window.go`); the PR-5 pane-A/pane-B split |
| **Status bar** | Context-sensitive key hints (driven by focus and mode) + error line. 1–2 rows. | Menu (`ui/menu.go`) + ErrBox (`ui/err.go`) |

The tab bar disappears: tabs live in the tree (and in the pane header), so `TabbedWindow`'s even-split tab row (`ui/tabbed_window.go:282-345`) is no longer needed. Number keys 1-9 keep jumping tabs of the selected instance (preserving the #930 muscle memory); `t`/`w` keep creating/closing tabs.

Hooks lose their persistent sidebar slot and move behind a key/click from the rail's automations section (they are set-and-forget; a persistent row is not warranted). The full `HooksPane` editor is kept, shown as an overlay.

### 2.2 Component tree

```
workspace (root model, app/)
├── layout.Grid          — pure region solver: W×H → []Rect          (landed, PR 1)
├── focus.Ring           — ordered focusables + active index          (landed, PR 1)
├── zones.Registry       — Rect → zoneID hit-test map, rebuilt per View  (landed, PR 1)
├── store.Projection     — THE data model (read-only projection)      (landed, PR 2)
│     instances []*session.Instance   ← reconcileSnapshot (moved from Sidebar)
│     tasks []task.Task               ← tasks.json load (as today)
│     selection {instanceTitle, tabIdx}, open panes [{instanceTitle, tabIdx}],
│     focus state, interaction mode (nav | interactive)
├── panes:
│   ├── rail.Pane        — left rail: tree (top) + automations rows (bottom-aligned)
│   ├── termpane.Pane ×N — embedded interactive terminal (new, ui/termpane, #1089)
│   └── statusbar.Pane   — hints + errors (adapted Menu/ErrBox)
└── overlays (unchanged): text, confirm, selection, search, hooks, task manager
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

**Data-ownership fix** (landed, PR 2 #1080): `Sidebar` used to own `[]*session.Instance` (`ui/sidebar.go:66`) and `TabbedWindow`/`TabPane` held instance pointers. Ownership now lives in a single `store.Projection` that `reconcileSnapshot` (`app/sync.go:282`) writes and every pane reads. Panes are stateless views + local UI state (scroll offset, expansion; for termpanes, the PTY/emulator pair, §2.4). The sync loop, cold start, and mutation seams (`app/sync.go`, `app/session_control.go`) carried over **unchanged** — this rewrite deliberately does not touch the #960 data path.

### 2.3 Interaction and focus model

**Two modes** (Sachin-confirmed, 2026-07-03):

- **Nav mode (default).** The host owns the keyboard. `Tab`/`Shift-Tab` cycles the focus ring `tree → pane 1 → … → pane N → automations`; `1-9` jumps tabs of the selected instance; j/k moves the tree; all existing stateDefault actions (`app/handle_actions.go:18-142`) work and are selection-relative: kill, PR open/copy, new/close tab, scroll.
- **Interactive mode.** `Enter` on a focused pane (or on a tree row, opening the pane first if needed) enters the pane. From then on **all keystrokes — including `Tab` — forward down the pane's PTY** to the agent/shell. There is **no full-screen takeover**: the pane keeps its rect and the instances rail stays visible the whole time. `Ctrl-]` pops back to nav mode.

**Why `Tab` cannot be a global host key**: shells, vim, and every agent CLI need `Tab` (completion). That is exactly why focus-switching lives in nav mode only and interactive mode forwards `Tab` to the agent. The **only** host-reserved key while interactive is `Ctrl-]` (already the attach detach-key default, `DetachKeyByte`), plus at most one prefix chord — final call in #1026/#1027.

**N-pane open/close/hide** (#1088, replaces the PR-5 A/B split):

- `s` on a tree row (or in a pane) opens the selected tab as a **new vertical-split pane** to the right of the existing panes. Splits are vertical (side-by-side) only for now.
- `x` on a focused pane **hides it back to the background**: the pane disappears from the workspace, the remaining panes re-divide the width, and the tab keeps running in its tmux session — reopen it any time from the tree. Nothing is killed; closing a pane and hiding a pane are the same operation (killing tabs stays `w`, an instance action).
- Focus moves across the N open panes via the nav-mode `Tab` focus ring; there is no pinned/primary pane distinction.

**Selection vs focus**: tree selection (which instance/tab is highlighted) is separate from pane focus (which region gets keys). If the selected tab is already open as a pane, the pane header highlights; `Enter` jumps focus there and enters interactive mode. If it is not open, `Enter`/`s` opens it. On leaving interactive mode, focus stays on that pane in nav mode.

The status bar re-renders per focus **and per mode** (the existing menu already does per-state hints, `ui/menu.go:226-283`); while interactive it shows only the escape hatch (`Ctrl-] nav`).

### 2.4 Embedded interactive panes (decision REVERSED, #1089)

**Decision: panes are embedded interactive terminals — full-screen attach takeover is retired as the primary interaction.** The original RFC decided the opposite (read-only panes + full-screen attach, old Open Question 1) on the theory that a client-side vt emulator was a heavy dependency with risky rendering fidelity. A spike disproved that: branch `spike/1089-embedded-terminal`, report `tmp_docs/spike-1089-embedded-terminal.md` on that branch (~430-LOC standalone demo, validated 2026-07-02). Sachin confirmed the reversal 2026-07-03.

**Architecture A (proven — this is the design).** Per visible pane:

1. Open a PTY and run `tmux attach-session -t <sess>` on it — the exact machinery `TmuxSession.Attach()` already uses (creack/pty, `session/tmux/tmux.go:336-372`), via a new attach mode that hands the ptmx to the termpane instead of the stdin/stdout copy + terminal takeover (~50–100 LOC at the existing `ptyFactory` attach seam, `session/tmux/tmux.go:350`).
2. Feed PTY output through `github.com/charmbracelet/x/vt` into a cell grid; copy the emulator's read side back to the PTY (terminal-query replies + encoded keystrokes).
3. Render the grid to an ANSI block each frame and place it in a fixed lipgloss rect in the pane's `View()`, cursor overlaid; repaints coalesced at a ~60 fps cap.
4. While interactive (§2.3), translate focused `tea.KeyMsg`s via the emulator's mode-aware key encoder (application cursor keys, bracketed paste — no hand-written escape sequences) and forward down the PTY.
5. Resize: pane rect change → `pty.Setsize` → tmux reflows on SIGWINCH; emulator grid resized in step.

**Spike results.** vim (alt-screen, CJK/emoji), less on a 50k-line file, htop, and 10 s of `yes` fast-streaming all render and interact correctly; Ctrl-C, F-keys, and bracketed paste forward; Ctrl-] detaches cleanly, the tmux session survives, re-attach repaints. Sustained streaming costs **~0.6 % of one core at the ~62 fps cap** — and tmux is a natural flow-limiter: the attach client receives screen *redraws*, not the raw output firehose, so architecture A inherits tmux's own throttling for free.

**Architecture B (`tmux -CC` control mode) — evaluated and REJECTED.** Control mode is built for clients that replace the entire tmux UI (iTerm2): you still need a vt emulator per pane *plus* a reimplementation of layout/attach semantics. Strictly more work than A for no fidelity gain at this scale. Revisit only if "many simultaneously-live panes without N attach clients" ever becomes a real need; at 0.6 % CPU per attachment there is no pressure.

**Gotchas to carry into production** (from the spike report):

- `x/vt` is **untagged** (pseudo-version) and forces bumps of `x/ansi`, `x/cellbuf`, and `go-runewidth` (+ `ultraviolet` as a new indirect dep). **Pin `go-runewidth`** — it moves seven minor versions with East Asian width-table changes; eyeball wide-char-adjacent UI tests. Build + tests already green with the bumps on the spike branch.
- **Input fidelity has a long tail**: modified arrows (e.g. Ctrl-Up) were swallowed in the spike harness and mouse forwarding is unimplemented; both need real-terminal QA across the actual agent CLIs (Claude Code and Aider are themselves TUIs). bubbletea v1's key model is lossy for some modifier combos — a data point for §3.3, not a blocker.
- **Attach policy**: only visible panes hold live attachments; background tabs keep their tmux sessions with no client. tmux sizes a session to its smallest attached client, so an external `tmux attach` to the same session shrinks the pane — the same behavior full-screen attach has today.

**What carries over from the hardened attach path**: pane teardown reuses the SIGKILL-bounded detach drain verbatim (`session/tmux/tmux.go:400-461`, #601/#602); the detach watchdog and remote-session terminal reassert remain armed. The Update-loop block on `<-ch` (`app/app.go:992`) and the `attached` pause exist only to serve full-screen takeover; they are deleted with it in the final cleanup PR (§4).

### 2.5 Mouse model (#1025)

bubbletea v1.3.5 delivers `tea.MouseMsg` with cell coordinates (cell-motion mode is already enabled, `app/app.go:36`). The missing piece is hit-testing, solved by the `zones.Registry`: during `View()`, each pane registers rectangles for its interactive rows/targets (`zoneID` = e.g. `tree:instance:api-fix`, `tree:tab:api-fix:2`, `pane:A:header`, `auto:task:nightly-sweep`, `status:key:quit`). The root model resolves every `MouseMsg` to (pane, local point) and dispatches. Hand-rolled (~150 lines + tests) rather than `bubblezone` — it fits the repo's dependency-lean norm and our Rect model, and avoids ANSI-marker post-processing of every frame.

Interactions:

| Gesture | Effect |
|---|---|
| Click tree instance/tab row | Select |
| Double-click tree row / click focused pane body | Enter interactive mode on that pane (opening it first if needed) |
| Click `▸`/`▾` glyph | Expand/collapse instance's tabs |
| Click pane header | Focus that pane (nav mode); click the header's hide glyph to return it to the background |
| Click automation row (rail) | Focus automations, select task; double-click opens the task-manager overlay |
| Click status-bar hint | Runs that action (menu already knows its bindings, `ui/menu.go:234`) |
| Wheel | Scrolls the region under the cursor (today it scrolls the content pane regardless of position, `app/app.go:378-394`) |
| Click overlay buttons (y/n, list rows) | Equivalent key |

Mouse *inside an interactive pane* is part of the #1089 input long tail (§2.4): the emulator has `SendMouse` and wiring `tea.MouseMsg` through is straightforward, but the ownership question — does the wheel scroll the inner app or host scrollback — is a design decision made during #1089 QA. Outside the interactive pane's rect, host gestures always apply.

### 2.6 Resize handling

`layout.Grid` becomes the single sizing authority (replacing the scattered 0.3/0.9/`AdjustPreviewWidth` math, `app/app.go:216-242`, `ui/tabbed_window.go:170-172`):

- Left rail: **narrower** (#1090) — `clamp(22, 25 %·W, 36)` cols, full height. Inside the rail, the automations section is bottom-aligned: rule + **≥ ~4 rows** — ~4 is the full-mode *floor*, which grows to one row per automation (grow-to-content) up to a half-rail cap, then spills into a scrollable strip — ceded to the tree as height tightens (2-row compact summary minimum, #1560). Status bar: 2 rows. Workspace: the entire remainder, **full vertical height** (#1090 — no bottom strip); N panes divide the width evenly with 1-col dividers.
- Every pane hard-clamps its own output to its Rect (the existing per-pane clamp discipline, §1.2, is a tested contract of the `Pane` interface — the shared `layout.ClampToRect` helper replaced the five ad-hoc implementations in PR 1).
- **Pane-count fitting** (#1088): each open pane needs a minimum usable width (~40 cols). `layout.Grid` computes how many panes fit; opening one more than fits (or shrinking the terminal) auto-hides the least-recently-focused pane to the background — its binding is retained and it is restored on grow, in order.
- **Degradation ladder** as the terminal shrinks: < ~110 cols → workspace collapses toward a single pane (hidden panes' bindings retained, restored on grow); < ~80 cols → the rail's automations section collapses to a 2-row compact summary (#1560); < ~60×15 → single pane + tree only; below hard minimum → the existing fallback banner (`ui/fallback.go:25`).
- Pane resize propagates PTY winsize → tmux reflow (§2.4); the full-screen SIGWINCH watcher in `session/tmux` (§1.6) is untouched while it still exists.

---

## 3. Bubbletea approach & libraries

### 3.1 Single model, restructured — not a framework

Stay with **one `tea.Program`, one root model**. What changes is internal decomposition: the `home` god-model (1271-line `app.go`, layout + key routing + attach + selection + overlay plumbing) becomes the thin root described in §2.2. No compositor framework is needed — `lipgloss.JoinHorizontal/JoinVertical` composition (as `View()` does today, `app/app.go:1236-1271`) is sufficient when every pane emits an exactly-Rect-sized block. The custom `overlay.PlaceOverlay` compositor (`ui/overlay/overlay.go:162`) is kept as-is for modals.

The original argument for one Update loop — the blocking-attach trick (§1.6) needs a loop it can deliberately block — retires with full-screen attach (§2.4). The single-model conclusion stands anyway: each termpane's PTY read pump delivers coalesced repaint signals into the one Update loop as messages, and the nav/interactive mode routing (§2.3) wants a single keyboard authority. Multi-program or goroutine-per-pane architectures still buy nothing here.

### 3.2 Dependencies

**One new dependency family (revised 2026-07-03).** The original "no new dependencies" stance held through PRs 1–5. #1089 adds `github.com/charmbracelet/x/vt` (+ `ultraviolet` as an indirect) and forces the ecosystem bumps described in §2.4 (`x/ansi`, `x/cellbuf`, `go-runewidth` — **pin** the x/vt pseudo-version and go-runewidth). Build and tests were validated green with these bumps on the spike branch. Everything else stays as before: bubbletea v1.3.5, lipgloss v1.1.0, bubbles v0.20.0; hit-testing, layout grid, and the tree are hand-rolled (landed). Still rejected: `bubblezone` (ANSI-marker scanning per frame; our Rect registry is simpler), `bubbles/list` (the windowed tree with multi-line rows and section headers doesn't fit its model), `hinshun/vt10x` (x/vt won the spike evaluation).

### 3.3 bubbletea v2 — considered, rejected for this rewrite

v2 improves the mouse/keyboard API but is a breaking migration across every Update signature and the teatest suite, orthogonal to the layout goals. Doing both at once doubles the risk of the cutover. Revisit after the rewrite settles; the `Pane` interface localizes a future v2 migration to the root model and message types.

---

## 4. Phased PR plan

Strategy: **in-place morph**. `master` always contains exactly one TUI, always usable, always green (`go build`, `go test`, lint, deadcode). Early PRs land pure, unit-tested infrastructure and data-ownership refactors with no visual change; the middle PRs each change one visible region; the tail converts panes to the embedded-interactive model and deletes the leftovers. This satisfies "migrate all at once" (no dual TUI, no toggle — users just see the TUI evolve across releases) while keeping each PR reviewable.

### 4.1 Landed (original PRs 1–5)

| # | PR | Delivered | Merged as |
|---|---|---|---|
| 1 | `tui: layout engine, Pane interface, focus ring, zone registry` | `ui/layout`: `Rect`, `Grid` solver + degradation ladder, `Pane` interface, `focus.Ring`, `zones.Registry`, `ClampToRect`. | [#1079](https://github.com/sachiniyer/agent-factory/pull/1079) |
| 2 | `tui: extract snapshot projection store; panes become views` | `store.Projection` owns instances/tasks/selection; panes read it. #960 reconcile path preserved. | [#1080](https://github.com/sachiniyer/agent-factory/pull/1080) |
| 3 | `tui: left rail becomes an instances+tabs tree` | `ui/tree`: tabs as children, expand/collapse, tab-level selection. | [#1081](https://github.com/sachiniyer/agent-factory/pull/1081) |
| 4 | `tui: workspace layout cutover` | `layout.Grid` + `Pane` composition, tab bar removed, statusbar pane, bottom automations strip (now superseded by #1087), hooks behind overlay. | [#1083](https://github.com/sachiniyer/agent-factory/pull/1083) |
| 5 | `tui: two-pane split + focus-ring navigation` | Pane A/B split + focus ring (now superseded by the #1088 N-pane model). | [#1085](https://github.com/sachiniyer/agent-factory/pull/1085) |

### 4.2 Remaining (Captain sequencing, revised 2026-07-03)

Sizes are rough production-code deltas excluding tests.

| # | PR | Scope & delivery | Files touched | Risk |
|---|---|---|---|---|
| R0 | `docs: RFC update — embedded-interactive panes + N-pane model + automations-in-rail` | This revision. | `docs/design/tui-rewrite.md` | — |
| R1 | `tui: rail layout — automations into the rail, narrower rail, full-height workspace (#1087, #1090)` | Automations move from the PR-4 bottom strip into the left rail, bottom-aligned below a horizontal rule; task-manager expansion becomes an overlay. Rail width clamp narrows; workspace and panes take the full vertical height (§2.6). | `ui/layout` (grid regions), rail pane, `ui/task_pane.go` hosting, `app/app.go` | **Low-medium** — layout-only; degradation ladder re-verified at 80×24. |
| R2 | `tui: N-pane open/close/hide + Tab focus ring (#1088)` | Replace the A/B split with the open-pane list in `store.Projection`: `s` opens the selected tab as a vertical-split pane, `x` hides a pane back to the background (binding retained), focus ring spans tree → N panes → automations, pane-count fitting + auto-hide on shrink (§2.6). | `app/app.go` (routing), `ui/layout`, `ui/pane/*`, `keys/keys.go` | **Medium** — focus/selection/hide interplay needs teatest coverage; capture traffic scales with open panes until R3 replaces polling. |
| R3 | `tui: embedded interactive terminal panes (#1089)` | New `ui/termpane/` package (attachment lifecycle, tea→emulator key translation, grid→ANSI render with cursor overlay, bubbletea glue — ~500–700 LOC + tests); a new attach mode in `session/tmux/tmux.go` handing the ptmx to the termpane at the existing `ptyFactory` attach seam (~:350, ~50–100 LOC — the session-lifecycle machinery is untouched); `app/` wiring for nav/interactive modes (Enter / Ctrl-], §2.3); capture-pane polling retired for open panes. Dependency bumps per §3.2. **~1–1.5k LOC over 2–4 PRs, TUI-side only — daemon #960 ownership untouched.** | New `ui/termpane/*`; `session/tmux/tmux.go` (narrow), `app/app.go`, `app/handle_actions.go`, `keys/keys.go`, `go.mod` | **High** — the risky unknown is input-edge-case QA across real agent CLIs and terminal diversity (§2.4 gotchas), not rendering (proven). Real-tmux flow matrix (§5.4) before each merge. |
| R4 | `tui: mouse — click selection, focus, interact, actions (#1025)` | Zone registration in every pane's `View()`; root `MouseMsg` router (replacing `app/app.go:378-394`); the §2.5 gesture table; wheel routed by hit test; in-pane mouse forwarding decision (§2.5). Closes #1025. | `app/app.go`, rail/pane/statusbar views, `ui/overlay/*` | **Medium** — additive input path; keyboard remains fully sufficient. Zone-vs-render unit tests catch coordinate drift. |
| R5 | `tui: delete old-TUI + full-screen-attach leftovers, final sweep` | Remove the full-screen attach takeover (Update-loop block, `attached` pause, `attachOverlayCallbackFn`), `ui/tabbed_window.go` remnants, superseded tests; help overlay + README/docs screenshots rewritten for the final layout; `deadcode -test ./...` clean; CLAUDE.md project-structure updated. Redesign issues close here. | Deletions across `ui/`, `app/`; `app/help.go`, `docs/`, `README.md`, `CLAUDE.md` | **Low** — deletion + docs once R1–R4 are stable. |

Deleted at the end: the remaining old-TUI files (`ui/tabbed_window.go`, `ui/tab_pane.go`'s capture view — superseded by `ui/termpane`) and the full-screen attach plumbing in `app/`. Surviving mostly untouched: `app/sync.go`, `app/session_control.go`, all of `session/` and `daemon/` (except the narrow R3 attach-mode hook in `session/tmux/tmux.go`), `ui/overlay/`, `ui/task_pane.go` (re-hosted), `keys/keys.go` (extended).

Sequencing notes: R1→R2→R3 is the intended chain (R1 is pure layout, R2 gives the pane model R3 fills with live terminals); R4 can land any time after R2, with the in-pane forwarding piece after R3; R5 is last. R3 ships as 2–4 stacked PRs (termpane package first, kept alive by its own tests; then the tmux hook + app wiring; then input-QA hardening).

---

## 5. Risks & mitigations

### 5.1 Attach/PTY regressions — the top risk

The attach/detach path is the most production-hardened code in the repo (#598, #601/#602, #683, #716, #845, #975, #1006, #1065). The original "zero diffs to `session/tmux/`" guarantee is relaxed exactly once, for R3: an **additive** attach mode that hands the ptmx to the termpane at the existing `ptyFactory` seam (`session/tmux/tmux.go:350`) — the spawn, detach-drain, and kill machinery (`waitForAttachDrain`, `killAttach`) is reused, not modified. Mitigations: (a) the R3 tmux diff stays ~50–100 LOC and additive; (b) the full-screen path remains intact and passing until R5 deletes it; (c) `app/attached_pause_test.go`, `app/detach_paint_test.go`, `app/remote_detach_reset_test.go`, `app/detach_watchdog_test.go` must pass with assertions intact while their subject exists, and termpane teardown gets equivalent drain-bound tests; (d) real-tmux verification (enter interactive → type → Ctrl-] ×10 with 5+ instances and multiple open panes) before merging each R3 PR.

### 5.2 tmux-server load with N live panes

#598's root cause was capture-pane traffic contending with the interactive client. The end state *improves* this profile: an embedded pane is one long-lived attach client receiving tmux-throttled screen redraws — the spike measured ~0.6 % of one core per pane under sustained streaming, and tmux ships only what the visible pane looks like, not the raw output (§2.4). Capture-pane polling retires for open panes in R3; it remains only wherever a non-attached preview is still rendered. Interim (R2, before R3): each extra open pane adds one capture per 100 ms tick — still far below the pre-#598 load (2× per instance per 500 ms across *all* instances). The detach watchdog (`detach-slow.log`) stays armed throughout; if contention resurfaces, pane cadence degrades to 250 ms — a one-constant change.

### 5.3 Performance with many instances

The tree renders more rows (instances × tabs) than the flat list. Mitigation: the tree keeps the sidebar's lazy windowing (`ui/sidebar.go:660-742`) — render only visible rows; collapse-by-default for non-selected instances keeps row count ≈ instances + selected-instance tabs. Snapshot reconcile is unchanged (already O(instances) at 750 ms). Target: 50 instances × 9 tabs with no visible jank at the 100 ms tick, verified with a synthetic-store benchmark test in PR 3.

### 5.4 Keeping the TUI usable through the cutover

Every phase ships a complete, keyboard-operable TUI. The flow matrix verified manually (dev-install on the dev box) before merging each visible-change PR: create (local+remote), name-collision, enter/exit interactive mode (agent tab, shell tab, remote; `Ctrl-]` returns to nav), **Tab-completion forwards inside an interactive pane**, a full-screen program (vim or htop) driven inside a pane, open/hide/close panes across the N-pane ring, tab create/close/jump, kill, search, task create/edit/run-now from the rail, hooks edit, PR open/copy, daemon restart mid-session, cold start with daemon warm-up, external `tmux attach` to a pane's session (shrink behavior), 80×24 terminal. This matrix becomes a checklist in each PR description.

### 5.5 Terminal-size edge cases

Historically a bug farm (`ui/layout_height_test.go`, hard clamps everywhere). Mitigations: single sizing authority (`layout.Grid`) with property tests (regions exactly tile W×H, no negative dims, ladder monotonic); the `Pane` contract "View() is exactly Rect-sized" enforced by a shared test helper run against every pane; fallback banner below hard minimum (existing `ui/fallback.go`).

### 5.6 Test strategy

- **Unit (hermetic)**: `ui/layout`, `ui/tree`, zone registry — pure-function tests, no tmux. `ui/termpane` is largely hermetic too: the emulator is a byte-in/grid-out state machine, so render and key-encoding tests feed bytes and assert cells without a terminal or tmux. Existing `ui/` string-assertion style carries over; the per-pane Rect-contract helper is shared infrastructure. Race verification per dev-box constraints (`-p=1 -parallel 2`).
- **Model-level**: `app/` tests keep driving `home.Update` with synthetic messages; mouse tests inject `tea.MouseMsg` with coordinates derived from the zone registry (not hardcoded), so layout changes can't silently break them.
- **e2e**: teatest flows (`app/e2e_test.go`) updated per phase; `integration/` black-box daemon+tmux tests are layout-agnostic and must stay green untouched. A new teatest scenario per feature: pane open/hide across the ring, focus-ring cycle, enter/exit interactive mode, click-to-interact. Real-tmux termpane coverage (attach → drive vim/less → detach) follows the isolated-server pattern the spike used (`tmux -L`, private socket).
- **Mouse caveat**: real-terminal mouse reporting can't be exercised by teatest; hit-testing is covered hermetically (zone registry unit tests + injected MouseMsg), and click-to-interact plus in-pane forwarding are on the manual matrix.

### 5.7 Release blast radius

Users `af upgrade` into the new layout with no warning. The PR-4 release already shipped the one-time "the TUI changed" screen (seen-bitmask, `app/help.go:137-169`); R1–R3 each refresh it plus README + `docs/` in the same release. The interaction change in R3 is the sharpest edge — Enter now types into the pane instead of taking over the screen — so its release note and help screen lead with `Ctrl-]`. Keep every existing default keybinding working (additions only until #1026/#1027).

---

## 6. Open questions for Sachin

Resolved 2026-07-03:

1. ~~**Embedded interactive panes**~~ — **RESOLVED, reversed.** Sachin confirmed embedded interaction as the end state; the #1089 spike proved architecture A. §2.4 is now the decision of record. (Original recommendation — read-only + full-screen attach — superseded.)
2. ~~**Split orientation**~~ (was Q4) — **RESOLVED**: vertical splits only, generalized from a fixed A/B pair to the N-pane model (#1088). Stacked splits stay out of scope.
3. ~~**New-verb default keys**~~ (was Q6) — **RESOLVED in shape**: `Tab` = nav-mode focus ring, `s` open pane, `x` hide pane, `Enter` interactive, `Ctrl-]` back to nav (the only host-reserved key while interactive). Exact bindings still revisitable wholesale in #1026/#1027.

Still open:

4. **Tasks over RPC** — **Partially resolved (#1029 PR 3).** CLI task operations now route through daemon RPCs (`ListTasks`/`AddTask`/`UpdateTask`/`RemoveTask`/`TriggerTask`); the daemon is the sole task writer for CLI, completing that half of the #960 single-writer story. TUI direct-write+poke remains a tracked follow-up to keep this epic presentation-only.
5. **Automations rail-section scope** — current-repo tasks only (matches today's `LoadTasksForCurrentRepo`), or all repos with a repo column? RFC assumes current-repo.
6. **Hooks placement** — RFC demotes hooks from a persistent sidebar section to an overlay reachable from the automations section + hotkey. Any objection?

---

## Appendix A — file inventory as of the original RFC (pre-epic, production code)

Snapshot from before PR 1; kept as the baseline the fates refer to. Post-PR-5 additions not listed: `ui/layout/`, `ui/tree/`, `ui/pane/`, `ui/statusbar/`, the projection store. R3 adds `ui/termpane/` (§4.2). Fates citing the original numbering map to the revised plan as: PR 6 → R4 (mouse), PR 7 → folded into R3/R5 (help + docs refresh), PR 8 → R5 (final delete).

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
| `ui/tab_pane.go` | 468 | Adapted into `ui/pane` content view (PR 4); capture view superseded by `ui/termpane` (R3), deleted (R5) |
| `ui/task_pane.go` | 905 | Kept; re-hosted in automations strip (PR 4), moves behind an overlay off the rail (R1) |
| `ui/hooks_pane.go` | 221 | Kept; shown as overlay (PR 4) |
| `ui/overlay/*` | 849 | Kept verbatim (+ clickable zones, PR 6) |
| `keys/keys.go` | 202 | Extended (split/focus verbs) |
| `ui/consts.go`, `ui/theme.go`, `ui/fallback.go` | 78 | Kept |
