// The status-dot + title mapping for a sidebar row (#1592 Phase 5 PR3). It follows
// the TUI renderer, ui/tree/render.go, for how (Liveness, InFlightOp) become a state
// bucket and the [lost] / [deleting] / [limit] / [remote] / [model changed] title
// prefixes. The glyphs differ: the web rail has no state words, so an idle Ready
// row is blank and a working row draws a muted circle. Adding a Liveness value
// forces a deliberate choice here, mirroring the TUI's TOTAL liveness switch
// (render.go:274-301).
//
// Shapes keep the low-contrast/color-blindness discipline: a filled circle for
// Working, hollow for Dead, dashed for Lost, an archive for Archived, and a filled
// diamond for LimitReached (see styles.css .af-dot-*).

import {
  InFlightOp,
  Liveness,
  Status,
  type IdleReason,
  type PromptDeliveryStatus,
  type SessionData,
} from "./types.js";
import { formatDuration } from "./time.js";
import type { IconName } from "./icon.js";

/** The state bucket a row READS AS. Drives the .af-dot-<kind> CSS class. It is
 *  TOTAL over sessions, which makes it the key the status filter (filter.ts)
 *  partitions the rail by. */
export type DotKind = "ready" | "working" | "lost" | "dead" | "archived" | "limit";

export type RowKind = DotKind;

/** The operator-level vocabulary for scanning the rail. It deliberately groups the
 * daemon's finer mechanical reasons by the next action, while keeping Archived as a
 * separate inactive-history bucket outside the four live-work groups. */
export type OperatorKind = "attention" | "needs-you" | "working" | "waiting-limit" | "broken" | "archived";

/** Human labels shared by the rail rows and their filter. The visible words carry
 * the state independently of color, and stay sentence case per #3220. */
export const OPERATOR_KIND_LABELS: Record<OperatorKind, string> = {
  attention: "Needs attention",
  "needs-you": "Idle",
  working: "Working",
  "waiting-limit": "Waiting on a limit",
  broken: "Broken",
  archived: "Archived",
};

/** The one-word state labels, keyed by RowKind. Single source of truth: the row's
 *  own aria/title label (rowStatus) and the filter menu's checkbox labels both read
 *  this map, so the two surfaces cannot drift into calling the same state different
 *  things. Sentence case per the repo copy convention. */
export const ROW_KIND_LABELS: Record<RowKind, string> = {
  ready: "Ready",
  working: "Working",
  lost: "Lost",
  dead: "Dead",
  limit: "Limit reached",
  archived: "Archived",
};

/** A fully-resolved status descriptor for one row: the dot to draw and the
 *  human label (used for the row's aria/title so the state is legible to a
 *  screen reader, not only by color). */
export interface RowStatus {
  /** The icon to draw, or null for an idle Ready row, which shows no dot. */
  icon: IconName | null;
  kind: DotKind;
  /** Accessible one-word state label, e.g. "Ready", "Working", "Lost". */
  label: string;
}

const IDLE_REASON_LABELS: Record<IdleReason, string> = {
  "usage-limit": "usage limit",
  "process-exited": "process exited",
  "restore-gave-up": "restore gave up",
  "restore-waiting-on-environment": "restore waiting on environment",
  "recreate-pending": "recreate notice pending",
  "prompt-not-delivered": "prompt not delivered",
  "delivery-unconfirmed": "delivery unknown",
  "no-pane-change-since-delivery": "no change after delivery",
  "settled-after-pane-change": "pane changed",
};

/** Human row detail for the daemon's mechanical idle reason and last observed
 * pane churn. Unknown future values render nothing instead of being interpreted. */
export function idleReasonDetail(s: SessionData, now: Date = new Date()): string {
  const label = idleReasonLabel(s.idle_reason);
  if (!label) {
    return "";
  }
  let detail = label;
  if (s.idle_reason === "restore-gave-up" && s.lost_restore_failure) {
    const { attempts, error } = s.lost_restore_failure;
    if (Number.isInteger(attempts) && attempts > 0 && error.trim() !== "") {
      const noun = attempts === 1 ? "attempt" : "attempts";
      detail = `restore gave up after ${attempts} ${noun}: ${error}`;
    }
  }
  // Mirror of session.LostRestoreWait.Detail — the wait names the error that
  // armed it until a restore attempt or a structural failure retires it.
  if (s.idle_reason === "restore-waiting-on-environment" && s.lost_restore_wait) {
    const { attempts, error } = s.lost_restore_wait;
    if (Number.isInteger(attempts) && attempts > 0 && error.trim() !== "") {
      const noun = attempts === 1 ? "environmental failure" : "environmental failures";
      detail = `restore is waiting on the environment (${attempts} ${noun}): ${error}`;
    }
  }
  if (s.last_pane_churn_at) {
    const churn = new Date(s.last_pane_churn_at);
    if (!Number.isNaN(churn.getTime())) {
      const age = `${formatPaneChurnAge(churn, now)} ago`;
      detail += s.idle_reason === "settled-after-pane-change" ? ` · ${age}` : ` · pane changed ${age}`;
    }
  }
  return detail;
}

