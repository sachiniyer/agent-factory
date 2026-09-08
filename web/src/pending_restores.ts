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

/** Request fences outlive dialogs and remain until a successful restore is visible. */
export class PendingRestores {
  private readonly tickets = new Map<string, {
    settled: boolean;
    uncertain: boolean;
    succeededAt: number | null;
    uncertainAt: number;
    uncertainSince: number;
    sawBusy: boolean;
    timer: RestoreTimer | null;
    retryDelayMs: number;
    restoreEligible: RestoreRow["restoreEligible"];
  }>();
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
    private readonly requestReconcile: () => void = () => {},
    private readonly schedule: (callback: () => void, delayMs: number) => RestoreTimer =
      (callback, delayMs) => globalThis.setTimeout(callback, delayMs),
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
      sawBusy: false,
      timer: null,
      retryDelayMs: RESTORE_RECONCILE_RETRY_MIN_MS,
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
        }
        return result;
      } catch (error) {
        // A response from an old connection cannot release a newer request.
        if (this.tickets.get(id) === ticket) {
          if (this.retainOnError(error)) {
            ticket.uncertain = true;
            ticket.uncertainAt = this.snapshotGeneration;
            ticket.uncertainSince = this.now();
            ticket.sawBusy = false;
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
      for (const [id, ticket] of this.tickets) this.armUncertainTimer(id, ticket);
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
          uncertainCompleted = ticket.sawBusy || (deadline !== null && issuedAt !== undefined && issuedAt > deadline);
        } else {
          // LifecycleActionNone covers every operation fence and several unsettled
          // states. Only a positive Archive capability proves restore completion.
          ticket.sawBusy = true;
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
      ticket.retryDelayMs = RESTORE_RECONCILE_RETRY_MIN_MS;
      if (ticket.settled) this.tickets.delete(id);
    }
    this.rows = null;
    // Keep issuance stamps monotonic for requests that survive reconnect.
    this.changed(new Set(this.tickets.keys()));
  }

  private armUncertainTimer(id: string, ticket: {
    uncertain: boolean; uncertainSince: number; timer: RestoreTimer | null; retryDelayMs: number;
  }): void {
    if (!ticket.uncertain || ticket.timer !== null || this.operationLockTimeoutMs === null) return;
    const remaining = Math.max(0,
      ticket.uncertainSince + this.operationLockTimeoutMs + RESTORE_ADMISSION_MARGIN_MS - this.now());
    ticket.timer = this.schedule(() => {
      ticket.timer = null;
      if (this.tickets.get(id) !== ticket || !ticket.uncertain) return;
      this.requestReconcile();
      if (this.tickets.get(id) === ticket && ticket.uncertain) this.armRetryTimer(id, ticket);
    }, remaining);
  }

  private armRetryTimer(id: string, ticket: {
    uncertain: boolean; timer: RestoreTimer | null; retryDelayMs: number;
  }): void {
    if (ticket.timer !== null) return;
    const delay = ticket.retryDelayMs;
    ticket.retryDelayMs = Math.min(ticket.retryDelayMs * 2, RESTORE_RECONCILE_RETRY_MAX_MS);
    ticket.timer = this.schedule(() => {
      ticket.timer = null;
      if (this.tickets.get(id) !== ticket || !ticket.uncertain) return;
      this.requestReconcile();
      if (this.tickets.get(id) === ticket && ticket.uncertain) this.armRetryTimer(id, ticket);
    }, delay);
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
