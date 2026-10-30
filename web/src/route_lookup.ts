/** Tickets for deep-link lookups by id (#6178). Each route resolution takes a new
 *  ticket; an answer is applied only while its ticket is still the latest, so a
 *  late archive read for a hash the user already left — or one superseded by a
 *  second resolution of the same hash — settles nothing. */
export class RouteLookups {
  private latest = 0;

  /** Takes a ticket, superseding every earlier one. */
  begin(): number {
    return ++this.latest;
  }

  current(ticket: number): boolean {
    return ticket === this.latest;
  }
}

export type LookupOutcome = "stale" | "missing" | "found";

/** Settles one by-id lookup: exactly one terminal effect per live ticket, none for
 *  a stale one, whichever order lookups answer in. */
export function settleLookup<T extends { id?: string }>(
  lookups: RouteLookups,
  ticket: number,
  id: string,
  rows: T[],
  effects: { found: (row: T) => void; missing: () => void },
): LookupOutcome {
  if (!lookups.current(ticket)) return "stale";
  const row = rows.find((r) => r.id === id);
  if (!row) {
    effects.missing();
    return "missing";
  }
  effects.found(row);
  return "found";
}
