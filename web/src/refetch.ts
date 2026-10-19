// The "latest request wins" fence, in one place, for every async read the web client
// issues more than once (#3659).
//
// Several projections refetch from more than one trigger — a mutation, a debounced
// event resync, a view switch, a poll — so two requests for the SAME projection are
// routinely in flight at once. Promises settle in whatever order the network hands
// them back, so without a fence the SLOWER one wins: it commits the state that was
// true before the mutation, and the view keeps showing it until something else
// happens to refresh. #2330 fixed that for the session snapshot and #3654 for the
// task list, each with its own hand-written counter; this is the third copy, so it
// becomes the mechanism instead.
//
// The rule has two halves, and the second is the one that gets forgotten:
//
//   - the GENERATION — a response is stale if a newer request for the same
//     projection went out after it left, whatever it contains.
//   - the TOKEN — a response is stale if the credential rotated while it was in
//     flight, because it then describes a daemon session that is no longer the one
//     on screen. A generation counter alone cannot see that.
//
// index.ts's refreshDaemonPalette is the shape this generalizes and deliberately
// keeps its own hand-written pair: it is an async/await body whose failure path
// retries on a timer and can route a rejected credential into a full disconnect, so
// it does much more than "commit or drop".

/** Hands out request tickets and can invalidate every outstanding one at once. */
export interface LatestRequestGate {
  begin(): LatestRequest;
  invalidate(): void;
}

/** One outstanding request; `isCurrent()` goes false as soon as a newer one begins. */
export interface LatestRequest {
  isCurrent(): boolean;
}

/** Fences asynchronous reads that share the same login token. */
export function createLatestRequestGate(): LatestRequestGate {
  let generation = 0;
  return {
    begin() {
      const requestGeneration = ++generation;
      return { isCurrent: () => requestGeneration === generation };
    },
    invalidate() {
      generation += 1;
    },
  };
}

/** One projection's fenced refetch: `refresh()` issues a request; `invalidate()`
 *  disowns whatever is already in flight without issuing one. */
export interface FencedRefetcher {
  refresh(): void;
  invalidate(): void;
}

export interface FencedRefetchSpec<T> {
  /** The installed credential. `null` is disconnected and no request goes out; ""
   *  is the authorized tokenless credential (#1696) and is a normal token here. */
  readToken: () => string | null;
  /** The single RPC this projection reads. When timeoutMs is set the request
   *  also receives an AbortSignal; a fetch that ignores it simply settles late
   *  and is disowned by the expiry fence below. */
  fetch: (token: string, signal?: AbortSignal) => Promise<T>;
  /** Bounds each request. fetch carries no deadline of its own, so without this
   *  a read the daemon accepts and never answers holds a per-origin connection
   *  slot open for the life of the page — and a caller that retries (a conflict
   *  timer, a mutation's settle refetch) piles up one more each round (#4888
   *  review). On expiry the request is aborted and disowned: a late answer can
   *  still resolve the promise but commits nothing. */
  timeoutMs?: number;
  /** Commits the answer. Called only for the newest request, and only while the
   *  credential it was issued under is still installed. */
  commit: (value: T) => void;
  /** Reports a failure, under the same fence — an older request's error must not
   *  paint over a newer request's answer. Omitted means the failure is swallowed
   *  (the caller's events plane or next mutation retries). */
  onError?: (error: unknown) => void;
}

/** Builds a refetcher for one projection. Every response it admits is the newest
 *  one issued under the still-installed credential; every other response — an older
 *  request that overtook it, one issued before a token rotation, one outstanding
 *  across a stream teardown — commits nothing at all, success or failure. */
export function createFencedRefetcher<T>(spec: FencedRefetchSpec<T>): FencedRefetcher {
  const gate = createLatestRequestGate();
  return {
    refresh(): void {
      const tok = spec.readToken();
      // `=== null` not `!tok`: "" is the authorized tokenless credential (#1696).
      if (tok === null) {
        return;
      }
      const request = gate.begin();
      // The timeout's own fence: an expired request cannot commit even when it
      // still looks newest — the gate only sees requests that began after it,
      // and nothing newer may ever begin.
      let expired = false;
      const isNewest = (): boolean => !expired && request.isCurrent() && spec.readToken() === tok;
      let controller: AbortController | undefined;
      // ReturnType — DOM says number, Node says Timeout; the clear call takes
      // either, and this file runs under both.
      let bound: ReturnType<typeof setTimeout> | undefined;
      if (spec.timeoutMs !== undefined) {
        controller = new AbortController();
        const own = controller;
        // globalThis, not window: refetch.ts also runs under node:test, where
        // no window exists — and the same timers do anyway.
        bound = globalThis.setTimeout(() => {
          expired = true;
          own.abort();
        }, spec.timeoutMs);
      }
      void spec
        .fetch(tok, controller?.signal)
        .then((value) => {
          if (isNewest()) {
            spec.commit(value);
          }
        })
        .catch((error: unknown) => {
          if (isNewest()) {
            spec.onError?.(error);
          }
        })
        .finally(() => {
          if (bound !== undefined) {
            globalThis.clearTimeout(bound);
          }
        });
    },
    invalidate(): void {
      gate.invalidate();
    },
  };
}
