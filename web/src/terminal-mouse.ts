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

/**
 * How far a finger must travel before a gesture counts as a scroll rather than a
 * tap, in CSS pixels.
 *
 * This threshold is not comfort, it is a capability. Scrolling the terminal by touch
 * means cancelling the touchmove, and a cancelled touch fires no compatibility mouse
 * events — so treating the first pixel of jitter as a scroll silently costs a
 * mouse-aware application the click that an unsteady tap should have delivered.
 *
 * 8px is Chromium's own touch slop, which is also the distance at which the browser
 * would start scrolling on its own. Claiming at the same point is what keeps the two
 * from overlapping: below it neither the browser nor af moves anything, and above it
 * af has already taken the gesture. It stays under one terminal row, so a real scroll
 * still begins within half a line of the finger.
 */
export const TOUCH_SCROLL_SLOP_PX = 8;

/** Whether a one-finger gesture has travelled far enough to be a scroll. */
export function touchScrollClaimsGesture(originY: number, y: number): boolean {
  return Math.abs(y - originY) >= TOUCH_SCROLL_SLOP_PX;
}

/**
 * Convert one step of a one-finger drag into whole terminal rows (#2682).
 *
 * The sign convention is xterm's own: a finger moving UP the screen (clientY
 * decreasing) scrolls toward the NEWEST output, which is what a positive wheel
 * deltaY does — so the drag is expressed as that wheel and shares its arithmetic,
 * including the sub-row remainder that keeps a slow drag moving at all.
 */
export function touchHistoryScrollPlan(
  lastY: number,
  y: number,
  rows: number,
  rowHeight: number,
  remainder: number,
): HistoryWheelPlan {
  return historyWheelPlan({ deltaMode: 0, deltaY: lastY - y }, rows, rowHeight, remainder);
}

/**
 * How long a finger must rest before its touch is a long press rather than a tap.
 *
 * 500ms is the platform convention on both Android and iOS, so it is what a thumb
 * already expects. It sits far enough above a tap that neither gesture has to be
 * described to the user, which matters because this is the only copy gesture a touch
 * device has (#2849) and nothing on screen advertises it.
 */
export const TOUCH_LONG_PRESS_MS = 500;

/**
 * Whether a touch is still close enough to where it started to count as a press.
 *
 * Deliberately the SAME threshold that turns a drag into a scroll: one number decides
 * both, so a gesture can never be a press and a scroll at once, and there is no band
 * between them where a finger is doing neither.
 */
export function touchPressStillHeld(originX: number, originY: number, x: number, y: number): boolean {
  return Math.abs(x - originX) < TOUCH_SCROLL_SLOP_PX && Math.abs(y - originY) < TOUCH_SCROLL_SLOP_PX;
}

export interface TerminalCellPoint {
  col: number;
  row: number;
}

/**
 * Resolve a viewport point to the terminal cell under it, given the rendered rows'
 * box. Returns null for a point outside the grid.
 *
 * The box is divided by the grid rather than by a measured character width: the rows
 * element is sized to exactly cols × rows cells, so this needs no font metrics and
 * cannot drift from them.
 */
export function terminalCellAtPoint(
  x: number,
  y: number,
  rect: { left: number; top: number; width: number; height: number },
  cols: number,
  rows: number,
): TerminalCellPoint | null {
  if (rect.width <= 0 || rect.height <= 0 || cols <= 0 || rows <= 0) {
    return null;
  }
  const col = Math.floor(((x - rect.left) / rect.width) * cols);
  const row = Math.floor(((y - rect.top) / rect.height) * rows);
  if (col < 0 || col >= cols || row < 0 || row >= rows) {
    return null;
  }
  return { col, row };
}

export interface TerminalWordRange {
  start: number;
  length: number;
}

/**
 * The run of non-blank cells containing `col`, or null when that cell is blank.
 *
 * `cells` carries ONE ENTRY PER COLUMN, so an index is a column. That is the whole
 * reason it is not a string: a wide character occupies two columns but one code
 * point, so a string index silently drifts from the column it is meant to name — and
 * the drift only appears with CJK output, i.e. never in the tests and always in the
 * field. A wide char's trailing column arrives as "" and belongs to the word it
 * continues.
 *
 * A word is a run of non-whitespace, not xterm's double-click word (which stops at
 * punctuation). What a terminal is worth copying from is exactly the whitespace-
 * delimited token — a URL, a path, a branch name, a container id — and splitting
 * those on `/` or `-` would hand back a fragment nobody wanted.
 */
export function wordRangeAtColumn(cells: readonly string[], col: number): TerminalWordRange | null {
  const partOfWord = (index: number): boolean => {
    const cell = cells[index];
    return cell !== undefined && (cell === "" || cell.trim() !== "");
  };
  if (!partOfWord(col)) {
    return null;
  }
  let start = col;
  while (start > 0 && partOfWord(start - 1)) {
    start -= 1;
  }
  let end = col;
  while (end + 1 < cells.length && partOfWord(end + 1)) {
    end += 1;
  }
  return { start, length: end - start + 1 };
}

/** How many columns the line's own content occupies, ignoring its blank tail — the
 *  fallback selection for a press that lands past the end of a line. */
export function lineContentColumns(cells: readonly string[]): number {
  let last = -1;
  for (let col = cells.length - 1; col >= 0; col -= 1) {
    const cell = cells[col];
    if (cell !== undefined && cell !== "" && cell.trim() !== "") {
      last = col;
      break;
    }
  }
  if (last < 0) {
    return 0;
  }
  // A wide character is the LAST thing on a line more often than one would guess (a
  // CJK log line), and its trailing column is part of it: stopping at the glyph would
  // hand back a length that cuts the character in half.
  let end = last;
  while (end + 1 < cells.length && cells[end + 1] === "") {
    end += 1;
  }
  return end + 1;
}
