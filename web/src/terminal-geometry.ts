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

/** A real scroll after fitting wins over the deferred anchor restore. */
export function shouldRestoreViewport(scheduledViewportY: number, currentViewportY: number): boolean {
  return scheduledViewportY === currentViewportY;
}
