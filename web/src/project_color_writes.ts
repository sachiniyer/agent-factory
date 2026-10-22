/** Runs write one at a time per key, and only for the latest value: a value set
 *  while a write for its key is in flight replaces any value still waiting, so a
 *  fast run of choices persists the last one and never lands out of order. The
 *  returned promise settles once that key has nothing left to write. */
export function createLatestWriter(
  write: (key: string, value: string) => Promise<void>,
  onError: (e: unknown) => void,
): (key: string, value: string) => Promise<void> {
  const waiting = new Map<string, string>();
  const draining = new Map<string, Promise<void>>();
  const drain = async (key: string): Promise<void> => {
    for (let value = waiting.get(key); value !== undefined; value = waiting.get(key)) {
      waiting.delete(key);
      try {
        await write(key, value);
      } catch (e) {
        onError(e);
      }
    }
    draining.delete(key);
  };
  return (key, value) => {
    waiting.set(key, value);
    const running = draining.get(key);
    if (running) return running;
    const started = drain(key);
    draining.set(key, started);
    return started;
  };
}
