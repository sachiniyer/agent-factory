// Application mouse tracking deliberately gives a TUI the terminal's pointer.
// These helpers define af's one escape hatch back to browser-owned selection and
// scrollback without teaching the terminal component platform/delta arithmetic.

export type TerminalMouseOverride = "Shift" | "Option";

export interface MouseModifierEvent {
  altKey: boolean;
  shiftKey: boolean;
}

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

export function terminalMouseOverrideHeld(event: MouseModifierEvent, override: TerminalMouseOverride): boolean {
  return override === "Option" ? event.altKey : event.shiftKey;
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
