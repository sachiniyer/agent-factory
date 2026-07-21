export interface TerminalGrid {
  rows: number;
  cols: number;
}

export interface TerminalHostSize {
  width: number;
  height: number;
}

/** Whether an active terminal should reclaim the grid that fits its visible host. */
export function shouldRefitVisibleTerminal(
  host: TerminalHostSize,
  current: TerminalGrid,
  proposed: TerminalGrid | undefined,
): boolean {
  if (host.width <= 0 || host.height <= 0 || !proposed || proposed.rows <= 0 || proposed.cols <= 0) {
    return false;
  }
  return proposed.rows !== current.rows || proposed.cols !== current.cols;
}

/** Restores the same reading distance from the bottom after a grid reflow. */
export function viewportLineFromBottom(baseY: number, distanceFromBottom: number): number {
  return Math.max(0, baseY - Math.max(0, distanceFromBottom));
}
