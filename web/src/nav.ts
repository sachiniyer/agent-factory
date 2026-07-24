// The keyboard/focus state machine for the web client (#1693). It mirrors the
// TUI's explicit nav-vs-attach modes so j/k ALWAYS navigate the rail — instead of
// the pre-#1693 behavior, which inferred "who owns the keyboard" purely from DOM
// focus and so silently handed j/k to the agent the moment a terminal was clicked,
// with no keyboard way back to the rail.
//
// The model is two keyboard modes plus the modal overlay:
//   - "rail" (the default): j/k / arrows move the selection; Enter attaches the
//     selected session and hands the keyboard to its terminal.
//   - "terminal": keys flow to the agent — Escape included, since it is the agents'
//     interrupt key (#2517). ctrl+] is the ONE keyboard hatch back to rail navigation
//     (blur the terminal), matching the TUI's tea.KeyCtrlCloseBracket detach.
//   - a modal, when open, owns the keyboard: only Escape (to cancel) is meaningful.
//
// This is kept pure — a (key, context) → action decision with no DOM and no I/O —
// so the exact transitions are unit-tested (nav.test.ts) independently of the
// event wiring in index.ts, exactly as the session-list reducer (sessions.ts) is.

/** Which pane owns the keyboard. The rail is the default; the terminal takes over
 *  on attach (Enter / click) and hands back on ctrl+] (or any non-keyboard exit —
 *  clicking the rail, the mobile drawer). Escape is NOT a detach: it forwards to the
 *  agent as its interrupt (#2517). */
export type KeyboardFocus = "rail" | "terminal";

/** The app's top-level view: the live sessions rail+terminal, or the tasks
 *  (scheduled automations) pane. Both are SCOPED to the selected project (redesign
 *  PR2): the old top-level Projects view is gone — its project list folded into the
 *  top-right project switcher (the switcher is how you change project now). It is a
 *  HIGHER-level switch than keyboard focus — it selects which surface the body shows
 *  — so it composes with the nav-vs-terminal model rather than replacing it. */
export type View = "sessions" | "tasks" | "config";

/** The view cycle order for the [ / ] view-switch keys, left-to-right the same as
 *  the appbar's view tabs. */
export const VIEWS: readonly View[] = ["sessions", "tasks", "config"];

/** The view `delta` steps from `current`, wrapping around the cycle (so ] past the
 *  last view returns to the first, and [ before the first wraps to the last). */
export function cycleView(current: View, delta: 1 | -1): View {
  const i = VIEWS.indexOf(current);
  const n = VIEWS.length;
  return VIEWS[(i + delta + n) % n];
}

/** The state the key decision reads: the current mode, whether a modal is up, the
 *  rail order + selection needed to compute the next selected row, and the selected
 *  session's tab shape for the nav-mode tab keys (#1592 Phase 5 PR7). */
export interface NavContext {
  focus: KeyboardFocus;
  modalOpen: boolean;
  /** The current top-level view. The rail-mode session keys (Enter/attach, j/k,
   *  1-9/t/w) apply only in the "sessions" view — the tasks view is a mouse/button-
   *  driven list surface — while the [ / ] view-switch keys apply in EVERY view's
   *  rail mode. */
  view: View;
  /** The rail's session ids in DISPLAY order (the same order the DOM shows). */
  orderedIds: string[];
  selectedId: string | null;
  /** The selected session's tab count (≥1: at least the agent tab). Bounds the
   *  1-9 tab-switch keys so a digit past the last tab is a no-op. */
  tabCount: number;
  /** The selected session's active tab index (0 = agent). `w` refuses to close
   *  tab 0, and the 1-9 keys no-op on the already-active tab. */
  activeTab: number;
  /** Whether the selected session supports user tab management (false for remote
   *  sessions, whose tabs are fixed by hook config). Gates `t`/`w`. */
  tabManagement: boolean;
}

/** What a keydown resolves to. Anything other than "none" is a handled key the
 *  caller should preventDefault (and stop from reaching the agent/form). */
