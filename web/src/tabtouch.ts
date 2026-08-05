// Deciding when a finger on the tab bar has PICKED A TAB UP (#2899).
//
// Reordering a tab, and dragging one onto a pane to split, were both HTML5
// drag-and-drop only (ui.ts attachTabDrag / split.ts wireDrop), and drag-and-drop does
// not start from a finger on the mobile browsers that matter — Chrome on Android never
// fires `dragstart` for touch input, and Safari's support is narrow and
// version-dependent. Every tab still rendered `draggable=true` there, so a phone user
// pressed a tab, dragged, and got NOTHING: no movement, no drop indicator, no message.
//
// Telling the two gestures apart is the whole trick, because a horizontal finger drag
// on the bar is genuinely ambiguous — it is also how the bar is scrolled when tabs
// overflow. What is NOT ambiguous is a press that stays put: nobody long-presses a tab
// except to pick it up. So the pick-up is the HOLD, and any real movement before it
// hands the finger back to the browser's scroll for good. Getting this wrong in the
// permissive direction costs a bar that will not scroll, which is why the decision is
// here, pure and tested, rather than inline in a pointer handler.

/** What the press has done so far. `movedPx` is the distance from where the finger
 *  landed (not per-move delta), so a slow drift accumulates like a fast swipe. */
export interface TabPressSample {
  heldMs: number;
  movedPx: number;
}

/** The thresholds separating a hold from a tap and from a scroll. */
export interface TabPressLimits {
  /** How long the finger must stay down to read as "picking the tab up". */
  holdMs: number;
  /** How far it may wander first. Beyond this the finger is scrolling the bar. */
  slopPx: number;
}

/**
 * `abandon` — the finger moved: it is scrolling or swiping, so leave it alone.
 * `pickUp`  — a settled long press: the tab is now being dragged.
 * `waiting` — too early to tell; keep watching.
 */
export type TabPressVerdict = "waiting" | "pickUp" | "abandon";

/** Slop is checked BEFORE the hold, so a long, slow scroll can never be mistaken for
 *  a hold just because it took a while. */
export function tabPressVerdict(sample: TabPressSample, limits: TabPressLimits): TabPressVerdict {
  if (sample.movedPx > limits.slopPx) {
    return "abandon";
  }
  if (sample.heldMs >= limits.holdMs) {
    return "pickUp";
  }
  return "waiting";
}

/** Deliberately longer than a tap and than the browser's own long-press callout, so
 *  a tab is picked up only by a press the user is still holding on purpose. */
export const TAB_PRESS_LIMITS: TabPressLimits = { holdMs: 500, slopPx: 10 };

/** Straight-line distance, the input to `movedPx`. */
export function pressDistance(fromX: number, fromY: number, toX: number, toY: number): number {
  return Math.hypot(toX - fromX, toY - fromY);
}
