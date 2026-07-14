// The status-dot + title mapping for a sidebar row (#1592 Phase 5 PR3). This is a
// line-for-line port of the TUI renderer, ui/tree/render.go — the single source of
// truth for how (Liveness, InFlightOp) become a glyph, a color, and the [lost] /
// [deleting] / [limit] / [remote] title prefixes. The web MUST match it exactly:
// two thin clients of the same projection cannot diverge in status semantics, only
// in pixels (design §3). Adding a Liveness value forces a deliberate choice here,
// mirroring the TUI's TOTAL liveness switch (render.go:274-301).
//
// Glyph shapes are copied from render.go so the state survives low contrast and
// color-blindness the same way the TUI intends (its #935 discipline): a filled ●
// for Ready, hollow ○/◌ for Dead/Lost, ▧ for Archived, ◆ for LimitReached. Colors
// (the `kind`) map to the exact hexes render.go paints (see styles.css .af-dot-*).

import { InFlightOp, Liveness, Status, type SessionData } from "./types.js";

/** The visual kind of a status dot, one per color bucket the TUI paints. Drives
 *  the .af-dot-<kind> CSS class whose color matches render.go's lipgloss styles.
 *  A working/busy row has no dot (#1765), so there is no "working" bucket. */
export type DotKind = "ready" | "lost" | "dead" | "archived" | "limit";

/** A fully-resolved status descriptor for one row: the dot to draw and the
 *  human label (used for the row's aria/title so the state is legible to a
 *  screen reader, not only by color — the same intent as the TUI's text prefixes). */
export interface RowStatus {
  /** The glyph to draw, or "" for a working/busy row which shows no dot (#1765). */
  glyph: string;
  /** The dot's color bucket, or null for a working row (the dot is omitted). */
  kind: DotKind | null;
  /** Accessible one-word state label, e.g. "Ready", "Working", "Lost". */
  label: string;
}

// Glyphs copied verbatim from ui/tree/render.go (readyIcon/deadIcon/lostIcon/
// archivedIcon/limitIcon), minus the trailing pad space the terminal adds.
const READY_GLYPH = "●";
const DEAD_GLYPH = "○";
const LOST_GLYPH = "◌";
const ARCHIVED_GLYPH = "▧";
const LIMIT_GLYPH = "◆";

// A working/busy row shows NO status dot (#1765): the TUI renders a blank status
// cell for LiveRunning / any in-flight op, and the web omits the dot entirely.
// Kept as a resolved status (empty glyph, null kind) so rowStatus stays total and
// callers can still detect the working state (isWorking, the project glance count).
const WORKING: RowStatus = { glyph: "", kind: null, label: "Working" };

/**
 * Resolves a session's status dot from its two axes, mirroring render.go exactly:
 * any in-flight op is a working/busy state and shows no dot (#1765); otherwise the
 * liveness picks the dot. Falls back to the legacy `status` int only when `liveness`
 * is absent (a pre-#1195 record — never emitted by the daemon's live Snapshot, but
 * handled so a stray zero renders as working, matching render.go's LivenessUnset arm).
 */
export function rowStatus(s: SessionData): RowStatus {
  const op = s.in_flight_op ?? InFlightOp.None;
  // An in-flight op overlays the liveness and reads as working (render.go:280-282),
  // so it shows no dot; the [deleting] title prefix (rowTitle) distinguishes
  // kill/archive.
  if (op !== InFlightOp.None) {
    return WORKING;
  }
  return dotForLiveness(livenessOf(s));
}

/** True when the row is a working/busy session — the state that shows NO status
 *  dot (#1765). Kept exported so the project switcher's per-project "working"
 *  glance count (project.ts) stays derivable now that the dot itself is gone. */
export function isWorking(s: SessionData): boolean {
  return rowStatus(s).kind === null;
}

/** The daemon always emits `liveness`; this only guards a pre-#1195 record by
 *  deriving it from the legacy `status` int (session.LivenessForStatus). */
function livenessOf(s: SessionData): number {
  const lv = s.liveness ?? Liveness.Unset;
  if (lv !== Liveness.Unset) {
    return lv;
  }
  switch (s.status) {
    case Status.Ready:
      return Liveness.Ready;
    case Status.Dead:
      return Liveness.Dead;
    case Status.Lost:
      return Liveness.Lost;
    case Status.Archived:
      return Liveness.Archived;
    // Running and the transient values (Loading/Deleting, which never persist)
    // fall through to the working dot, matching render.go's LivenessUnset arm.
    default:
      return Liveness.Running;
  }
}

