type RestoreRow = {
  id?: string;
  restoreEligible: boolean;
  restoreSettled?: boolean;
  operationLockHeld?: boolean;
};
export type RestoreEvidence =
  | {
    kind: "snapshot";
    generation: number;
    operationLockTimeoutMs?: number;
    operationClockMs?: number;
  }
  | { kind: "updated" | "restored"; id: string };

// Network delivery and handler scheduling get a small margin after the daemon's
// own admission deadline before an unchanged row proves no restore is queued.
export const RESTORE_ADMISSION_MARGIN_MS = 1_000;
export const RESTORE_RECONCILE_RETRY_MIN_MS = 2_000;
const RESTORE_RECONCILE_RETRY_MAX_MS = 10_000;
// Pre-projection daemons bounded manual restore admission at 30 seconds (#2700).
// This value only delays their first reconciliation probe. Without a daemon
// clock and explicit lock ownership, browser elapsed time cannot prove release.
const LEGACY_OPERATION_LOCK_TIMEOUT_MS = 30_000;

type RestoreTimer = ReturnType<typeof globalThis.setTimeout>;
type RestoreTicket = {
  settled: boolean;
  uncertain: boolean;
  succeededAt: number | null;
  uncertainAt: number;
  uncertainSince: number;
  admissionClockStartedAt: number | null;
  timer: RestoreTimer | null;
  retryDelayMs: number;
  reconcileGeneration: number;
  restoreEligible: RestoreRow["restoreEligible"];
};

/** Request fences outlive dialogs and remain until a successful restore is visible. */
export class PendingRestores {
  private readonly tickets = new Map<string, RestoreTicket>();
  private snapshotGeneration = 0;
  private rows: ReadonlyArray<RestoreRow> | null = null;
  private operationLockTimeoutMs: number | null = null;
  private operationClockMs: number | null = null;

  constructor(
    private readonly changed: (ids: ReadonlySet<string>) => void,
    private readonly retainOnError: (error: unknown) => boolean = () => false,
    private readonly committedOnError: (error: unknown) => boolean = () => false,
    // Local time only schedules probes. It never proves a daemon-side deadline;
    // current-daemon expiry uses the daemon's own monotonic readings.
    private readonly now: () => number = () => globalThis.performance.now(),
    private readonly requestReconcile: () => void | Promise<void> = () => {},
    private readonly schedule: (callback: () => void, delayMs: number) => RestoreTimer =
      (callback, delayMs) => {
        const timer = globalThis.setTimeout(callback, delayMs);
        // Browser timers are numbers. Let Node's unit runner exit when a test
        // intentionally leaves a fenced ticket behind for a later observation.
        if (typeof timer === "object") timer.unref();
        return timer;
      },
    private readonly cancel: (timer: RestoreTimer) => void = timer => globalThis.clearTimeout(timer),
  ) {}

  has(id: string): boolean {
    return this.tickets.has(id);
  }

  run<T>(id: string, request: () => Promise<T>, restoreEligible: RestoreRow["restoreEligible"]): Promise<T> | null {
    if (this.has(id)) return null;
    const ticket = {
      settled: false,
      uncertain: false,
      succeededAt: null as number | null,
      uncertainAt: this.snapshotGeneration,
      uncertainSince: 0,
      admissionClockStartedAt: null as number | null,
      timer: null,
      retryDelayMs: RESTORE_RECONCILE_RETRY_MIN_MS,
      reconcileGeneration: 0,
      restoreEligible,
    };
    this.tickets.set(id, ticket);
    this.changed(new Set(this.tickets.keys()));
    return (async () => {
      try {
        const result = await request();
        if (this.tickets.get(id) === ticket) {
          ticket.settled = true;
          // Events can advance the row before the HTTP response arrives.
          if (this.rows) this.observe(this.rows);
          if (this.tickets.get(id) === ticket) this.reconcile(id, ticket);
        }
        return result;
      } catch (error) {
        // A response from an old connection cannot release a newer request.
        if (this.tickets.get(id) === ticket) {
          if (this.retainOnError(error)) {
            ticket.uncertain = true;
            ticket.uncertainAt = this.snapshotGeneration;
            ticket.uncertainSince = this.now();
            ticket.admissionClockStartedAt = null;
            ticket.retryDelayMs = RESTORE_RECONCILE_RETRY_MIN_MS;
            this.armUncertainTimer(id, ticket);
            if (this.committedOnError(error)) {
              ticket.settled = true;
              ticket.succeededAt = this.snapshotGeneration;
            }
            if (this.rows) this.observe(this.rows);
          } else {
            this.release(id, ticket);
            this.changed(new Set(this.tickets.keys()));
          }
        }
        throw error;
      }
    })();
  }

