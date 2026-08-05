// Recognising a touch attempt to DRAG a tab, so the tab bar can explain itself
// instead of doing nothing (the #2787 shape).
//
// Reordering a tab, and dragging one onto a pane to split, are both HTML5
// drag-and-drop (ui.ts attachTabDrag / split.ts wireDrop). Drag-and-drop does not
// start from a finger on the mobile browsers that matter — Chrome on Android never
// fires `dragstart` for touch input, and Safari's support is narrow and
// version-dependent. Every tab still renders `draggable=true` there, so a phone user
// presses a tab, drags, and gets NOTHING: no movement, no drop indicator, no message.
// Silence is the defect; the missing capability is a separate, deliberate product
// question (see the issue).
//
// Detecting the ATTEMPT is the whole trick, because a horizontal finger drag on the
// bar is genuinely ambiguous — it is also how the bar is scrolled when tabs overflow.
// What is NOT ambiguous is a press that stays put: nobody long-presses a tab except
// to pick it up. So the gesture this reads is the HOLD, and any real movement hands
// the finger straight back to the browser's scroll. Nothing here claims the pointer,
// suppresses scrolling, or calls preventDefault — a wrong guess costs a toast, never
// a broken scroll.

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
 * `explain` — a settled long press: the user is trying to drag a tab.
 * `waiting`  — too early to tell; keep watching.
 */
export type TabPressVerdict = "waiting" | "explain" | "abandon";

/** Slop is checked BEFORE the hold, so a long, slow scroll can never be mistaken for
 *  a hold just because it took a while. */
export function tabPressVerdict(sample: TabPressSample, limits: TabPressLimits): TabPressVerdict {
  if (sample.movedPx > limits.slopPx) {
    return "abandon";
  }
  if (sample.heldMs >= limits.holdMs) {
    return "explain";
  }
  return "waiting";
}

/** Deliberately longer than a tap and than the browser's own long-press callout, so
 *  the hint lands only on a press the user is still holding on purpose. */
export const TAB_PRESS_LIMITS: TabPressLimits = { holdMs: 500, slopPx: 10 };

/** Straight-line distance, the input to `movedPx`. */
export function pressDistance(fromX: number, fromY: number, toX: number, toY: number): number {
  return Math.hypot(toX - fromX, toY - fromY);
}
