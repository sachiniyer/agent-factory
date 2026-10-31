// The push chip of the Docs tab (proposal mode, step 3): it turns the daemon's
// push status into what the header shows, and counts the countdown down itself.
// The contract is docs/dev/proposals/proposal-mode/api.md, Push.

import type { PushMode, PushState, PushStatus } from "./proposal_api.js";

/** How long "Sent to agent" stays after a push. */
export const SENT_VISIBLE_MS = 4_000;

export interface PushView {
  mode: PushMode;
  state: PushState;
  /** The chip text; "" shows no text. */
  text: string;
  /** The chip button: Hold in a countdown, Send now when held or in manual mode. */
  action: "hold" | "send" | null;
  actionEnabled: boolean;
  /** A push route call is in flight: the switch and the button wait for it. */
  busy: boolean;
  unsent: number;
}

/** The view of one status. `remainingMs` is what is left of the countdown now;
 *  `sentVisible` is false once "Sent to agent" has shown long enough. */
export function pushView(
  status: PushStatus,
  remainingMs: number,
  sentVisible: boolean,
  busy = false,
  error = "",
): PushView {
  let text = "";
  let action: PushView["action"] = null;
  let actionEnabled = !busy;
  switch (status.state) {
    case "countdown":
      text = remainingMs > 0 ? `Sending to agent in ${Math.ceil(remainingMs / 1000)}s` : "Sending to agent…";
      action = "hold";
      break;
    case "waiting_agent":
      text = "Waits for agent";
      break;
    case "held":
      text = "Held";
      action = "send";
      break;
    case "sent":
      text = sentVisible ? "Sent to agent" : "";
      break;
    case "idle":
      break;
  }
  if (status.mode === "manual") {
    if (status.state !== "waiting_agent" && status.state !== "sent") text = "";
    action = "send";
  }
  if (action === "send") actionEnabled = !busy && status.unsent > 0;
  if (error !== "") text = error;
  return { mode: status.mode, state: status.state, text, action, actionEnabled, busy, unsent: status.unsent };
}

export interface PushChipDeps {
  now(): number;
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(handle: unknown): void;
  /** Gets the view on each change; null when the session has no push status. */
  render(view: PushView | null): void;
}

/** Holds the latest push status and re-renders as the countdown runs down. */
export class PushChip {
  private status: PushStatus | null = null;
  private receivedAt = 0;
  private sentVisible = true;
  private busy = false;
  private error = "";
  private timer: unknown = null;

  constructor(private readonly deps: PushChipDeps) {}

  get current(): PushStatus | null {
    return this.status;
  }

  /** Counts statuses applied. A reply to a request that went out at version v is
   *  older than any status applied since: the daemon's events and a later fetch
   *  already say more than it does. */
  version = 0;

  apply(status: PushStatus | null): void {
    this.version++;
    this.status = status;
    this.receivedAt = this.deps.now();
    this.sentVisible = true;
    this.error = "";
    this.update();
  }

  /** Applies the reply to a request sent at `sentAt` (a `version` read before it went
   *  out) unless a newer status arrived meanwhile. Returns whether it was applied. */
  applyReply(status: PushStatus | null, sentAt: number): boolean {
    if (sentAt !== this.version) return false;
    this.apply(status);
    return true;
  }

  setBusy(busy: boolean): void {
    this.busy = busy;
    this.update();
  }

  /** Shows an error in place of the chip text until the next status. */
  setError(message: string): void {
    this.error = message;
    this.update();
  }

  dispose(): void {
    this.stopTimer();
  }

  private stopTimer(): void {
    if (this.timer !== null) {
      this.deps.clearTimer(this.timer);
      this.timer = null;
    }
  }

  private update(): void {
    this.stopTimer();
    const status = this.status;
    if (!status) {
      this.deps.render(null);
      return;
    }
    const remaining = (status.dueInMs ?? 0) - (this.deps.now() - this.receivedAt);
    this.deps.render(pushView(status, remaining, this.sentVisible, this.busy, this.error));
    if (status.state === "countdown" && remaining > 0) {
      // Wake when the shown whole second changes.
      this.timer = this.deps.setTimer(() => {
        this.timer = null;
        this.update();
      }, remaining % 1000 || 1000);
    } else if (status.state === "sent" && this.sentVisible) {
      const left = SENT_VISIBLE_MS - (this.deps.now() - this.receivedAt);
      this.timer = this.deps.setTimer(() => {
        this.timer = null;
        this.sentVisible = false;
        this.update();
      }, Math.max(0, left));
    }
  }
}