  /** Stamp issuance, not arrival: a pre-success Snapshot cannot prove completion. */
  beginSnapshot(): number {
    return ++this.snapshotGeneration;
  }

  observe(rows: ReadonlyArray<RestoreRow>, evidence?: RestoreEvidence): void {
    this.rows = rows;
    if (evidence?.kind === "snapshot") {
      const operationLockTimeoutMs = typeof evidence.operationLockTimeoutMs === "number" &&
        evidence.operationLockTimeoutMs >= 0 ? evidence.operationLockTimeoutMs : LEGACY_OPERATION_LOCK_TIMEOUT_MS;
      const operationClockMs = typeof evidence.operationClockMs === "number" &&
        Number.isFinite(evidence.operationClockMs) && evidence.operationClockMs >= 0 ? evidence.operationClockMs : null;
      const clockSupportChanged = (this.operationClockMs === null) !== (operationClockMs === null);
      const clockReset = this.operationClockMs !== null && operationClockMs !== null &&
        operationClockMs < this.operationClockMs;
      if (this.operationLockTimeoutMs !== operationLockTimeoutMs || clockSupportChanged || clockReset) {
        for (const ticket of this.tickets.values()) {
          this.cancelTimer(ticket);
          ticket.admissionClockStartedAt = null;
        }
      }
      this.operationLockTimeoutMs = operationLockTimeoutMs;
      this.operationClockMs = operationClockMs;
    }
    const eligibility = new Map(rows.map(row => [row.id, row.restoreEligible]));
    let changed = false;
    for (const [id, ticket] of this.tickets) {
      const row = rows.find(candidate => candidate.id === id);
      // Absence is unknown, not an inferred release. Only current daemons can
      // provide the explicit negative fact needed by the admission proof.
      const operationLockKnownFree = row?.operationLockHeld === false;
      const authoritative = evidence?.kind === "updated" || evidence?.kind === "restored" || evidence?.kind === "snapshot";
      // A delayed recover-fence update may arrive after HTTP success. Only a
      // Snapshot issued afterward proves completion. Identity-only restored events
      // carry no attempt id and may belong to a previous restore cycle.
      const observedAfterSuccess = ticket.succeededAt !== null && evidence?.kind === "snapshot" &&
        evidence.generation > ticket.succeededAt;
      const causalUncertainSnapshot = evidence?.kind === "snapshot" &&
        evidence.generation > ticket.uncertainAt;
      let daemonAdmissionExpired = false;
      if (causalUncertainSnapshot && this.operationLockTimeoutMs !== null && this.operationClockMs !== null) {
        if (ticket.admissionClockStartedAt === null || this.operationClockMs < ticket.admissionClockStartedAt) {
          // Start from the first post-error daemon reading. A pre-request
          // reading could expire before the queued restore's own lock wait.
          ticket.admissionClockStartedAt = this.operationClockMs;
        } else {
          daemonAdmissionExpired = this.operationClockMs >
            ticket.admissionClockStartedAt + this.operationLockTimeoutMs + RESTORE_ADMISSION_MARGIN_MS;
        }
      }
      // This is the sole off-ramp for an unchanged uncertain row: both facts
      // must be explicitly reported by the same current-daemon Snapshot.
      const daemonReleaseProven = daemonAdmissionExpired && operationLockKnownFree;
      let uncertainCompleted = false;
      if (ticket.uncertain && authoritative && causalUncertainSnapshot) {
        if (!eligibility.has(id)) {
          uncertainCompleted = true;
        } else if (row?.restoreSettled) {
          // The projection has no attempt id. A predecessor can settle after B
          // is queued but before B acquires the daemon operation lock, so only a
          // Snapshot beyond B's admission bound can release its fence.
          uncertainCompleted = daemonReleaseProven;
        } else if (row?.restoreEligible) {
          // A reconnect can hold these captured rows behind slower task/project
          // loads. Processing time cannot turn a pre-deadline Snapshot into proof.
          // The elapsed bound proves a waiter cannot still be queued, but an
          // admitted restore may own the lock before OpRestoring is projected.
          uncertainCompleted = daemonReleaseProven;
        } else {
          // LifecycleActionNone covers every operation fence and several unsettled
          // states. It carries no attempt identity, so it cannot make a later
          // restorable projection conclusive for this ticket.
          uncertainCompleted = false;
        }
      }
      if ((ticket.settled && (observedAfterSuccess || !eligibility.has(id) || (ticket.restoreEligible && !eligibility.get(id)))) || uncertainCompleted) {
        this.release(id, ticket);
        changed = true;
      }
    }
    if (evidence?.kind === "snapshot") {
      for (const [id, ticket] of this.tickets) this.armTimerForState(id, ticket);
    }
    if (changed) this.changed(new Set(this.tickets.keys()));
  }

