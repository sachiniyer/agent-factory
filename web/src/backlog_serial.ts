// One write at a time for a field whose request replaces its whole value (a title,
// the waits-on list, a body): an older request must not commit after a newer one and
// overwrite it. A request submitted while one is in flight waits and goes out when it
// settles; only the latest waiting request is kept (coalesced), because each carries
// the complete field. The queue is shared by key (queueFor), not owned by a panel:
// closing an item and reopening it while a save is in flight must not start a second
// queue whose older request could land after the newer one.

export class SerialQueue {
  private saving = false;
  private queued: { run: () => Promise<boolean> | void; done: (ok: boolean) => void } | null = null;

  /** `run` may return a promise that settles true once the write AND the refresh after
   *  it have landed; a plain void return counts as settled at once. `done` hears that
   *  request's own outcome. */
  submit(run: () => Promise<boolean> | void, done: (ok: boolean) => void): void {
    if (this.saving) {
      this.queued = { run, done };
      return;
    }
    const result = run();
    if (!result) {
      done(true);
      return;
    }
    this.saving = true;
    const finish = (ok: boolean): void => {
      this.saving = false;
      done(ok);
      const next = this.queued;
      this.queued = null;
      if (next) this.submit(next.run, next.done);
    };
    result.then(finish, () => finish(false));
  }
}

const queues = new Map<string, SerialQueue>();

/** The queue for one field of one item; the same key always answers the same queue. */
export function queueFor(key: string): SerialQueue {
  let q = queues.get(key);
  if (!q) {
    q = new SerialQueue();
    queues.set(key, q);
  }
  return q;
}

/** A field written through a serial queue, one value per request. */
export class SerialSaver<T> {
  constructor(
    private readonly save: (value: T) => Promise<boolean> | void,
    private readonly settled: (value: T, ok: boolean) => void,
    private readonly queue: SerialQueue = new SerialQueue(),
  ) {}

  submit(value: T): void {
    this.queue.submit(() => this.save(value), (ok) => this.settled(value, ok));
  }
}

// The value a field was last submitted with, until a confirming read carries it, kept
// by key like the queues: it must outlive the panel that submitted it, or a reopened
// panel would build its next change from the stale stored value.
const drafts = new Map<string, unknown>();

export function getDraft<T>(key: string): T | null {
  return drafts.has(key) ? (drafts.get(key) as T) : null;
}

export function setDraft<T>(key: string, value: T | null): void {
  if (value === null) drafts.delete(key);
  else drafts.set(key, value);
}

/** Forgets every queue and draft: tests start each case clean. */
export function resetSerialState(): void {
  drafts.clear();
  queues.clear();
}
