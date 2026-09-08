type RestoreRow = { id?: string; status: string | null };

/** Request fences outlive dialogs and remain until a successful restore is visible. */
export class PendingRestores {
  private readonly tickets = new Map<string, { settled: boolean; status: RestoreRow["status"] }>();
  private rows: ReadonlyArray<RestoreRow> | null = null;

  constructor(
    private readonly changed: (ids: ReadonlySet<string>) => void,
    private readonly retainOnError: (error: unknown) => boolean = () => false,
  ) {}

  has(id: string): boolean {
    return this.tickets.has(id);
  }

  run<T>(id: string, request: () => Promise<T>, status: RestoreRow["status"]): Promise<T> | null {
    if (this.has(id)) return null;
    const ticket = { settled: false, status };
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
            ticket.settled = true;
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
    const statuses = new Map(rows.map(row => [row.id, row.status]));
    let changed = false;
    for (const [id, ticket] of this.tickets) {
      if (ticket.settled && (!statuses.has(id) || statuses.get(id) !== ticket.status)) {
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
