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
// So this module implements no hold policy of its own. It answers the one
// question the daemon cannot see from outside the browser — "does this user have
// a partially typed line?" — and takes the same lease the attached TUI takes.
// Both surfaces then get their behaviour from one implementation, which is what
// keeps them from drifting: not a comment claiming parity, but the absence of a
// second copy to disagree with.
//
// TAKING IS THE ONLY VERB. This never asks the daemon to RESUME, and that is a
// correctness requirement rather than a simplification. The lease is a single
// expiry per session (daemon/manager_status.go: `m.pausedPolls[key] = ...`) with
// no notion of who holds it, so a resume issued by this browser would clear an
// attached TUI's claim — or another window's — and re-open the very window #1638
// closed, until that holder's next heartbeat. Releasing by simply ceasing to
// renew costs at most one lease (3s) of extra hold and cannot revoke anyone
// else's protection. It also makes every request idempotent and order-free: a
// pause only ever pushes the expiry out, so two of them arriving out of order
// have the same effect as either one alone.
//
// Failure is graceful in the same way the TUI's is: every lease RPC is
// best-effort, and a lapsed lease means the daemon delivers as it does today —
// the pre-#3024 behaviour, not a broken one.

/** What the caller should do. "pause" takes or extends the lease; there is
 *  deliberately no release verb — see the note above. */
export type HoldAction = "pause" | "none";

/** ENTER, and only Enter. CR is what xterm sends for the Enter that submits.
 *
 *  LF is deliberately NOT here: Shift+Enter emits a bare LF as a composer newline
 *  that does not submit (#2374, terminal.ts), so treating LF as a commit would
 *  drop the hold in the middle of a multi-line draft — exactly when an automated
 *  delivery landing would submit that half-written draft. */
const COMMIT = "\r";

/** Ctrl-C, and only Ctrl-C. It discards the line in a shell and interrupts an
 *  agent, so after it there is no partial line left to protect.
 *
 *  Ctrl-U and Ctrl-D are NOT here, though an earlier version of this had them.
 *  They are context-dependent editing controls, not abandonments: in a
 *  readline-style shell Ctrl-U kills only from the cursor BACK to the start (text
 *  after the cursor survives), and Ctrl-D is EOF only on an EMPTY buffer — on a
 *  non-empty line it deletes the character under the cursor. Releasing on either
 *  would let a delivery land in the text that is still there, which is the defect
 *  this exists to prevent. Nothing outside the terminal can see the editor's
 *  buffer, so the honest answer is to keep holding and let the idle bound end it. */
const ABANDON = "\x03";

/** Anything the terminal EMITS rather than the user typing: focus reports
 *  (CSI I/O), mouse reports, replies to device queries, and the arrow/function
 *  keys. xterm's onData carries all of these alongside typed text, so a hold keyed
 *  on "any onData" would be created and renewed by merely focusing the pane,
 *  clicking, or scrolling — deferring cron and watch deliveries with no draft
 *  anywhere. Every one of them starts with ESC, and no plain typed character does.
 *
 *  Alt-chords also arrive as ESC+char and are ignored here. That is the safe
 *  direction: the cost is no hold for an alt-chord, not a spurious one. */
const ESC = "\x1b";

/** Bracketed paste. When the agent enables the mode, xterm wraps a pasted block as
 *  ESC[200~ … ESC[201~ and sends the whole thing through onData as one chunk. That
 *  begins with ESC, so the filter above would throw away a genuine pasted draft and
 *  take no lease — leaving an automated delivery free to append to it and submit.
 *
 *  The payload is inspected instead, and treated as text rather than scanned for a
 *  commit: inside a bracketed paste an embedded CR is LITERAL, which is the whole
 *  point of the mode — the shell inserts it rather than executing the line. So a
 *  pasted block that happens to end in a newline still leaves the user mid-draft. */
const PASTE_START = "\x1b[200~";
const PASTE_END = "\x1b[201~";

/** True when the chunk contains something a user would recognise as typing —
 *  a printable character. Used to START a hold, so that bare control keys on an
 *  empty prompt (a stray backspace, a tab completion on nothing) do not invent a
 *  draft to protect. Once a hold exists, any input renews it. */
