export type LookupOutcome = "stale" | "missing" | "found";

/** Settles one by-id deep-link lookup (#6178, #6204): its answer applies only while
 *  `intent` still says the navigation it was resolving is the user's latest, so a
 *  rail click, keyboard selection, new hash or back/forward since supersedes it.
 *  Exactly one terminal effect for a live ticket, none for a stale one. */
export function settleLookup<T extends { id?: string }>(
  intent: { isCurrent(ticket: number): boolean },
  ticket: number,
  id: string,
  rows: T[],
  effects: { found: (row: T) => void; missing: () => void },
): LookupOutcome {
  if (!intent.isCurrent(ticket)) return "stale";
  const row = rows.find((r) => r.id === id);
  if (!row) {
    effects.missing();
    return "missing";
  }
  effects.found(row);
  return "found";
}