function idleReasonLabel(reason: IdleReason | undefined): string {
  return reason ? (IDLE_REASON_LABELS as Partial<Record<string, string>>)[reason] ?? "" : "";
}

/** Row detail for a live remote-probe outage (#5571 W1.3): static text, never
 *  an animated indicator, mirroring the TUI's "unreachable <age> · <err>"
 *  (render.go). It outranks the idle detail — the outage is why every other
 *  fact on the row may be stale. Returns "" when the pair is incomplete, so a
 *  malformed projection renders nothing rather than half a claim. */
export function unreachableDetail(s: SessionData, now: Date = new Date()): string {
  const u = s.unreachable;
  if (!u?.since || !u.error) {
    return "";
  }
  const since = new Date(u.since);
  if (Number.isNaN(since.getTime())) {
    return "";
  }
  return `unreachable ${formatPaneChurnAge(since, now)} ago · ${u.error}`;
}

function formatPaneChurnAge(churn: Date, now: Date): string {
  return formatDuration(now.getTime() - churn.getTime());
}

const DEAD_ICON: IconName = "circle";
const LOST_ICON: IconName = "circle-dashed";
const ARCHIVED_ICON: IconName = "archive";
const LIMIT_ICON: IconName = "diamond";

const WORKING: RowStatus = { icon: "circle", kind: "working", label: ROW_KIND_LABELS.working };

/**
 * Resolves a session's status dot from its two axes: any in-flight op is a
 * working/busy state; otherwise the liveness picks the dot. Falls back to the legacy `status` int only when `liveness`
 * is absent (a pre-#1195 record — never emitted by the daemon's live Snapshot, but
 * handled so a stray zero renders as working, matching render.go's LivenessUnset arm).
 */
export function rowStatus(s: SessionData): RowStatus {
  const op = s.in_flight_op ?? InFlightOp.None;
  // The [deleting] title prefix (rowTitle) distinguishes kill/archive.
  if (op !== InFlightOp.None) {
    return WORKING;
  }
  return dotForLiveness(livenessOf(s));
}

export function isWorking(s: SessionData): boolean {
  return rowStatus(s).kind === "working";
}

/** True while ANY client-visible op is in flight (create, kill, archive, restore,
 * replace, respawn). Every one of them either has no live pane yet or is about to
 * end the current one, so a row carrying one is not a place to open or stay bound
 * to. One predicate rather than a list, so a new op is fenced by default (#6104). */
export function hasInFlightOp(s: SessionData): boolean {
  return (s.in_flight_op ?? InFlightOp.None) !== InFlightOp.None;
}

/** True only for the daemon-owned create operation. Unlike isWorking (which also
 * includes a running agent and every other in-flight operation), this is used to
 * keep a not-yet-attachable row inert until its completed projection arrives. */
export function isCreating(s: SessionData): boolean {
  return (s.in_flight_op ?? InFlightOp.None) === InFlightOp.Creating;
}

/**
 * The state bucket a row READS AS, the key the rail's status filter partitions by
 * (filter.ts). Deliberately derived from rowStatus — the DISPLAYED status — and not
 * from the raw liveness, because the two genuinely differ: a session with an
 * in-flight op renders as Working whatever its liveness is. Filtering on
 * liveness would hide a row the user is looking at and reveal one they are not — the filter must
 * partition by what the eye sees, so it stays the single mapping in rowStatus.
 */
export function rowKind(s: SessionData): RowKind {
  return rowStatus(s).kind;
}

/** Groups liveness + idle_reason by what the operator should do next.
 *
 * In-flight work wins over stale evidence because rowStatus applies that daemon
 * overlay first. A positive non-delivery result is broken even though its process
 * is Ready; uncertainty remains Needs you rather than being mislabeled as failure.
 * Unknown future idle reasons fall back to the mechanically established liveness. */
