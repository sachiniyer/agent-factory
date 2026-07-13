// The keyboard/focus state machine for the web client (#1693). It mirrors the
// TUI's explicit nav-vs-attach modes so j/k ALWAYS navigate the rail — instead of
// the pre-#1693 behavior, which inferred "who owns the keyboard" purely from DOM
// focus and so silently handed j/k to the agent the moment a terminal was clicked,
// with no keyboard way back to the rail.
//
// The model is two keyboard modes plus the modal overlay:
//   - "rail" (the default): j/k / arrows move the selection; Enter attaches the
//     selected session and hands the keyboard to its terminal.
//   - "terminal": keys flow to the agent; Escape is the ONE hatch back to rail
//     navigation (blur the terminal), matching the TUI's detach/back-to-nav.
//   - a modal, when open, owns the keyboard: only Escape (to cancel) is meaningful.
//
// This is kept pure — a (key, context) → action decision with no DOM and no I/O —
// so the exact transitions are unit-tested (nav.test.ts) independently of the
// event wiring in index.ts, exactly as the session-list reducer (sessions.ts) is.

/** Which pane owns the keyboard. The rail is the default; the terminal takes over
 *  on attach (Enter / click) and hands back on Escape. */
export type KeyboardFocus = "rail" | "terminal";

/** The state the key decision reads: the current mode, whether a modal is up, and
 *  the rail order + selection needed to compute the next selected row. */
export interface NavContext {
  focus: KeyboardFocus;
  modalOpen: boolean;
  /** The rail's session ids in DISPLAY order (the same order the DOM shows). */
  orderedIds: string[];
  selectedId: string | null;
}

/** What a keydown resolves to. Anything other than "none" is a handled key the
 *  caller should preventDefault (and stop from reaching the agent/form). */
export type NavAction =
  | { kind: "none" }
  | { kind: "closeModal" }
  | { kind: "select"; id: string }
  | { kind: "attach" }
  | { kind: "toRail" };

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
export function decideKey(key: string, ctx: NavContext): NavAction {
  // A modal owns the keyboard while open: Escape cancels it; everything else falls
  // through to the form (a normal keystroke into its input), never the rail.
  if (ctx.modalOpen) {
    return key === "Escape" ? { kind: "closeModal" } : { kind: "none" };
  }
  // The terminal owns the keyboard: keys go to the agent. Escape is the escape
  // hatch back to rail navigation (mirrors the TUI detach); nothing else is ours.
  if (ctx.focus === "terminal") {
    return key === "Escape" ? { kind: "toRail" } : { kind: "none" };
  }
  // Rail navigation (the default). Enter attaches the current selection to the
  // terminal and hands it the keyboard; j/k and the arrows move the selection.
  if (key === "Enter") {
    return ctx.selectedId ? { kind: "attach" } : { kind: "none" };
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
