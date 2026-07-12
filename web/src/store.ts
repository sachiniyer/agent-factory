// A tiny observable store (#1592 Phase 5, design §3.1). It is the browser analogue
// of the TUI's read-only projection: a plain object updated in one place, with
// subscriber callbacks that re-render the affected DOM. No framework runtime — the
// surface is small enough that a ~20-line store beats a dependency we go:embed into
// every binary. PR3+ replaces the placeholder session state with the live
// SnapshotResponse mirror fed by /v1/events.

export type Listener<T> = (state: T) => void;

export class Store<T> {
  private state: T;
  private readonly listeners = new Set<Listener<T>>();

  constructor(initial: T) {
    this.state = initial;
  }

  /** The current immutable snapshot of state. */
  get(): T {
    return this.state;
  }

  /** Merges a partial update and notifies every subscriber with the new state. */
  set(patch: Partial<T>): void {
    this.state = { ...this.state, ...patch };
    for (const listener of this.listeners) {
      listener(this.state);
    }
  }

  /** Registers a listener and returns an unsubscribe function. */
  subscribe(listener: Listener<T>): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }
}