export function operatorKind(s: SessionData): OperatorKind {
  const displayed = rowKind(s);
  if (hasAttention(s)) {
    return "attention";
  }
  if (displayed === "working") {
    return "working";
  }
  if (displayed === "archived") {
    return "archived";
  }
  if (displayed === "limit" || s.idle_reason === "usage-limit") {
    return "waiting-limit";
  }
  if (
    displayed === "lost" ||
    displayed === "dead" ||
    s.idle_reason === "process-exited" ||
    s.idle_reason === "prompt-not-delivered"
  ) {
    return "broken";
  }
  return "needs-you";
}

/** True when the agent asked for the user and the row is still live. The daemon
 * clears the request on Lost/Dead/Archived; this guard keeps a stale projection
 * from painting it over those glyphs. */
export function hasAttention(s: SessionData): boolean {
  if (!s.attention) {
    return false;
  }
  const kind = rowKind(s);
  return kind === "ready" || kind === "working";
}

/** The reason the agent gave with its attention request, trimmed, or "". */
export function attentionReason(s: SessionData): string {
  return hasAttention(s) ? (s.attention?.reason ?? "").trim() : "";
}

export type GlyphKind = DotKind | "attention";

/** The glyph a rail row draws. An idle Ready row is blank, except when its idle
 * reason makes it Broken or Waiting on a limit: the rail has no state words, so
 * the glyph must carry that. */
export function rowDot(s: SessionData): { icon: IconName; kind: GlyphKind } | null {
  if (hasAttention(s)) {
    return { icon: "circle", kind: "attention" };
  }
  const status = rowStatus(s);
  if (status.kind !== "ready") {
    return status.icon ? { icon: status.icon, kind: status.kind } : null;
  }
  switch (operatorKind(s)) {
    case "broken":
      return { icon: DEAD_ICON, kind: "dead" };
    case "waiting-limit":
      return { icon: LIMIT_ICON, kind: "limit" };
    default:
      return null;
  }
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
      return { icon: null, kind: "ready", label: ROW_KIND_LABELS.ready };
    case Liveness.Lost:
      return { icon: LOST_ICON, kind: "lost", label: ROW_KIND_LABELS.lost };
    case Liveness.Dead:
      return { icon: DEAD_ICON, kind: "dead", label: ROW_KIND_LABELS.dead };
    case Liveness.Archived:
      return { icon: ARCHIVED_ICON, kind: "archived", label: ROW_KIND_LABELS.archived };
    case Liveness.LimitReached:
      return { icon: LIMIT_ICON, kind: "limit", label: ROW_KIND_LABELS.limit };
    // LiveRunning and LivenessUnset both render as working (render.go:285, 297).
    case Liveness.Running:
    case Liveness.Unset:
    default:
      return WORKING;
  }
}

/** True when the row is an archived session (dimmed, no text prefix — the archive
 *  icon and dimming already convey it), mirroring render.go:335. */
export function isArchived(s: SessionData): boolean {
  return livenessOf(s) === Liveness.Archived;
}

/** True when the session is parked at a usage-limit wall — the state the diamond
 *  and the "[limit] resets …" title prefix already render.
 *
 *  It gates the Retry action (#1934), mirroring the TUI, which advertises `c` only
 *  when the selected session is actually limit-blocked (ui/menu.go) and refuses it
 *  with an explanatory error otherwise (app/handle_actions.go handleLimitRetry).
 *  Reading the liveness rather than the mere presence of `limit_reset_at` is the
 *  point: a session can be limit-blocked with no parsed reset time yet, and a
 *  resumed one keeps its stale reset timestamp in the projection.
 *
 *  An in-flight operation withholds it (#2997). A resume already running keeps the
 *  row LiveLimitReached by design — its fence preserves liveness — so liveness alone
 *  would advertise a Retry the daemon then refuses as busy.
 *
 *  This gates the action. It is NOT the whole story for display, and an earlier
 *  version of this note claimed otherwise: rowStatus above returns WORKING for any
 *  in-flight op, so a respawning row shows the working circle and isWorking() counts it as
 *  working (project.ts). Only the "[limit] resets …" prefix from rowTitle survives,
 *  because that arm keys on liveness. Consistent with every other op. */
export function isLimitReached(s: SessionData): boolean {
  return (
    livenessOf(s) === Liveness.LimitReached &&
    (s.in_flight_op ?? InFlightOp.None) === InFlightOp.None
  );
}

