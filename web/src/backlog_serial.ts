// One write at a time for a field whose request replaces its whole value (a title,
// the waits-on list): an older request must not commit after a newer one and
// overwrite it. A value submitted while one is in flight waits and goes out when it
// settles; only the latest waiting value is kept (coalesced), because each request
// carries the complete field.

export class SerialSaver<T> {
  private saving = false;
  private queued: { value: T } | null = null;

  /** `save` may return a promise that settles true once the write and the refresh
   *  after it have landed; a plain void return counts as settled at once. `settled`
   *  hears each request's own outcome. */
  constructor(
    private readonly save: (value: T) => Promise<boolean> | void,
    private readonly settled: (value: T, ok: boolean) => void,
  ) {}

  submit(value: T): void {
    if (this.saving) {
      this.queued = { value };
      return;
    }
    const done = this.save(value);
    if (!done) {
      this.settled(value, true);
      return;
    }
    this.saving = true;
    const finish = (ok: boolean): void => {
      this.saving = false;
      this.settled(value, ok);
      const next = this.queued;
      this.queued = null;
      if (next) this.submit(next.value);
    };
    done.then(finish, () => finish(false));
  }
}