export type NavAction =
  | { kind: "none" }
  | { kind: "closeModal" }
  | { kind: "select"; id: string }
  | { kind: "attach" }
  | { kind: "toRail" }
  | { kind: "switchTab"; index: number }
  | { kind: "newTab" }
  | { kind: "closeTab" }
  | { kind: "switchView"; view: View }
  | { kind: "cyclePane"; delta: 1 | -1 }
  | { kind: "closePane" };

/** Modifier flags the keybinds read. Alt gates the split-pane chords; Ctrl gates
 *  the ctrl+] detach. altGraph is `getModifierState("AltGraph")`: it excludes an
 *  AltGr-produced "]" (an AltGr key on many EU layouts) from the detach chord, since
 *  Chromium on Linux signals AltGr this way rather than via ctrlKey. Shift and Meta
 *  are ignored so the chords never shadow a browser/OS shortcut. */
export interface KeyMods {
  alt?: boolean;
  ctrl?: boolean;
  altGraph?: boolean;
}

/** The next selected id after moving `delta` rows, clamped to the ends. From no
 *  selection, a downward move lands on the first row and an upward move on the last
 *  — matching the pre-#1693 rail nav (index.ts) this replaces. */
export function nextSelection(orderedIds: string[], selectedId: string | null, delta: 1 | -1): string | null {
  if (orderedIds.length === 0) {
    return null;
  }
  const cur = selectedId ? orderedIds.indexOf(selectedId) : -1;
  let next: number;
  if (cur === -1) {
    next = delta > 0 ? 0 : orderedIds.length - 1;
  } else {
    next = Math.min(Math.max(cur + delta, 0), orderedIds.length - 1);
  }
  return orderedIds[next] ?? null;
}

/** Resolves one keydown against the current mode. Pure: it never touches the DOM or
 *  the store — the caller (index.ts onKeydown) performs the effect for the returned
 *  action. Precedence is modal → terminal → rail, so an open modal and a focused
 *  terminal never leak keys to the rail. */