/** The TOTAL liveness→dot switch, one arm per value (render.go:284-301). */
function dotForLiveness(lv: number): RowStatus {
  switch (lv) {
    case Liveness.Ready:
      return { glyph: READY_GLYPH, kind: "ready", label: "Ready" };
    case Liveness.Lost:
      return { glyph: LOST_GLYPH, kind: "lost", label: "Lost" };
    case Liveness.Dead:
      return { glyph: DEAD_GLYPH, kind: "dead", label: "Dead" };
    case Liveness.Archived:
      return { glyph: ARCHIVED_GLYPH, kind: "archived", label: "Archived" };
    case Liveness.LimitReached:
      return { glyph: LIMIT_GLYPH, kind: "limit", label: "Limit reached" };
    // LiveRunning and LivenessUnset both render as working (render.go:285, 297).
    case Liveness.Running:
    case Liveness.Unset:
    default:
      return WORKING;
  }
}

/** True when the row is an archived session (dimmed, no text prefix — the ▧ glyph
 *  and dimming already convey it), mirroring render.go:335. */
export function isArchived(s: SessionData): boolean {
  return livenessOf(s) === Liveness.Archived;
}

/**
 * The rail's session comparator, a line-for-line mirror of the TUI sidebar
 * (ui/sidebar_model.go partitionByArchived, #1605): live rows first, then the
 * archived group last. The two groups order OPPOSITELY — live rows are oldest-
 * created first (the projection's stable order), while archived rows are NEWEST-
 * created first, so the archive reads as a most-recent-on-top history, the inverse
 * of the live tree (#1605). Title breaks a created_at tie in BOTH groups so the
 * order is total and never jitters. Shared by the sessions rail (ui.ts
 * orderedSessions) and the project switcher (project.ts) so the two surfaces can
 * never diverge on order (#1674 PR3 review: the web must sort archived desc like
 * the TUI, not asc).
 */
export function compareSessionsForRail(a: SessionData, b: SessionData): number {
  const aArchived = isArchived(a);
  const aa = aArchived ? 1 : 0;
  const bb = isArchived(b) ? 1 : 0;
  if (aa !== bb) {
    return aa - bb;
  }
  const at = a.created_at ?? "";
  const bt = b.created_at ?? "";
  if (at !== bt) {
    const asc = at < bt ? -1 : 1;
    // Live: oldest-first (asc). Archived: newest-first (desc), matching the TUI.
    return aArchived ? -asc : asc;
  }
  return a.title < b.title ? -1 : a.title > b.title ? 1 : 0;
}

/**
 * Builds the row title with the same prefixes the TUI prepends (render.go:304-345),
 * in the same precedence: [remote] outermost, then the state marker. Archived rows
 * deliberately carry NO word prefix (render.go:326-338) — the glyph + dimming say
 * it, and an 11-char prefix would eat the title cell.
 */
export function rowTitle(s: SessionData): string {
  const lv = livenessOf(s);
  const op = s.in_flight_op ?? InFlightOp.None;
  let title = s.title;
  if (op === InFlightOp.Killing || op === InFlightOp.Archiving) {
    title = "[deleting] " + title;
  } else if (lv === Liveness.Lost) {
    title = "[lost] " + title;
  } else if (lv === Liveness.LimitReached) {
    title = limitBadgePrefix(s) + title;
  }
  if (s.backend_type === "remote") {
    title = "[remote] " + title;
  }
  return title;
}

/** Mirrors ui/tree/render.go:limitBadgePrefix: "[limit] resets <t> " when a reset
 *  time is known, else a bare "[limit] ". */
function limitBadgePrefix(s: SessionData): string {
  if (!s.limit_reset_at) {
    return "[limit] ";
  }
  const reset = new Date(s.limit_reset_at);
  if (Number.isNaN(reset.getTime())) {
    return "[limit] ";
  }
  return `[limit] resets ${formatLimitReset(reset, new Date())} `;
}

/**
 * Mirrors ui/tree/render.go:formatLimitReset: a bare hour like "3pm" on the hour,
 * "3:04pm" otherwise, prefixed with the month/day ("Jul 6 3pm") when the reset is
 * not today, rendered in the viewer's local zone.
 */
function formatLimitReset(reset: Date, now: Date): string {
  const h12 = ((reset.getHours() + 11) % 12) + 1;
  const ampm = reset.getHours() < 12 ? "am" : "pm";
  const min = reset.getMinutes();
  const clock = min === 0 ? `${h12}${ampm}` : `${h12}:${String(min).padStart(2, "0")}${ampm}`;
  const sameDay =
    reset.getFullYear() === now.getFullYear() &&
    reset.getMonth() === now.getMonth() &&
    reset.getDate() === now.getDate();
  if (sameDay) {
    return clock;
  }
  const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  return `${months[reset.getMonth()]} ${reset.getDate()} ${clock}`;
}
