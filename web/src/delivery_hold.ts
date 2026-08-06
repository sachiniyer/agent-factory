// Holding an automated delivery while the user is mid-line (#3024).
//
// THE RULE IS THE DAEMON'S, AND THIS FILE DOES NOT RESTATE IT. What decides
// whether an automated delivery is held is `deferWhileAttached` in
// daemon/delivery.go, which consults the status-poll pause lease
// (`isPollPaused`). Everything that matters is already settled there:
//
//   - Only AUTOMATED deliveries defer. A manual send-prompt is an explicit user
//     action and still lands immediately (daemon/delivery.go).
//   - A held delivery is NOT dropped. It reports StatusDeferredAttached — a
//     status, deliberately not an "errored:" one — so a cron task records a
//     benign deferred run and re-fires on its next tick, and the watch path turns
//     it into errTargetBusy to re-queue and retry (#1586).
//   - The hold is BOUNDED SERVER-SIDE by statusPollLease, "never a
//     client-supplied duration", precisely so a crashed or misbehaving client
//     cannot silence an instance indefinitely (#1160, daemon/manager_status.go).
//
// So this module implements no hold policy of its own. It only answers the one
// question the daemon cannot see from outside the browser — "does this user have
// a partially typed line?" — and takes the same lease the attached TUI takes
// (app/home_attach.go runStatusPollPauseHeartbeat). Both surfaces then get their
// behaviour from one implementation, which is what keeps them from drifting: not
// a comment claiming parity, but the absence of a second copy to disagree with.
//
// Failure is graceful in the same way the TUI's is: every lease RPC is
// best-effort, and a lapsed lease means the daemon delivers as it does today —
// the pre-#3024 behaviour, not a broken one.

/** What the caller should do with the daemon's pause lease. */
export type HoldAction = "pause" | "renew" | "resume" | "none";

/** Bytes that END a line, releasing the hold: the user committed, so a queued
 *  delivery can land cleanly behind what they sent. CR is what xterm sends for
 *  Enter; LF is accepted too rather than assumed absent. */
const COMMIT_BYTES = new Set(["\r", "\n"]);

/** Bytes that ABANDON a line, releasing the hold for the opposite reason: there
 *  is no longer a partial line to protect. Ctrl-C is the interrupt (it discards
 *  the line), Ctrl-U kills it, Ctrl-D on an empty line ends input. Holding after
 *  one of these would delay a delivery to guard a line that no longer exists. */
const ABANDON_BYTES = new Set(["\x03", "\x15", "\x04"]);

/**
 * Tracks whether the user has an uncommitted line, and says when to take, renew
 * and release the daemon's pause lease.
 *
 * Pure and clock-injected so the whole state machine is testable without a
 * browser, a daemon, or a timer — the same shape as PendingInput (#2811), and for
 * the same reason: the interesting cases here are orderings, and a test that has
 * to drive a real terminal to reach them will not be written for all of them.
 */
export class MidLineHold {
  private uncommitted = false;
  private lastInputMs = 0;
  private lastRenewMs = 0;

  /**
   * @param renewIntervalMs how often to renew while the line stays uncommitted.
   *   Must be comfortably below the daemon's statusPollLease; the TUI uses 1s
   *   against a 3s lease "leaving two missed renews of slack" and this matches
   *   deliberately. Getting it wrong is not a correctness bug on either side — a
   *   lapsed lease just delivers, as today.
   * @param idleReleaseMs how long a line may sit untouched before the hold is
   *   released anyway. This is the answer to "do not hold indefinitely": a user
   *   who walks away mid-line stops renewing, and the queued delivery lands
   *   within this plus the daemon's own lease. Stated rather than implicit.
   */
  constructor(
    private readonly renewIntervalMs = 1_000,
    private readonly idleReleaseMs = 15_000,
  ) {}

  /** True while the user is considered to have a partially typed line. */
  get holding(): boolean {
    return this.uncommitted;
  }

  /**
   * Records one chunk of user input and returns what to do with the lease.
   *
   * Input arrives from xterm's onData as arbitrary chunks, not keystrokes: a
   * paste is ONE chunk that may contain a newline in the middle. Such a chunk
   * both commits the text before the newline and leaves a partial line after it,
   * so the answer is decided by the LAST commit/abandon byte in the chunk, not by
   * whether one appears anywhere in it. Reading it as "contains a newline →
   * committed" would release the hold while the user is mid-line again.
   */
  noteInput(data: string, nowMs: number): HoldAction {
    if (data === "") {
      return "none";
    }
    this.lastInputMs = nowMs;

    let ends: boolean | null = null;
    for (const ch of data) {
      if (COMMIT_BYTES.has(ch) || ABANDON_BYTES.has(ch)) {
        ends = true;
      } else {
        ends = false;
      }
    }

    if (ends === true) {
      // The chunk's last meaningful byte ended a line and nothing followed it.
      if (!this.uncommitted) {
        return "none";
      }
      this.uncommitted = false;
      return "resume";
    }

    // There is text after the last line ending (or no line ending at all): the
    // user is mid-line.
    if (!this.uncommitted) {
      this.uncommitted = true;
      this.lastRenewMs = nowMs;
      return "pause";
    }
    if (nowMs - this.lastRenewMs >= this.renewIntervalMs) {
      this.lastRenewMs = nowMs;
      return "renew";
    }
    return "none";
  }

  /**
   * Called on a timer while holding. Renews the lease, or releases it once the
   * line has sat untouched for idleReleaseMs.
   *
   * The renew exists because a user composing slowly can pause longer than the
   * daemon's lease between keystrokes; without it their line would stop being
   * protected mid-thought, which is the defect this fixes.
   */
  tick(nowMs: number): HoldAction {
    if (!this.uncommitted) {
      return "none";
    }
    if (nowMs - this.lastInputMs >= this.idleReleaseMs) {
      this.uncommitted = false;
      return "resume";
    }
    if (nowMs - this.lastRenewMs >= this.renewIntervalMs) {
      this.lastRenewMs = nowMs;
      return "renew";
    }
    return "none";
  }

  /**
   * Drops the hold without asking, for a teardown that makes the question moot —
   * the pane closing, the session changing, the socket going away for good.
   * Returns whether a lease was actually held, so a caller only sends the resume
   * RPC when there is something to resume.
   */
  release(): boolean {
    const held = this.uncommitted;
    this.uncommitted = false;
    return held;
  }
}
