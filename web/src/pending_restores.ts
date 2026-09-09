type RestoreRow = { id?: string; restoreEligible: boolean; restoreSettled?: boolean };
export type RestoreEvidence =
  | { kind: "snapshot"; generation: number; operationLockTimeoutMs?: number }
  | { kind: "updated" | "restored"; id: string };

// Network delivery and handler scheduling get a small margin after the daemon's
// own admission deadline before an unchanged row proves no restore is queued.
export const RESTORE_ADMISSION_MARGIN_MS = 1_000;
export const RESTORE_RECONCILE_RETRY_MIN_MS = 2_000;
const RESTORE_RECONCILE_RETRY_MAX_MS = 10_000;
const SNAPSHOT_ISSUANCE_HISTORY = 128;

type RestoreTimer = ReturnType<typeof globalThis.setTimeout>;
type RestoreTicket = {
  settled: boolean;
  uncertain: boolean;
  succeededAt: number | null;
  uncertainAt: number;
  uncertainSince: number;
  timer: RestoreTimer | null;
  retryDelayMs: number;
  reconcileGeneration: number;
  restoreEligible: RestoreRow["restoreEligible"];
};

/** Request fences outlive dialogs and remain until a successful restore is visible. */
export class PendingRestores {
  private readonly tickets = new Map<string, RestoreTicket>();
  private snapshotGeneration = 0;
  private readonly snapshotIssuedAt = new Map<number, number>();
  private rows: ReadonlyArray<RestoreRow> | null = null;
  private operationLockTimeoutMs: number | null = null;

  constructor(
    private readonly changed: (ids: ReadonlySet<string>) => void,
    private readonly retainOnError: (error: unknown) => boolean = () => false,
    private readonly committedOnError: (error: unknown) => boolean = () => false,
    // Match the daemon's monotonic operation-lock deadline across machine sleep.
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
    const generation = ++this.snapshotGeneration;
    this.snapshotIssuedAt.set(generation, this.now());
    if (this.snapshotIssuedAt.size > SNAPSHOT_ISSUANCE_HISTORY) {
      const oldest = this.snapshotIssuedAt.keys().next().value;
      if (typeof oldest === "number") this.snapshotIssuedAt.delete(oldest);
    }
    return generation;
  }

  observe(rows: ReadonlyArray<RestoreRow>, evidence?: RestoreEvidence): void {
    this.rows = rows;
    if (evidence?.kind === "snapshot") {
      if (typeof evidence.operationLockTimeoutMs === "number" && evidence.operationLockTimeoutMs >= 0 &&
        this.operationLockTimeoutMs !== evidence.operationLockTimeoutMs) {
        this.operationLockTimeoutMs = evidence.operationLockTimeoutMs;
        for (const ticket of this.tickets.values()) this.cancelTimer(ticket);
      }
      for (const [id, ticket] of this.tickets) this.armTimerForState(id, ticket);
    }
    const eligibility = new Map(rows.map(row => [row.id, row.restoreEligible]));
    let changed = false;
    for (const [id, ticket] of this.tickets) {
      const row = rows.find(candidate => candidate.id === id);
      const authoritative = evidence?.kind === "updated" || evidence?.kind === "restored" || evidence?.kind === "snapshot";
      // A delayed recover-fence update may arrive after HTTP success. Only a
      // Snapshot issued afterward proves completion. Identity-only restored events
      // carry no attempt id and may belong to a previous restore cycle.
      const observedAfterSuccess = ticket.succeededAt !== null && evidence?.kind === "snapshot" &&
        evidence.generation > ticket.succeededAt;
      const causalUncertainSnapshot = evidence?.kind === "snapshot" &&
        evidence.generation > ticket.uncertainAt;
      const issuedAt = evidence?.kind === "snapshot" ? this.snapshotIssuedAt.get(evidence.generation) : undefined;
      let uncertainCompleted = false;
      if (ticket.uncertain && authoritative && causalUncertainSnapshot) {
        if (!eligibility.has(id)) {
          uncertainCompleted = true;
        } else if (row?.restoreSettled) {
          uncertainCompleted = true;
        } else if (row?.restoreEligible) {
          // A reconnect can hold these captured rows behind slower task/project
          // loads. Processing time cannot turn a pre-deadline Snapshot into proof.
          const deadline = this.operationLockTimeoutMs === null ? null :
            ticket.uncertainSince + this.operationLockTimeoutMs + RESTORE_ADMISSION_MARGIN_MS;
          uncertainCompleted = deadline !== null && issuedAt !== undefined && issuedAt > deadline;
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
    const remaining = Math.max(0,
      ticket.uncertainSince + this.operationLockTimeoutMs + RESTORE_ADMISSION_MARGIN_MS - this.now());
    ticket.timer = this.schedule(() => {
      ticket.timer = null;
      if (this.tickets.get(id) !== ticket || !ticket.uncertain) return;
      this.reconcile(id, ticket);
    }, remaining);
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
