// The Docs pane's roster: which docs the daemon has. The doc list reaches the pane by
// several routes (the first load, a refresh after a change event, the fetch after Add doc),
// and their replies can arrive in any order. A doc missing from a list is taken as deleted,
// so a reply older than one already applied must never be used: the list it carries can
// lack a doc that was added since. The pane and the model test share this code.

export class RosterOrder {
  private issued = 0;
  private applied = 0;

  /** A request for the list goes out: its ticket orders it among the others. */
  begin(): number {
    return ++this.issued;
  }

  /** A reply arrives. It is used only if no reply to a later request has been used. */
  accept(ticket: number): boolean {
    if (ticket < this.applied) return false;
    this.applied = ticket;
    return true;
  }
}

/** The open docs the list no longer has: deleted, as far as this reply says. */
export function missingFrom(open: Iterable<string>, listed: Iterable<string>): string[] {
  const have = new Set(listed);
  return [...open].filter((name) => !have.has(name));
}
