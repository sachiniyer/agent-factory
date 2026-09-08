type RestoreRow = { id?: string; restoreEligible: boolean };
export type RestoreEvidence =
  | { kind: "snapshot"; generation: number; operationLockTimeoutMs?: number }
  | { kind: "updated" | "restored"; id: string };

// Network delivery and handler scheduling get a small margin after the daemon's
// own admission deadline before an unchanged row proves no restore is queued.
export const RESTORE_ADMISSION_MARGIN_MS = 1_000;

/** Request fences outlive dialogs and remain until a successful restore is visible. */
export class PendingRestores {
  private readonly tickets = new Map<string, {
    settled: boolean;
    uncertain: boolean;
    succeededAt: number | null;
    uncertainAt: number;
    uncertainSince: number;
    sawBusy: boolean;
    restoreEligible: RestoreRow["restoreEligible"];
  }>();
  private snapshotGeneration = 0;
  private rows: ReadonlyArray<RestoreRow> | null = null;

  constructor(
    private readonly changed: (ids: ReadonlySet<string>) => void,
    private readonly retainOnError: (error: unknown) => boolean = () => false,
    private readonly committedOnError: (error: unknown) => boolean = () => false,
    private readonly now: () => number = Date.now,
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
            if (this.committedOnError(error)) {
              ticket.settled = true;
              ticket.succeededAt = this.snapshotGeneration;
            }
            if (this.rows) this.observe(this.rows);
          } else {
            this.tickets.delete(id);
            this.changed(new Set(this.tickets.keys()));
          }
        }
        throw error;
      }
    })();
  }

  observe(rows: ReadonlyArray<RestoreRow>): void {
    this.rows = rows;
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
      let uncertainCompleted = false;
      if (ticket.uncertain && authoritative && causalUncertainSnapshot) {
        if (!eligibility.has(id)) {
          uncertainCompleted = true;
        } else if (!row?.restoreEligible) {
          ticket.sawBusy = true;
        } else {
          const timeout = evidence.operationLockTimeoutMs;
          uncertainCompleted = ticket.sawBusy || (typeof timeout === "number" && timeout >= 0 &&
            this.now() - ticket.uncertainSince >= timeout + RESTORE_ADMISSION_MARGIN_MS);
        }
      }
      if ((ticket.settled && (observedAfterSuccess || !eligibility.has(id) || (ticket.restoreEligible && !eligibility.get(id)))) || uncertainCompleted) {
        this.tickets.delete(id);
        changed = true;
      }
    }
    if (changed) this.changed(new Set(this.tickets.keys()));
  }

  reset(): void {
    this.tickets.clear();
    this.rows = null;
    this.changed(new Set());
  }
}
