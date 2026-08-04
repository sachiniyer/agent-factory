// Application mouse tracking deliberately gives a TUI the terminal's pointer.
// These helpers define af's one escape hatch back to browser-owned selection and
// scrollback without teaching the terminal component platform/delta arithmetic.

export type TerminalMouseOverride = "Shift" | "Option";

export interface MouseModifierEvent {
  altKey: boolean;
  shiftKey: boolean;
}

/** The MouseEvent flag xterm's SelectionService.shouldForceSelection reads to decide
 *  "select text" vs. "report this to the application". */
export type TerminalMouseOverrideFlag = "altKey" | "shiftKey";

export interface HistoryWheelEvent {
  deltaMode: number;
  deltaY: number;
}

export interface HistoryWheelPlan {
  lines: number;
  remainder: number;
}

/** xterm itself uses Option to force selection on macOS and Shift elsewhere. */
export function terminalMouseOverride(platform: string): TerminalMouseOverride {
  // Keep this in lockstep with xterm 5.5 Platform.isMac. In particular, xterm
  // classifies iPad separately and SelectionService therefore uses Shift there.
  return ["Macintosh", "MacIntel", "MacPPC", "Mac68K"].includes(platform) ? "Option" : "Shift";
}

export function terminalMouseOverrideFlag(override: TerminalMouseOverride): TerminalMouseOverrideFlag {
  return override === "Option" ? "altKey" : "shiftKey";
}

export function terminalMouseOverrideHeld(event: MouseModifierEvent, override: TerminalMouseOverride): boolean {
  return event[terminalMouseOverrideFlag(override)];
}

/**
 * Inverts the force-selection modifier xterm will read off this mousedown, so the
 * UNMODIFIED drag selects text and the held modifier is what hands the drag to a
 * mouse-tracking application (#2787).
 *
 * xterm resolves that fork from exactly one bit on the raw event — Option on macOS
 * (gated by macOptionClickForcesSelection), Shift elsewhere — and consults it from
 * BOTH of its mousedown listeners: the one that starts a selection and the one that
 * reports the button to the PTY. Flipping the bit before either runs therefore moves
 * the whole fork, keeping both behaviours; only which gesture needs a modifier
 * changes. There is no xterm option for this and no custom-mouse-handler hook (the
 * wheel is the only one), so the event itself is the seam.
 *
 * The flag is a readonly accessor on MouseEvent.prototype; an own data property
 * shadows it for every later listener without touching the prototype and without
 * re-dispatching a synthetic (untrusted, focus-losing) event.
 */
export function invertTerminalMouseOverride(event: MouseModifierEvent, override: TerminalMouseOverride): void {
  const flag = terminalMouseOverrideFlag(override);
  Object.defineProperty(event, flag, { value: !event[flag], configurable: true, enumerable: true });
}

/** Convert browser wheel units into whole terminal rows while retaining sub-row
 * trackpad motion for the next event. deltaMode follows WheelEvent: pixels=0,
 * lines=1, pages=2. */
export function historyWheelPlan(
  event: HistoryWheelEvent,
  rows: number,
  rowHeight: number,
  remainder: number,
): HistoryWheelPlan {
  if (event.deltaY === 0) {
    return { lines: 0, remainder };
  }

  let rowDelta = event.deltaY;
  if (event.deltaMode === 0) {
    rowDelta /= Math.max(1, rowHeight);
  } else if (event.deltaMode === 2) {
    rowDelta *= Math.max(1, rows);
  }

  const total = remainder + rowDelta;
  const wholeRows = total < 0 ? Math.ceil(total) : Math.floor(total);
  const lines = Object.is(wholeRows, -0) ? 0 : wholeRows;
  return { lines, remainder: total - lines };
}
