// The visible "Reconnecting…" state (#6430). The event stream and the focused
// pane's PTY socket each report when they are down and when their next attempt is
// due; this turns those facts into the words the header and rail show. Pure and
// clock-injected so the 2s grace and the countdown are unit-testable without a DOM.

/** A socket must stay down this long before the UI says so: a reconnect that
 *  finishes inside a blink would otherwise flash the chip on every network hiccup. */
export const RECONNECT_GRACE_MS = 2000;

export interface ConnectionInput {
  live: string;
  termStatus: string;
  /** epoch ms of the event stream's next attempt, when one is scheduled */
  liveRetryAt?: number | null;
  /** epoch ms of the focused PTY socket's next attempt, when one is scheduled */
  termRetryAt?: number | null;
}

export interface ConnectionNotice {
  /** Which socket(s) are down; "" when connected. */
  down: boolean;
  /** Whole seconds until the next attempt, or null while an attempt is in flight. */
  retryInS: number | null;
}

/** Is any watched socket in its reconnect loop? `connecting` is the first
 *  connect, not a drop, and `exited` is a finished PTY, so neither counts. */
export function isDown(input: ConnectionInput): boolean {
  return input.live === "reconnecting" || input.termStatus === "reconnecting";
}

/** The soonest scheduled attempt among the sockets that are down. */
export function nextRetryAt(input: ConnectionInput): number | null {
  const at: number[] = [];
  if (input.live === "reconnecting" && typeof input.liveRetryAt === "number") at.push(input.liveRetryAt);
  if (input.termStatus === "reconnecting" && typeof input.termRetryAt === "number") at.push(input.termRetryAt);
  return at.length ? Math.min(...at) : null;
}

/** What to show at `now`, given when the outage began (`downSince`, epoch ms). */
export function connectionNotice(input: ConnectionInput, downSince: number | null, now: number): ConnectionNotice | null {
  if (!isDown(input) || downSince === null || now - downSince < RECONNECT_GRACE_MS) return null;
  const at = nextRetryAt(input);
  const remaining = at === null ? null : Math.max(0, Math.ceil((at - now) / 1000));
  return { down: true, retryInS: remaining === 0 ? null : remaining };
}

/** The visible words: "Reconnecting…" plus the countdown when one is known. */
export function connectionWords(notice: ConnectionNotice): { label: string; retry: string; full: string } {
  const retry = notice.retryInS === null ? "" : `retry in ${notice.retryInS}s`;
  return {
    label: "Reconnecting…",
    retry,
    full: retry ? `Reconnecting… ${retry}` : "Reconnecting…",
  };
}
