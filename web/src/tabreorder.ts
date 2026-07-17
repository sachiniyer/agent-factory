// The index math behind reordering tabs by dragging WITHIN the tab bar (#1813).
//
// Kept pure — geometry in, indices out, no DOM — for the same reason layout.ts and
// tabaddr.ts are: this is the part with the off-by-one and the pinned-tab invariant
// in it, so it is the part that has to be unit-testable without a browser. ui.ts
// supplies the measured tab centers and reads back an index.
//
// The vocabulary matters, because the two indices are NOT the same number:
//
//   - an INSERTION point is a gap between tabs, counted in the list as it stands
//     WITH the dragged tab still in it (0 = before the first tab, n = after the
//     last). It is what the drop indicator is drawn at.
//   - a TARGET index is where the dragged tab ends up AFTER it is lifted out — what
//     the daemon's reorder RPC takes. Removing the tab shifts every gap above it
//     down by one, which is the whole off-by-one.

/** How many leading tabs are PINNED and may never be moved or displaced: the agent
 *  tab at index 0.
 *
 *  This is not a UI preference. Go's Tabs[0] is a load-bearing invariant — archive
 *  and the agent's own conversation/tmux all index it — so the daemon rejects both
 *  moving the agent tab and moving anything in front of it. The bar enforces the
 *  same rule up front rather than drawing an indicator for a drop the daemon would
 *  refuse. */
export const PINNED_TABS = 1;

/**
 * The insertion point for a pointer at `x`, given each tab's horizontal CENTER in
 * the same coordinate space (viewport px). A pointer past a tab's midpoint means
 * the drop goes AFTER that tab — the standard "nearest gap" rule, and the reason
 * centers (not edges) are the input.
 *
 * Clamped to [PINNED_TABS, centers.length], so no drop can ever land in front of
 * the pinned agent tab: aiming left of it resolves to the gap just AFTER it. The
 * clamp lives here, with the math, so the indicator and the request can't disagree
 * about where a drop is legal.
 */
export function insertionIndexAt(centers: number[], x: number): number {
  let i = 0;
  while (i < centers.length && x >= centers[i]) {
    i++;
  }
  return Math.min(Math.max(i, PINNED_TABS), centers.length);
}

/**
 * The 0-based index the tab at `from` must be moved to for a drop at `insertion`,
 * or null when the move is refused or would be a no-op — the single gate the bar's
 * drop asks before issuing a request.
 *
 * null covers three cases, all of which the daemon also refuses or ignores:
 *   - `from` is a pinned tab (the agent tab): it may not be moved at all.
 *   - the drop lands in the tab's own gap, on either side of it — visually "where
 *     it already is". Both `insertion === from` and `insertion === from + 1` are
 *     no-ops, which is exactly what the -1 shift below collapses them into.
 *   - the resolved target is where the tab already sits.
 */
export function reorderTargetIndex(from: number, insertion: number): number | null {
  if (from < PINNED_TABS) {
    return null; // the agent tab is pinned
  }
  // Lifting the tab out shifts every gap ABOVE it down by one; gaps below are
  // unaffected. Without this, dragging a tab one slot to the right would move it
  // two.
  const target = insertion > from ? insertion - 1 : insertion;
  return target === from ? null : target;
}