function hasPrintable(data: string): boolean {
  for (const ch of data) {
    const code = ch.codePointAt(0) ?? 0;
    if (code >= 0x20 && code !== 0x7f) {
      return true;
    }
  }
  return false;
}

/**
 * Tracks whether the user has an uncommitted line, and says when to take or
 * extend the daemon's pause lease.
 *
 * Pure and clock-injected so the whole state machine is testable without a
 * browser, a daemon, or a timer — the same shape as PendingInput (#2811), and for
 * the same reason: the interesting cases here are orderings, and a test that has
 * to drive a real terminal to reach them will not be written for all of them.
 */
export class MidLineHold {
  private uncommitted = false;
  private lastInputMs = 0;
  private lastPauseMs = 0;

  /**
   * @param renewIntervalMs how often to re-send the pause while the line stays
   *   uncommitted. Must be comfortably below the daemon's statusPollLease; the TUI
   *   uses 1s against a 3s lease "leaving two missed renews of slack" and this
   *   matches deliberately. Getting it wrong is not a correctness bug on either
   *   side — a lapsed lease just delivers, as today.
   * @param idleReleaseMs how long a line may sit untouched before the hold ends
   *   anyway. This is the answer to "do not hold indefinitely": a user who walks
   *   away mid-line stops being renewed for, and the queued delivery lands within
   *   this plus one daemon lease. Stated rather than implicit.
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
   * Records one chunk of terminal input and returns whether to pause.
   *
   * Input arrives from xterm as arbitrary chunks, not keystrokes: a paste is ONE
   * chunk that may contain newlines in the middle. Such a chunk both commits the
   * text before the last newline and leaves a partial line after it, so the answer
   * is decided by what FOLLOWS the last commit byte, not by whether one appears
   * anywhere. Reading it as "contains Enter → committed" would drop the hold while
   * the user is mid-line again.
   */
  noteInput(data: string, nowMs: number): HoldAction {
    if (data === "") {
      return "none";
    }
    if (data.startsWith(PASTE_START)) {
      // A pasted draft. Held on its content alone — no commit scan, because
      // bracketed paste makes an embedded newline literal text rather than Enter.
      const payload = data.slice(PASTE_START.length).replace(PASTE_END, "");
      if (!hasPrintable(payload)) {
        return "none";
      }
      this.lastInputMs = nowMs;
      return this.beginOrRenew(nowMs);
    }
    if (data.startsWith(ESC)) {
      return "none";
    }
    this.lastInputMs = nowMs;

    const lastCommit = Math.max(data.lastIndexOf(COMMIT), data.lastIndexOf(ABANDON));
    if (lastCommit >= 0) {
      const tail = data.slice(lastCommit + 1);
      if (!hasPrintable(tail)) {
        // Ended on the commit: nothing of the user's is left unsent.
        this.uncommitted = false;
        return "none";
      }
      // Committed, then started a new line in the same chunk.
      return this.beginOrRenew(nowMs);
    }

    if (!this.uncommitted && !hasPrintable(data)) {
      return "none";
    }
    return this.beginOrRenew(nowMs);
  }

  /**
   * Called on a timer while holding. Re-pauses to extend the lease, or lets the
   * hold end once the line has sat untouched for idleReleaseMs.
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
      return "none";
    }
    if (nowMs - this.lastPauseMs >= this.renewIntervalMs) {
      this.lastPauseMs = nowMs;
      return "pause";
    }
    return "none";
  }

  /** Drops the hold for a teardown that makes the question moot — the pane
   *  closing, the session changing. Nothing is sent to the daemon: the lease is
   *  left to expire, because clearing it here would revoke a claim this browser
   *  may not be the only holder of. */
  release(): void {
    this.uncommitted = false;
  }

  private beginOrRenew(nowMs: number): HoldAction {
    if (!this.uncommitted) {
      this.uncommitted = true;
      this.lastPauseMs = nowMs;
      return "pause";
    }
    if (nowMs - this.lastPauseMs >= this.renewIntervalMs) {
      this.lastPauseMs = nowMs;
      return "pause";
    }
    return "none";
  }
}