/** True when an operator must inspect the pane before explicitly retrying a
 * manual handoff mission whose first submission may already have landed. The
 * verdict is transaction-scoped; session-wide prompt evidence may describe a
 * later, unrelated delivery. */
export function isPendingManualHandoffDeliveryUnconfirmed(s: SessionData): boolean {
  const pending = s.pending_account_swap;
  const liveness = livenessOf(s);
  return (
    (s.in_flight_op ?? InFlightOp.None) === InFlightOp.None &&
    s.startup_state_unknown !== true &&
    s.user_killed !== true &&
    (liveness === Liveness.Running || liveness === Liveness.Ready) &&
    pending?.manual === true &&
    pending.replacement_panes_started === true &&
    pending.mission_delivery_status !== undefined &&
    pending.mission_delivery_status !== "not-delivered"
  );
}

/** True when a mission verdict leaves it unknown whether the mission landed.
 * The one list of those verdicts on this side; mirrors
 * session.ambiguousHandoffDelivery. */
function ambiguousHandoffDelivery(status: PromptDeliveryStatus | undefined): boolean {
  return status === "sent-unverified" || status === "could-not-confirm";
}

/** True when an operator may retire a pending mission on the attestation that
 * it already landed (#4429): any ambiguous verdict, plus a recorded delivery
 * whose settle a crash interrupted. Mirrors session.confirmableHandoffDelivery. */
function confirmableHandoffDelivery(status: PromptDeliveryStatus | undefined): boolean {
  return ambiguousHandoffDelivery(status) || status === "delivered";
}

/** True when an agent-only handoff has a known incoming pane whose mission
 * submission was ambiguous. The operator may inspect that pane and explicitly
 * override the replay fence; positive non-delivery stays owned by automation.
 *
 * A startup-unknown row DOES admit the explicit retry (#4429): the daemon
 * probes the pane first and restores the runtime binding only if it answers,
 * then runs readiness and the send; a pane that does not answer refuses the
 * retry. Dead liveness states still refuse: restore owns a runtime that no
 * longer exists. Mirrors session.Instance.CanRetryPendingHandoffMissionDelivery. */
export function isPendingAgentHandoffDeliveryUnconfirmed(s: SessionData): boolean {
  const liveness = livenessOf(s);
  const status = s.pending_handoff_delivery_status;
  const op = s.in_flight_op ?? InFlightOp.None;
  const dead =
    liveness === Liveness.Lost || liveness === Liveness.Dead || liveness === Liveness.Archived;
  return (
    s.pending_handoff_mission !== undefined &&
    s.pending_handoff_mission !== "" &&
    ambiguousHandoffDelivery(status) &&
    s.user_killed !== true &&
    !dead &&
    (liveness === Liveness.Running || liveness === Liveness.Ready || s.startup_state_unknown === true) &&
    (op === InFlightOp.None || op === InFlightOp.Replacing)
  );
}

/** True when the operator can attest the agent handoff's pending mission
 * already landed and retire it WITHOUT a resend (#4429) — the "mark delivered"
 * exit for an ambiguous verdict, and the supported way out of a startup-unknown
 * wedge. A delivered verdict is included: it is the crash window between a
 * confirmed send and its clearing settle. Not-delivered belongs to automatic
 * recovery; dead liveness belongs to restore. Mirrors
 * session.Instance.CanConfirmPendingHandoffDelivery. */
export function isPendingAgentHandoffDeliveryConfirmable(s: SessionData): boolean {
  const liveness = livenessOf(s);
  const status = s.pending_handoff_delivery_status;
  const op = s.in_flight_op ?? InFlightOp.None;
  const dead =
    liveness === Liveness.Lost || liveness === Liveness.Dead || liveness === Liveness.Archived;
  return (
    s.pending_handoff_mission !== undefined &&
    s.pending_handoff_mission !== "" &&
    confirmableHandoffDelivery(status) &&
    s.user_killed !== true &&
    !dead &&
    (op === InFlightOp.None || op === InFlightOp.Replacing) &&
    (s.startup_state_unknown === true ||
      liveness === Liveness.Running ||
      liveness === Liveness.Ready ||
      liveness === Liveness.LimitReached)
  );
}

/** True when a manual account swap's pending mission can be retired on the
 * operator's attestation that it already landed — the account-swap half of
 * #4429's confirm verb. Unlike the retry gate this admits startup-unknown and
 * an orphaned respawn fence: the daemon probes the pane before honoring the
 * attestation. Mirrors
 * session.Instance.CanConfirmPendingManualAccountSwapDelivery. */
