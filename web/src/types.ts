// The wire DTOs the sidebar reads (#1592 Phase 5 PR3). These are a hand-mirror of
// the Go source of truth — NOT a fork of it: SessionData mirrors the subset of
// session.InstanceData (session/storage.go) the rail renders, and the Liveness /
// InFlightOp enums mirror session/liveness.go. Per design §3.2 the client reuses
// the daemon's projection shapes verbatim; it must not invent its own status
// logic. (FLAG §7.1: hand-mirror vs codegen — hand-mirror in v1, revisit if it
// drifts.)
//
// Liveness and InFlightOp are Go `int` types with no custom MarshalJSON, so they
// travel as bare integers on the wire; these const objects pin the exact numeric
// values from session/liveness.go's iota blocks. Adding a value there is a
// deliberate, breaking change here too — the same "the switch is TOTAL" discipline
// the TUI renderer keeps (ui/tree/render.go:274).

/** session.Liveness (session/liveness.go): the daemon-owned health axis. */
export const Liveness = {
  Unset: 0,
  Running: 1,
  Ready: 2,
  Lost: 3,
  Dead: 4,
  Archived: 5,
  LimitReached: 6,
} as const;

/** session.InFlightOp (session/liveness.go): the transient client-op axis. */
export const InFlightOp = {
  None: 0,
  Creating: 1,
  Killing: 2,
  Archiving: 3,
  Restoring: 4,
} as const;

/** session.Status (session/instance.go) — the legacy single-axis int, read ONLY
 *  as a defensive fallback when a projection somehow omits `liveness` (never
 *  expected from the daemon's live Snapshot, which always emits it). */
export const Status = {
  Running: 0,
  Ready: 1,
  Loading: 2,
  Deleting: 3,
  Dead: 4,
  Lost: 5,
  Archived: 6,
} as const;

/**
 * The subset of session.InstanceData (session/storage.go) the sidebar renders.
 * Field names and JSON tags match the Go struct exactly so this decodes the
 * daemon projection as-is. Optional fields carry Go's `omitempty` semantics:
 * `liveness`/`in_flight_op` are dropped when zero, `limit_reset_at` only present
 * for a LimitReached row.
 */
export interface SessionData {
  id?: string;
  title: string;
  branch: string;
  path?: string;
  /** RFC3339 creation time; the rail orders rows by it for a stable list. */
  created_at?: string;
  /** Legacy single-axis status int (fallback source only; see Status). */
  status?: number;
  /** Daemon-owned health axis; absent (→ Unset) only on pre-#1195 records. */
  liveness?: number;
  /** Transient client-op axis; absent (→ None) in the steady state. */
  in_flight_op?: number;
  /** Usage-limit reset time (RFC3339), present only for a LimitReached row. */
  limit_reset_at?: string;
  /** Backend discriminator; "remote" marks a remote-hook session (→ [remote]). */
  backend_type?: string;
}

/** The Snapshot RPC response (daemon/snapshot.go: SnapshotResponse). */
export interface SnapshotResponse {
  instances: SessionData[] | null;
  delivery_alarms?: unknown[];
}

/** agentproto.EventType (agentproto/message.go): the /v1/events discriminators. */
export type EventType =
  | "session.created"
  | "session.updated"
  | "session.killed"
  | "session.archived"
  | "session.restored"
  | "task.created"
  | "task.updated"
  | "task.removed";

/**
 * agentproto.Event (agentproto/message.go): one message on the /v1/events plane.
 * A session.* event's `data` is a marshaled InstanceData; created/updated carry
 * the full projection, while killed/archived/restored carry only `{title}` (see
 * daemon/control_server.go), so a client keys deletes/refetches off the title.
 */
export interface WireEvent {
  type: EventType;
  data?: SessionData;
}
