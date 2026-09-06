// Local feedback layered over daemon-owned state. Never feed a projected row back
// into the authoritative reducer: a failed request must reveal the latest daemon
// state, not restore a saved row over events received while the RPC was pending.
import type { CreateSessionInput } from "./api.js";
import { applyEvent, sessionKey, upsertSession } from "./sessions.js";
import { InFlightOp, type SessionData, type WireEvent } from "./types.js";

export interface MutationTicket {
  readonly epoch: number;
  readonly sequence: number;
}
interface Pending {
  ticket: MutationTicket;
  kind: "create" | "archive" | "kill";
  row: SessionData;
  acknowledged: boolean;
  confirmed: boolean;
  startedRevision: number;
  existingKeys?: Set<string>;
}

export class OptimisticSessions {
  private authoritative: SessionData[] = [];
  private pending = new Map<number, Pending>();
  private epoch = 0;
  private sequence = 0;
  private revision = 0;
  private sessionEvents = new Map<string, { revision: number; completed: boolean }>();

  reset(sessions: SessionData[] = []): void {
    this.epoch++;
    this.revision++;
    this.pending.clear();
    this.sessionEvents.clear();
    this.authoritative = sessions;
  }

  /** Captured before fetching, so an older snapshot cannot undo an RPC or event. */
  snapshotFence(): number { return this.revision; }

  snapshot(sessions: SessionData[], fence: number): boolean {
    if (fence !== this.revision) return false;
    this.authoritative = sessions;
    this.revision++;
    for (const [key, op] of this.pending) {
      if (op.acknowledged) this.pending.delete(key);
    }
    return true;
  }

  beginCreate(input: CreateSessionInput, now = new Date().toISOString()): MutationTicket {
    const ticket = this.ticket();
    this.pending.set(ticket.sequence, {
      ticket, kind: "create", acknowledged: false, confirmed: false, startedRevision: this.revision,
      existingKeys: new Set(this.authoritative.map(sessionKey)),
      row: { id: `optimistic:${ticket.epoch}:${ticket.sequence}`, title: input.title,
        branch: "", created_at: now, worktree: { repo_path: input.repoPath },
        in_flight_op: InFlightOp.Creating },
    });
    return ticket;
  }

  begin(kind: "archive" | "kill", row: SessionData): MutationTicket | null {
    // The UI can deliver two clicks before its DOM patches. Do not send a second
    // destructive RPC for a target already owned by a local request.
    if ([...this.pending.values()].some(op => sessionKey(op.row) === sessionKey(row))) return null;
    const ticket = this.ticket();
    this.pending.set(ticket.sequence, { ticket, kind, row, acknowledged: false, confirmed: false, startedRevision: this.revision });
    return ticket;
  }

  isCurrent(ticket: MutationTicket): boolean {
    return ticket.epoch === this.epoch && this.pending.get(ticket.sequence)?.ticket === ticket;
  }

  succeed(ticket: MutationTicket, created?: SessionData): boolean {
    if (!this.isCurrent(ticket)) return false;
    this.revision++;
    const op = this.pending.get(ticket.sequence)!;
    if (created) {
      const latest = this.sessionEvents.get(sessionKey(created));
      // Creating precedes this response and must yield to its final projection.
      // A completed event or later kill already describes what happened after
      // creation: a delayed HTTP response must not rewind or resurrect that row.
      if (!latest?.completed || latest.revision <= op.startedRevision) {
        this.authoritative = upsertSession(this.authoritative, created);
      }
      this.pending.delete(ticket.sequence);
    } else if (op.confirmed) {
      this.pending.delete(ticket.sequence);
    } else {
      // Archive/kill RPCs return no projection. Retain feedback until a snapshot
      // requested AFTER the acknowledgement or a matching completion event.
      op.acknowledged = true;
    }
    return true;
  }

  fail(ticket: MutationTicket): boolean {
    if (!this.isCurrent(ticket)) return false;
    this.revision++;
    this.pending.delete(ticket.sequence);
    return true;
  }

  event(event: WireEvent): boolean {
    const result = applyEvent(this.authoritative, event);
    this.authoritative = result.sessions;
    this.revision++;
    if (event.data && (event.type === "session.created" || event.type === "session.updated" ||
      event.type === "session.archived" || event.type === "session.restored" || event.type === "session.killed")) {
      const completed = event.type === "session.killed" ||
        (event.data.liveness !== undefined && event.data.in_flight_op !== InFlightOp.Creating);
      this.sessionEvents.set(sessionKey(event.data), { revision: this.revision, completed });
    }
    for (const [key, op] of this.pending) {
      if (!event.data || sessionKey(event.data) !== sessionKey(op.row)) continue;
      const completed = (op.kind === "kill" && event.type === "session.killed") ||
        (op.kind === "archive" && event.type === "session.archived" && event.data.liveness !== undefined);
      if (completed) {
        op.confirmed = true;
        if (op.acknowledged) this.pending.delete(key);
      }
    }
    return result.needsResync;
  }

  project(): SessionData[] {
    let rows = this.authoritative;
    const creating = this.authoritative.filter(row => row.in_flight_op === InFlightOp.Creating);
    const displayed = new Set<string>();
    for (const op of this.pending.values()) {
      if (op.confirmed) continue;
      if (op.kind === "create") {
        // Coalesce identical visible feedback, without claiming that this daemon
        // identity belongs to our request. Only its RPC response can do that.
        // A concurrent creator may own the same title; failure never removes it.
        const match = creating.find(row => !displayed.has(sessionKey(row)) &&
          !op.existingKeys?.has(sessionKey(row)) && row.title === op.row.title &&
          row.worktree?.repo_path === op.row.worktree?.repo_path);
        if (match) displayed.add(sessionKey(match));
        else rows = [...rows, op.row];
      } else {
        rows = rows.map(row => sessionKey(row) === sessionKey(op.row) ? {
          ...row, in_flight_op: op.kind === "archive" ? InFlightOp.Archiving : InFlightOp.Killing,
          lifecycle_action: undefined, can_kill: false, can_handoff: false,
        } : row);
      }
    }
    return rows;
  }

  private ticket(): MutationTicket {
    this.revision++;
    return { epoch: this.epoch, sequence: ++this.sequence };
  }
}
