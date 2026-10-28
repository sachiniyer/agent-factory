export interface LatestWriter<T> {
  /** Queues value for key. The promise settles once key has nothing left to write. */
  set(key: string, value: T): Promise<void>;
  /** Drops key's waiting value. The promise settles once its in-flight write, and
   *  anything set meanwhile, has finished, so a caller can order a mutation after it. */
  cancel(key: string): Promise<void>;
}

/** Runs write one at a time per key, and only for the latest value: a value set
 *  while a write for its key is in flight replaces any value still waiting, so a
 *  fast run of choices persists the last one and never lands out of order. */
export function createLatestWriter<T>(
  write: (key: string, value: T) => Promise<void>,
  onError: (e: unknown, key: string, value: T) => void,
): LatestWriter<T> {
  const waiting = new Map<string, { value: T }>();
  const draining = new Map<string, Promise<void>>();
  const drain = async (key: string): Promise<void> => {
    for (let next = waiting.get(key); next !== undefined; next = waiting.get(key)) {
      waiting.delete(key);
      try {
        await write(key, next.value);
      } catch (e) {
        onError(e, key, next.value);
      }
    }
    draining.delete(key);
  };
  return {
    set(key, value) {
      waiting.set(key, { value });
      const running = draining.get(key);
      if (running) return running;
      const started = drain(key);
      draining.set(key, started);
      return started;
    },
    cancel(key) {
      waiting.delete(key);
      return draining.get(key) ?? Promise.resolve();
    },
  };
}
