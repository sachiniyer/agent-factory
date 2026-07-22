export interface TerminalGrid {
  rows: number;
  cols: number;
}

export interface TerminalHostSize {
  width: number;
  height: number;
}

export interface TerminalBufferPosition {
  baseY: number;
  cursorY: number;
  viewportY: number;
}

export interface ViewportAnchorTarget {
  atBottom: boolean;
  markerLine: number | null;
  fallbackLine: number;
}

export interface ViewportRestoreIntent {
  scheduledUserScroll: number;
  currentUserScroll: number;
}

export type TerminalUserScrollSource = "wheel" | "touch" | "scrollbar";

export interface TerminalUserScrollPlan {
  cancelScheduledVisibleFit: boolean;
}

/** Whether FitAddon has a real grid for a non-hidden host. This also gates the
 * first socket connect, so a newly opened client never advertises xterm's 80x24
 * constructor default to the shared PTY before layout has produced its real grid. */
export function hasVisibleTerminalGeometry(
  host: TerminalHostSize,
  proposed: TerminalGrid | undefined,
): proposed is TerminalGrid {
  return host.width > 0 && host.height > 0 && !!proposed && proposed.rows > 0 && proposed.cols > 0;
}

/** Whether an active terminal should reclaim the grid that fits its visible host. */
export function shouldRefitVisibleTerminal(
  host: TerminalHostSize,
  current: TerminalGrid,
  proposed: TerminalGrid | undefined,
): boolean {
  if (!hasVisibleTerminalGeometry(host, proposed)) {
    return false;
  }
  return proposed.rows !== current.rows || proposed.cols !== current.cols;
}

/** Offset from xterm's absolute cursor line to the visible viewport's first line.
 * An xterm marker registered here follows that content as output adds/removes lines. */
export function viewportMarkerOffset(position: TerminalBufferPosition): number {
  return position.viewportY - (position.baseY + position.cursorY);
}

/** Resolves a saved viewport anchor. xterm leaves a disposed marker object alive
 * with line=-1, so only a non-negative marker can outrank the absolute fallback. */
export function viewportAnchorLine(anchor: ViewportAnchorTarget, baseY: number): number {
  if (anchor.atBottom) {
    return baseY;
  }
  return anchor.markerLine !== null && anchor.markerLine >= 0 ? anchor.markerLine : anchor.fallbackLine;
}

/** A real user scroll after fitting wins over the deferred anchor restore. Output
 * may move viewportY on its own and must not pose as user intent. */
export function shouldRestoreViewport(intent: ViewportRestoreIntent): boolean {
  return intent.scheduledUserScroll === intent.currentUserScroll;
}

/** Plans the activation work for an explicit user scroll while a peer anchor is
 * pending. Wheel, touch, and a scrollbar gesture are equally authoritative. A
 * queued focus/visibility fit must be cancelled before the synchronous input fit,
 * or it can reschedule restoration from the newly incremented revision. */
export function terminalUserScrollPlan(
  source: TerminalUserScrollSource,
  hasScheduledVisibleFit: boolean,
): TerminalUserScrollPlan {
  switch (source) {
    case "wheel":
    case "touch":
    case "scrollbar":
      return { cancelScheduledVisibleFit: hasScheduledVisibleFit };
  }
}
