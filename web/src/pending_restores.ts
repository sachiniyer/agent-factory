type RestoreRow = { id?: string; archived: boolean };

/** Request fences outlive dialogs and remain until a successful restore is visible. */
export class PendingRestores {
  private readonly tickets = new Map<string, { settled: boolean }>();
  private rows: ReadonlyArray<RestoreRow> | null = null;

  constructor(private readonly changed: (ids: ReadonlySet<string>) => void) {}

  has(id: string): boolean {
    return this.tickets.has(id);
  }

  run<T>(id: string, request: () => Promise<T>): Promise<T> | null {
    if (this.has(id)) return null;
    const ticket = { settled: false };
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
          this.tickets.delete(id);
          this.changed(new Set(this.tickets.keys()));
        }
        throw error;
      }
    })();
  }

  observe(rows: ReadonlyArray<RestoreRow>): void {
    this.rows = rows;
    const archived = new Set(rows.filter(row => row.archived).map(row => row.id));
    let changed = false;
    for (const [id, ticket] of this.tickets) {
      if (ticket.settled && !archived.has(id)) {
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