export function isPendingManualSwapDeliveryConfirmable(s: SessionData): boolean {
  const pending = s.pending_account_swap;
  const liveness = livenessOf(s);
  const op = s.in_flight_op ?? InFlightOp.None;
  const dead =
    liveness === Liveness.Lost || liveness === Liveness.Dead || liveness === Liveness.Archived;
  const status = pending?.mission_delivery_status;
  return (
    pending?.manual === true &&
    pending.replacement_panes_started === true &&
    confirmableHandoffDelivery(status) &&
    s.user_killed !== true &&
    !dead &&
    (op === InFlightOp.None || op === InFlightOp.Respawning) &&
    (s.startup_state_unknown === true ||
      liveness === Liveness.Running ||
      liveness === Liveness.Ready ||
      liveness === Liveness.LimitReached)
  );
}

/** True when this session's agent can be handed off to a different one in place
 *  (#2013) — the state that gates the Handoff action, mirroring the TUI, which
 *  advertises `F` only for a handoff-capable selection (app/handle_handoff.go) and
 *  refuses it with an explanatory error otherwise.
 *
 *  Reads the daemon-projected `can_handoff` decision (a local-worktree session in a
 *  runtime state that admits the swap) rather than re-deriving it from
 *  backend_type/liveness here: a second copy of the handoff rule in the browser is
 *  how the picker's "who can be replaced" drifts from the daemon's same-agent guard.
 *  Fails closed — `=== true`, so an older daemon that omits the field offers nothing. */
export function canHandoff(s: SessionData): boolean {
  return s.can_handoff === true;
}


/**
 * Whether a session is the reserved root agent (#2513). The web CONSUMES the
 * daemon-projected `is_root` decision (InstanceData.IsRoot — the daemon's own
 * session.IsReservedTitle, scrubbed by ForStorage) rather than re-deriving the
 * reserved-title rule in the browser, the same "consume the decision, don't
 * re-implement it" discipline as can_kill/can_handoff/lifecycle_action. Absence or
 * false fails closed (treated as not root).
 */
export function isRootSession(s: SessionData): boolean {
  return s.is_root === true;
}

/**
 * The rail's session comparator, a line-for-line mirror of the TUI sidebar
 * (ui/sidebar_model.go partitionByArchived, #1605; ui/store/order.go
 * LessInstanceOrder, #1144): the reserved root agent pins to the top, then live
 * rows, then the archived group last. The two dated groups order OPPOSITELY —
 * live rows are oldest-created first (the projection's stable order), while
 * archived rows are NEWEST-created first, so the archive reads as a
 * most-recent-on-top history, the inverse of the live tree (#1605). Title breaks
 * a created_at tie in BOTH groups so the order is total and never jitters. Shared
 * by the sessions rail (ui.ts orderedSessions) and the project switcher
 * (project.ts) so the two surfaces can never diverge on order (#1674 PR3 review:
 * the web must sort archived desc like the TUI, not asc).
 */
