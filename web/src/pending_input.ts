// Type-ahead for the attach terminal's WebSocket (#2811).
//
// A real terminal never loses what you typed while it was starting up: the bytes
// sit in the tty's input queue and the program reads them when it gets there. The
// web terminal had no such queue. Its send path was:
//
//     if (ws && ws.readyState === WebSocket.OPEN) { ws.send(bytes); }
//
// …so every keystroke typed before the socket finished connecting was discarded,
// silently. That window is not exotic — it is open for the whole of every fresh
// tab (the pane mounts, then the socket connects) and again for every reconnect
// gap. What it produces is worse than a dropped keystroke: the SURVIVING tail of
// a half-sent command line reaches the shell on its own, so
// `for i in $(seq 1 80); do printf 'mouse-mode-%s\n' "$i"; done` arrives as
// `-mode-%s\n' "$i"; done` and leaves the shell sitting at a continuation prompt
// having run nothing. That exact corruption was captured in CI on #2796.
//
// This is the queue that was missing. It is deliberately DUMB — it holds opaque
// frames, in order, and hands them back on demand — so the socket lifecycle stays
// terminal.ts's business and this stays unit-testable without a DOM.

/** The default ceiling on buffered input, in bytes. Real type-ahead is a command
 *  line or a paste; 64 KiB is orders of magnitude beyond that, so the cap only
 *  ever engages on a socket that is never coming back (or a runaway sender) —
 *  where the alternative is growing without bound for the life of the page. */
export const DEFAULT_MAX_PENDING_INPUT_BYTES = 64 * 1024;

/**
 * An ordered, bounded hold for outbound input frames.
 *
 * When the cap is reached, the NEWEST frame is refused rather than evicting the
 * oldest. That direction is the whole point: a command line is only meaningful
 * from its first byte, and dropping the front is precisely the corruption this
 * exists to prevent — a truncated tail that still executes is far more dangerous
 * than input that never arrives. push() reports the refusal so the caller can
 * say so out loud instead of losing it quietly a second time.
 */
export class PendingInput {
  private frames: Uint8Array[] = [];
  private queued = 0;

  constructor(private readonly maxBytes: number = DEFAULT_MAX_PENDING_INPUT_BYTES) {}

  /** Holds one frame. Returns false when the cap refused it — nothing was stored,
   *  and everything already queued is untouched. */
  push(frame: Uint8Array): boolean {
    if (frame.length === 0) {
      return true; // nothing to hold; not a refusal
    }
    if (this.queued + frame.length > this.maxBytes) {
      return false;
    }
    this.frames.push(frame);
    this.queued += frame.length;
    return true;
  }

  /** Hands back every held frame IN THE ORDER IT WAS TYPED and empties the queue.
   *  Order is the contract: this is a byte stream, so a reordered flush would
   *  corrupt the command line exactly as a dropped prefix does. */
  drain(): Uint8Array[] {
    const out = this.frames;
    this.frames = [];
    this.queued = 0;
    return out;
  }

  /** Discards everything held. Used when the PTY this input was meant for is gone
   *  (exit or dispose), where delivering it later would type into a different
   *  program than the user was looking at. */
  clear(): void {
    this.frames = [];
    this.queued = 0;
  }

  /** Bytes currently held — the cap's accounting, exposed for tests and callers
   *  that want to report pressure. */
  get bytes(): number {
    return this.queued;
  }

  /** Frames currently held. */
  get length(): number {
    return this.frames.length;
  }
}