export function decideKey(key: string, ctx: NavContext, mods: KeyMods = {}): NavAction {
  // A modal owns the keyboard while open: Escape cancels it; everything else falls
  // through to the form (a normal keystroke into its input), never the rail.
  if (ctx.modalOpen) {
    return key === "Escape" ? { kind: "closeModal" } : { kind: "none" };
  }
  // Split-pane keys (feat: drag-and-drop split tabs), an Alt chord over the j/k/w
  // movement vocabulary so they read as "same motion, on PANES not the rail": Alt+j/k
  // cycle pane focus, Alt+w closes the focused pane. They fire in EITHER mode (a split
  // is only meaningful while attached, and in terminal mode a bare j/k/w must still
  // reach the agent — the Alt guard is what keeps them from leaking there) but only in
  // the sessions view with a selection. Handled before the terminal branch so terminal
  // mode doesn't swallow them.
  if (mods.alt === true && (key === "j" || key === "k" || key === "w")) {
    // Consume the chord even with no selection / outside the sessions view, so Alt+j
    // never falls through to a rail navigation (a pane action with no panes is just a
    // no-op, not a selection move).
    if (ctx.view !== "sessions" || !ctx.selectedId) {
      return { kind: "none" };
    }
    if (key === "j") {
      return { kind: "cyclePane", delta: 1 };
    }
    if (key === "k") {
      return { kind: "cyclePane", delta: -1 };
    }
    return { kind: "closePane" };
  }
  // ctrl+] is the terminal-detach chord (#2517), mirroring the TUI's
  // tea.KeyCtrlCloseBracket (app/interactive.go). It is guarded against Alt/AltGr: on
  // Windows AltGr arrives as ctrl+alt, and "]" is an AltGr key on many EU layouts
  // (German AltGr+9, Spanish, French, Italian, …), so a "]" typed via AltGr must NOT
  // be read as the chord — it has to reach the agent. Both altKey and the AltGraph
  // modifier are excluded (Chromium on Linux signals AltGr through the latter, not
  // ctrlKey), matching clipboard.ts's Ctrl-chord guard. Handled before BOTH the
  // terminal branch (so it detaches) and the rail-mode [ / ] view cycle below (so a
  // habitual repeat after detaching is inert, not a view switch); its toRail is a
  // no-op when already on the rail.
  if (key === "]" && mods.ctrl === true && mods.alt !== true && mods.altGraph !== true) {
    return ctx.focus === "terminal" ? { kind: "toRail" } : { kind: "none" };
  }
  // The terminal owns the keyboard: EVERY OTHER key goes to the agent, Escape
  // included. Escape is the agents' INTERRUPT key (#2070) — swallowing it here (as a
  // detach) meant a running agent could never be interrupted from the web at all
  // (#2517). Only the ctrl+] chord above is ours; everything else forwards. (An open
  // menu/modal still owns Escape: the modal branch above, and each menu's own capture
  // listener in ui.ts, run first.)
  if (ctx.focus === "terminal") {
    return { kind: "none" };
  }
  // View switching: [ / ] cycle the top-level view (sessions ⇄ tasks). Rail-mode
  // ONLY — a modal owns the keyboard (handled above) and a focused terminal forwards
  // them to the agent (also above), so switching views composes with the #1694 focus
  // model instead of fighting it. Shared across every view, unlike the session keys
  // below.
  if (key === "[") {
    return { kind: "switchView", view: cycleView(ctx.view, -1) };
  }
  if (key === "]") {
    return { kind: "switchView", view: cycleView(ctx.view, 1) };
  }
  // The remaining rail keys (Enter/attach, j/k selection, 1-9/t/w tabs) are the
  // SESSIONS view's model: they act on the live session list + the selected
  // session's terminal. The tasks view is a mouse/button-driven list surface with no
  // terminal, so those keys pass through there — only the view-switch keys above are
  // ours outside the sessions view.
  if (ctx.view !== "sessions") {
    return { kind: "none" };
  }
  // Rail navigation (the default). Enter attaches the current selection to the
  // terminal and hands it the keyboard; j/k and the arrows move the selection.
  if (key === "Enter") {
    return ctx.selectedId ? { kind: "attach" } : { kind: "none" };
  }
  // Tab management, mirroring the TUI's nav-mode tab keys (#930 t/w/1-9). These
  // only fire in rail mode: in terminal mode the branch above already forwards
  // them to the agent (a shell needs t/w/digits), exactly like the TUI forwards
  // everything while interactive. All require a selected session.
  if (ctx.selectedId) {
    // 1-9 switch to that tab of the selected session; a digit past the last tab
    // or onto the already-active tab is a no-op (passes through).
    if (key.length === 1 && key >= "1" && key <= "9") {
      const index = key.charCodeAt(0) - "1".charCodeAt(0);
      if (index < ctx.tabCount && index !== ctx.activeTab) {
        return { kind: "switchTab", index };
      }
      return { kind: "none" };
    }
    // t creates a new $SHELL tab (no command prompt, like Instance.AddShellTab);
    // w closes the active non-agent tab. Both need user tab management (remote
    // sessions' tabs are fixed), and w refuses the agent tab (index 0).
    if (key === "t") {
      return ctx.tabManagement ? { kind: "newTab" } : { kind: "none" };
    }
    if (key === "w") {
      return ctx.tabManagement && ctx.activeTab > 0 ? { kind: "closeTab" } : { kind: "none" };
    }
  }
  let delta: 1 | -1;
  if (key === "ArrowDown" || key === "j") {
    delta = 1;
  } else if (key === "ArrowUp" || key === "k") {
    delta = -1;
  } else {
    return { kind: "none" };
  }
  const next = nextSelection(ctx.orderedIds, ctx.selectedId, delta);
  return next ? { kind: "select", id: next } : { kind: "none" };
}