  reset(): void {
    // Logical disconnect does not stop the HTTP request or daemon mutation.
    for (const [id, ticket] of this.tickets) {
      this.cancelTimer(ticket);
      ticket.reconcileGeneration++;
      ticket.retryDelayMs = RESTORE_RECONCILE_RETRY_MIN_MS;
      if (ticket.settled) this.tickets.delete(id);
    }
    this.rows = null;
    // Keep issuance stamps monotonic for requests that survive reconnect.
    this.changed(new Set(this.tickets.keys()));
  }

  private armTimerForState(id: string, ticket: RestoreTicket): void {
    if (ticket.uncertain) this.armUncertainTimer(id, ticket);
    else if (ticket.settled) this.armRetryTimer(id, ticket);
  }

  private armUncertainTimer(id: string, ticket: RestoreTicket): void {
    if (!ticket.uncertain || ticket.timer !== null || this.operationLockTimeoutMs === null) return;
    let remaining: number;
    if (this.operationClockMs !== null && ticket.admissionClockStartedAt === null) {
      remaining = 0; // Establish a causal daemon-clock baseline immediately.
    } else {
      remaining = this.operationClockMs === null
        ? ticket.uncertainSince + this.operationLockTimeoutMs + RESTORE_ADMISSION_MARGIN_MS - this.now()
        : ticket.admissionClockStartedAt! + this.operationLockTimeoutMs + RESTORE_ADMISSION_MARGIN_MS -
          this.operationClockMs;
      if (remaining <= 0) {
        this.armRetryTimer(id, ticket);
        return;
      }
    }
    ticket.timer = this.schedule(() => {
      ticket.timer = null;
      if (this.tickets.get(id) !== ticket || !ticket.uncertain) return;
      this.reconcile(id, ticket);
    }, Math.max(0, remaining));
  }

  private armRetryTimer(id: string, ticket: RestoreTicket): void {
    if (!this.needsReconcile(ticket) || ticket.timer !== null) return;
    const delay = ticket.retryDelayMs;
    ticket.retryDelayMs = Math.min(ticket.retryDelayMs * 2, RESTORE_RECONCILE_RETRY_MAX_MS);
    ticket.timer = this.schedule(() => {
      ticket.timer = null;
      if (this.tickets.get(id) !== ticket || !this.needsReconcile(ticket)) return;
      this.reconcile(id, ticket);
    }, delay);
  }

  private reconcile(id: string, ticket: RestoreTicket): void {
    const generation = ++ticket.reconcileGeneration;
    const finish = () => {
      if (this.tickets.get(id) !== ticket || ticket.reconcileGeneration !== generation) return;
      if (!this.needsReconcile(ticket)) return;
      this.armRetryTimer(id, ticket);
    };
    try {
      const result = this.requestReconcile();
      if (result && typeof result.then === "function") {
        // requestResync absorbs transport errors; either outcome completes this
        // attempt, and a still-fenced ticket then schedules the next backoff.
        void result.then(finish, finish);
      } else {
        finish();
      }
    } catch {
      finish();
    }
  }

  private needsReconcile(ticket: RestoreTicket): boolean {
    return ticket.uncertain || ticket.settled;
  }

  private cancelTimer(ticket: { timer: RestoreTimer | null }): void {
    if (ticket.timer === null) return;
    this.cancel(ticket.timer);
    ticket.timer = null;
  }

  private release(id: string, ticket: { timer: RestoreTimer | null }): void {
    this.cancelTimer(ticket);
    if (this.tickets.get(id) === ticket) this.tickets.delete(id);
  }
}
