/** Request fences outlive progress dialogs and event-stream disconnects. */
export class PendingRestores {
  private readonly tickets = new Map<string, object>();

  constructor(private readonly changed: (ids: ReadonlySet<string>) => void) {}

  has(id: string): boolean {
    return this.tickets.has(id);
  }

  run<T>(id: string, request: () => Promise<T>): Promise<T> | null {
    if (this.has(id)) return null;
    const ticket = {};
    this.tickets.set(id, ticket);
    this.changed(new Set(this.tickets.keys()));
    return (async () => {
      try {
        return await request();
      } finally {
        // A response from an old connection cannot release a newer request.
        if (this.tickets.get(id) === ticket) {
          this.tickets.delete(id);
          this.changed(new Set(this.tickets.keys()));
        }
      }
    })();
  }

  reset(): void {
    this.tickets.clear();
    this.changed(new Set());
  }
}