export function compareSessionsForRail(a: SessionData, b: SessionData): number {
  const aArchived = isArchived(a);
  const aa = aArchived ? 1 : 0;
  const bb = isArchived(b) ? 1 : 0;
  if (aa !== bb) {
    return aa - bb;
  }
  // Root pins to the top of its group by IDENTITY, before created_at — mirroring
  // the TUI's LessInstanceOrder (a root re-ensure with a fresh created_at after a
  // tmux death still leads). Root is live in practice, so this leaves it first of
  // the live rows (#1144/#2513).
  const ar = isRootSession(a) ? 0 : 1;
  const br = isRootSession(b) ? 0 : 1;
  if (ar !== br) {
    return ar - br;
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

/** The backend types whose workspace lives off-box (session/archive_sandbox.go
 *  backendKindForType: docker, ssh, "sandbox", and "remote" — the hook runtime).
 *  None of them can service tab management: every Add*Tab path needs a
 *  daemon-side git worktree they do not have, so the daemon's
 *  Capabilities().TabManagement is false for all four (#1874).
 *
 *  This list is the ONE place the web names backend types. It exists only because
 *  the session envelope carries `backend_type` but not the daemon's capability
 *  bits — the honest fix is to serialize the capability so no client re-derives
 *  it (see #1874). Until then, a NEW off-box runtime must be added here, which is
 *  what ui.test.ts pins. */
const OFF_BOX_BACKENDS = new Set(["docker", "ssh", "sandbox", "remote"]);

/** The existing backend-type fallback for off-box workspace ownership. */
export function isOffBoxWorkspace(s: Pick<SessionData, "backend_type">): boolean {
  return OFF_BOX_BACKENDS.has(s.backend_type ?? "local");
}

/**
 * Builds the row title with the same prefixes the TUI prepends (render.go:400-465),
 * in the same precedence for ordinary status prefixes, with the orthogonal model
 * diagnostic outermost so narrow rows cannot hide it. Archived rows carry NO
 * word prefix (render.go:423-434) — the glyph + dimming say it, and an 11-char
 * prefix would eat the title cell.
 */
export function rowTitle(s: SessionData): string {
  const lv = livenessOf(s);
  const op = s.in_flight_op ?? InFlightOp.None;
  let title = s.title;
  if (isOffBoxWorkspace(s)) {
    title = "[remote] " + title;
  }
  if (lv === Liveness.Lost) {
    title = "[lost] " + title;
  }
  if (op === InFlightOp.Killing || op === InFlightOp.Archiving) {
    title = "[deleting] " + title;
  }
  if (lv === Liveness.LimitReached) {
    title = limitBadgePrefix(s) + title;
  }
  const recreate = rootRecreateNote(s);
  if (recreate) {
    title = `[${recreate}] ` + title;
  }
  if (archiveWarningText(s) !== "") {
    title = "[archive incomplete] " + title;
  }
  // Mirroring render.go: the outage marker is static text and sits just inside
  // [model changed]; only a complete since/error pair earns it.
  if (s.unreachable?.since && s.unreachable?.error) {
    title = "[unreachable] " + title;
  }
  if (s.model_change) {
    title = "[model changed] " + title;
  }
  return title;
}

/** The daemon-projected, bounded incomplete-archive notice. Keeping the trim in
 * one helper means the rail badge and the persistent pane banner cannot disagree
 * about whether a warning exists. */
export function archiveWarningText(s: SessionData): string {
  return s.archive_warning?.trim() ?? "";
}

/** The cross-surface phrase every surface renders before the reason a session
 *  has no af callback (#5571 W3.1) — kept as one exported constant so the rail
 *  banner, the row tooltip and the backend picker can never spell it three ways.
 *  Mirrors session.CallbackUnavailablePrefix. */
export const CALLBACK_UNAVAILABLE_PREFIX = "af callback unavailable: ";

/** The daemon-projected callback-absence reason, rendered under the shared
 *  prefix. "" means callback was delivered (or the backend injects none). */
export function callbackUnavailableText(s: SessionData): string {
  const reason = s.callback_unavailable?.trim() ?? "";
  return reason === "" ? "" : CALLBACK_UNAVAILABLE_PREFIX + reason;
}

/** Every session-level notice the persistent pane banner carries — the
 *  incomplete-archive warning and the callback-absence advisory — joined so a
 *  row carrying both cannot drop either. "" means there is nothing to show. */
export function sessionWarningText(s: SessionData): string {
  return [archiveWarningText(s), callbackUnavailableText(s)]
    .filter((t) => t !== "")
    .join(" · ");
}

/** Mirrors session.RootRecreateContext.Note (session/root_recreate.go): the short
 *  note a re-created root carries when it did not demonstrably come back on its
 *  prior conversation (#2629). The switch is exhaustive over the values af writes
 *  and returns "" for anything else, so a value a newer daemon adds renders no
 *  note rather than a guessed one. */
export function rootRecreateNote(s: SessionData): string {
  switch (s.root_recreate_context) {
    case "fresh":
      return "fresh context";
    case "unknown":
      return "context unknown";
    default:
      return "";
  }
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

/** One word for a narrow rail row: the host's name, else the runtime type; empty
 *  for a local session or a daemon that projects no placement. Mirrors Go
 *  PlacementData.Short. */
export function placementShort(s: SessionData): string {
  const p = s.placement;
  if (!p || p.kind !== "remote") return "";
  return p.host || p.backend || "";
}

/** The full one-line place for a detail view. Mirrors Go PlacementData.Detail. */
export function placementDetail(s: SessionData): string {
  const p = s.placement;
  if (!p) return "";
  const parts: string[] = [];
  if (p.kind === "remote") {
    parts.push(placementShort(s));
    if (p.host && p.backend) parts.push(p.backend);
  } else {
    parts.push("local");
  }
  if (p.location) parts.push(p.location);
  return parts.filter(Boolean).join(" · ");
}
