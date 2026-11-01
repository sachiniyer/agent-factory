// Keeps the ACTIVE tab visible in a horizontally scrolling tab bar (#6420).
//
// The bar preserves its scroll offset across a re-render so that a deliberate scroll
// (or a touch gesture in flight) is never snapped away. The price was that nothing
// ever revealed a tab that BECAME active off-screen: creating the 15th tab, or
// pressing 9 on a long strip, left the new active tab outside the viewport and the
// bar showing no highlighted tab at all.
//
// Pure — geometry in, a scroll delta out, no DOM — like tabreorder.ts, so the edge
// cases (taller than the viewport, already inside, hanging off either side) are unit
// tested without a browser. ui.ts supplies measured rects and applies the delta.

/** How far to scroll the bar (positive = toward the end) so the tab spanning
 *  [tabLeft, tabRight] lies inside the viewport [barLeft, barRight], all in the same
 *  coordinate space. 0 when it is already fully inside. A tab wider than the bar
 *  aligns its START, the part that carries the label. */
export function scrollDeltaToReveal(barLeft: number, barRight: number, tabLeft: number, tabRight: number): number {
  if (tabLeft < barLeft) return tabLeft - barLeft;
  if (tabRight - tabLeft > barRight - barLeft) return tabLeft - barLeft;
  if (tabRight > barRight) return tabRight - barRight;
  return 0;
}

/** Whether the render must reveal the active tab: only when the (session, active
 *  index) pair differs from the last one revealed. A re-render with the same active
 *  tab — a roster event, a rename, the user scrolling the strip away — must NOT undo
 *  the user's scroll. */
export function shouldRevealActive(previousKey: string | null, sessionId: string, activeIndex: number): boolean {
  return previousKey !== `${sessionId}:${activeIndex}`;
}

export function activeRevealKey(sessionId: string, activeIndex: number): string {
  return `${sessionId}:${activeIndex}`;
}
