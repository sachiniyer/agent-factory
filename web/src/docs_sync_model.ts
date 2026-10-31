// The sync model of one open doc in the Docs tab: a pure reducer. `step` takes the
// current Model and an Event and returns the next Model and the effects the shell
// (docs_sync.ts) must run. There is no I/O, timer or DOM here, so every
// state-and-event pair can be tested, including by random interleavings
// (docs_sync_model.test.ts).
//
// The design and its full transition table are in the #6292 thread. In short: a
// document is in one `phase`; a save in flight, a reload, a reconcile and a conflict
// are part of the state, not side flags; every request carries an id so a late,
// duplicate or out-of-order reply is ignored; a remote change that cannot be applied
// yet is remembered in `stale` and `settle` drains it whenever the document is idle
// and clean; a save whose outcome is unknown (timeout, network error, 5xx) is
// reconciled against the daemon's head before anything else is sent or loaded; and
// unsaved work is mirrored into per-window storage so closing the page cannot lose it.

import type { DocText, SaveResult } from "./proposal_api.js";
import { hasConflictMarkers, mergeWithMarkers } from "./text_diff.js";

export const AUTOSAVE_DELAY_MS = 500;
export const SAVE_RETRY_MS = 5000;
/** A save that timed out and is still unanswered this long after is treated as ended: the
 *  browser's own request timeout is about as long, and a doubt must not outlive its tab. */
export const SAVE_GIVE_UP_MS = 5 * 60_000;
/** How long to wait before the next attempt after `fails` saves in a row went unanswered. */
export function retryDelay(fails: number): number {
  return Math.min(SAVE_RETRY_MS * 2 ** Math.max(0, fails - 1), 60_000);
}

export type SyncStatus =
  | "loading"
  | "saved"
  | "pending"
  | "saving"
  | "merged"
  | "conflict"
  | "markers"
  | "recovered"
  | "gone"
  | "error";

export interface Conflict {
  ours: string;
  theirs: string;
  base: string;
  /** The commit that `theirs` is at; "" when the daemon did not say. */
  head: string;
}

/** "restoring" sits between the first load and "idle": the load has returned and
 *  per-window storage is being read. Nothing is drained, armed or stored in it, so
 *  a stored draft is always restored before any reload can start. "reconciling" is
 *  the read of the daemon's head after a save whose outcome is unknown. */
export type Phase = "loading" | "restoring" | "idle" | "saving" | "reloading" | "reconciling" | "conflict";

export interface Flight {
  seq: number;
  text: string;
  base: string;
  /** The acknowledged text `base` names: what the save's edits were made against. */
  baseText: string;
  final: boolean;
}

export interface Model {
  phase: Phase;
  /** The first load finished. */
  loaded: boolean;
  /** What the user sees and has typed. */
  buffer: string;
  /** The text at `base`, as far as this tab knows. */
  saved: string;
  base: string;
  /** The next request id. Replies carry the id of their request. */
  nextSeq: number;
  /** Set exactly when phase is "saving". */
  flight: Flight | null;
  /** Set exactly when phase is "conflict". */
  conflict: Conflict | null;
  /** The buffer when the reload started; set exactly when phase is "reloading". */
  reloadFrom: string | null;
  /** The id of the load in flight in phase "reloading" or "reconciling". */
  readSeq: number | null;
  /** The daemon may be ahead of `base`. Drained by `settle`. */
  stale: boolean;
  /** A read failed; no new one until a timer fires. */
  backoff: boolean;
  /** Saves in a row whose outcome was unknown or failed; the next attempt waits longer for each. */
  fails: number;
  /** A save's outcome is unknown: it may or may not have landed. Nothing is sent
   *  or reloaded until the daemon's head has been read (phase "reconciling"). */
  unknown: boolean;
  /** Texts of saves whose outcome is unknown. */
  inDoubt: string[];
  /** Saves that timed out and have not been answered: they can still land. Their texts
   *  stay in `inDoubt` until they have completed and the daemon has been read again. */
  late: { seq: number; text: string; baseText: string; ended: number | null }[];
  /** The daemon refused a save for good (too large, rejected): no retry until the
   *  buffer changes. */
  fatal: boolean;
  /** The doc was deleted under this tab. Nothing is sent or read; the unsaved work
   *  stays in storage until the user restores the doc or discards it. */
  gone: boolean;
  /** A final save is wanted once the current one has replied. */
  leaving: boolean;
  /** The tab is gone: no timers, no reloads, only the final save. */
  closed: boolean;
  /** The doc is being deleted by this window: no timers, saves or reads; the draft stays stored. */
  suspended: boolean;
  /** The sync is finished: every event is ignored. */
  dead: boolean;
  timer: "save" | "retry" | null;
  note: { kind: "merged" | "error" | "recovered"; detail: string } | null;
  /** What per-window storage currently holds for this doc (a JSON key), or null. */
  stashed: string | null;
  loadFailed: string | null;
}

export type Event =
  | { t: "load_ok"; doc: DocText }
  /** What per-window storage holds for this doc, or null. */
  | { t: "draft_read"; draft: Draft | null }
  | { t: "load_fail"; error: string }
  | { t: "edit"; text: string }
  | { t: "timer" }
  | { t: "save_ok"; seq: number; result: Extract<SaveResult, { kind: "saved" }> }
  | { t: "save_conflict"; seq: number; result: Extract<SaveResult, { kind: "conflict" }> }
  /** The outcome is unknown: a timeout, a network error, a 5xx. */
  /** `outstanding`: the request timed out unanswered, so it can still land. */
  | { t: "save_fail"; seq: number; error: string; outstanding?: boolean }
  /** A save that timed out has finally been answered (or has failed): it can land no more. */
  | { t: "late_done"; seq: number }
  /** The daemon refused the save outright (4xx): it certainly did not apply. */
  | { t: "save_fatal"; seq: number; error: string }
  | { t: "remote"; commit: string }
  | { t: "reload_ok"; seq: number; doc: DocText }
  | { t: "reload_fail"; seq: number; error: string }
  | { t: "reconcile_ok"; seq: number; doc: DocText }
  | { t: "reconcile_fail"; seq: number; error: string }
  | { t: "reconnect" }
  /** The doc no longer exists on the daemon (a 404, or the doc list says so). */
  /** The doc is gone: told by the doc list (no request id), or by a 404 to the read or
   *  save with this id, which counts only while that request is still the active one. */
  | { t: "gone"; of?: "read" | "save"; seq?: number }
  /** The user restored a deleted doc: `head` is the new doc's state. */
  | { t: "revive"; head: DocText }
  /** The doc is being deleted by this window: stop retrying and arming; the draft stays stored. */
  | { t: "suspend" }
  /** The delete landed (or was given up on): the sync is finished. `keepDraft`
   *  leaves the stored draft for the next tab to restore. */
  | { t: "abandon"; keepDraft: boolean }
  | { t: "flush" }
  | { t: "leave" }
  | { t: "close" }
  | { t: "keep_mine"; head: DocText }
  | { t: "take_theirs"; head: DocText }
  | { t: "show_both"; head: DocText };

export type Effect =
  | { t: "load"; kind: "initial" | "reload" | "reconcile"; seq: number }
  | { t: "save"; seq: number; text: string; base: string; final: boolean }
  | { t: "set_buffer"; text: string }
  | { t: "arm"; kind: "save" | "retry"; ms: number }
  | { t: "disarm" }
  /** Keeps unsaved work in per-window storage, so it survives closing the tab or
   *  the page. The proposal doc never receives a conflict draft. */
  | { t: "stash"; draft: Draft }
  | { t: "unstash" }
  /** The requests of an adopted draft that may still be out: after `ms`, treat them as ended. */
  | { t: "give_up"; seqs: number[]; ms: number };

/** Unsaved work, as stored for this window: an unresolved conflict's draft, or
 *  edits no save has landed yet. */
export interface Draft {
  buffer: string;
  /** The text the user's edits were made against. */
  base: string;
  /** The agent's side when the conflict arose; "" for unsent edits. */
  theirs: string;
  /** The commit `theirs` is at; "" for unsent edits. */
  head: string;
  /** Absent means a conflict draft (the only kind stored before this field). */
  kind?: "conflict" | "unsent";
  /** Texts a save of ours may have left on the daemon: the user's edit may be to
   *  revert one of them, which a merge against `base` cannot see. */
  maybe?: string[];
  /** For each of `maybe`, the acknowledged text its save was made against (`base` when
   *  absent): the head may have moved since, and a merge needs the right base. */
  maybeBases?: string[];
}

export function initialModel(): Model {
  return {
    phase: "loading",
    loaded: false,
    buffer: "",
    saved: "",
    base: "",
    nextSeq: 1,
    flight: null,
    conflict: null,
    reloadFrom: null,
    readSeq: null,
    stale: false,
    backoff: false,
    fails: 0,
    unknown: false,
    inDoubt: [],
    late: [],
    fatal: false,
    gone: false,
    leaving: false,
    closed: false,
    suspended: false,
    dead: false,
    timer: null,
    note: null,
    stashed: null,
    loadFailed: null,
  };
}

export function isDirty(m: Model): boolean {
  return m.buffer !== m.saved;
}

export function statusOf(m: Model): { status: SyncStatus; detail: string } {
  if (!m.loaded) {
    return m.loadFailed !== null ? { status: "error", detail: m.loadFailed } : { status: "loading", detail: "" };
  }
  if (m.gone) return { status: "gone", detail: "" };
  if (m.phase === "conflict") return { status: "conflict", detail: "" };
  if (m.phase === "saving") return { status: "saving", detail: "" };
  if (m.note?.kind === "error") return { status: "error", detail: m.note.detail };
  if (hasConflictMarkers(m.buffer)) return { status: "markers", detail: "" };
  if (isDirty(m) && m.note?.kind === "recovered") return { status: "recovered", detail: "" };
  if (isDirty(m)) return { status: "pending", detail: "" };
  if (m.note?.kind === "merged") return { status: "merged", detail: "" };
  return { status: "saved", detail: "" };
}

/** Returns the next model and the effects to run. The input is not modified. */
export function step(prev: Model, e: Event): [Model, Effect[]] {
  if (prev.dead) return [prev, []];
  const m: Model = { ...prev, inDoubt: [...prev.inDoubt], late: [...prev.late] };
  const fx: Effect[] = [];
  reduce(m, e, fx);
  settle(m, fx);
  return [m, fx];
}

function disarm(m: Model, fx: Effect[]): void {
  if (m.timer !== null) {
    m.timer = null;
    fx.push({ t: "disarm" });
  }
}

function arm(m: Model, fx: Effect[], kind: "save" | "retry", ms: number): void {
  disarm(m, fx);
  m.timer = kind;
  fx.push({ t: "arm", kind, ms });
}

/** Whether a save may go out now. */
function canSave(m: Model): boolean {
  return m.phase === "idle" && m.loaded && !m.suspended && !m.gone && !m.unknown && !m.fatal;
}

/** The tab opened with a draft in per-window storage: an unresolved conflict from
 *  an earlier tab, or edits whose save never landed. Applied inside draft_read, before
 *  settle can start a reload that would otherwise let the storage mirror drop it. */
function restoreDraft(m: Model, fx: Effect[], draft: Draft): void {
  m.stashed = draftKey(draft);
  if (draft.buffer === m.saved) {
    // Nothing differs from the head, but a save of the closed pane that stored this draft
    // may still be out (the request outlives its pane): its text stays in doubt, here, until
    // it has ended or a change event shows it landed. Clearing the draft now would lose an undo.
    const doubts = draft.kind === "unsent" ? (draft.maybe ?? []).map((text, i) => ({ text, baseText: draft.maybeBases?.[i] ?? draft.base })).filter((d) => d.text !== m.saved) : [];
    if (doubts.length === 0) return;
    const seqs: number[] = [];
    for (const d of doubts) {
      const seq = m.nextSeq++;
      seqs.push(seq);
      m.late = [...m.late, { seq, text: d.text, baseText: d.baseText, ended: null }];
    }
    m.inDoubt = [...m.inDoubt, ...doubts.map((d) => d.text)];
    fx.push({ t: "give_up", seqs, ms: SAVE_GIVE_UP_MS });
    return;
  }
  const unsent = draft.kind === "unsent";
  // If the daemon holds a text one of our saves may have left there, the edits were
  // made against that text (an undo back to the old one is only visible that way).
  // A save of ours landed if the head already contains its edits (merging them onto the
  // head changes nothing), even when the agent edited other lines since; the edits then
  // rebase from that text, which is how an undo back to the old one survives.
  const landed = unsent
    ? [...(draft.maybe ?? [])]
        .map((t, i) => ({ t, from: draft.maybeBases?.[i] ?? draft.base }))
        .reverse()
        .find((d) => d.t !== d.from && mergeWithMarkers(d.from, d.t, m.saved) === m.saved)?.t
    : undefined;
  const base = unsent ? (landed ?? draft.base) : draft.base;
  const rebased = unsent ? mergeWithMarkers(base, draft.buffer, m.saved) : draft.buffer;
  const clean = unsent && !hasConflictMarkers(rebased);
  m.buffer = clean ? rebased : draft.buffer;
  fx.push({ t: "set_buffer", text: m.buffer });
  if (clean) {
    // Edits that no save landed, rebased onto what the daemon holds now: they go
    // out again, and the status says they were recovered.
    m.note = { kind: "recovered", detail: "" };
    return;
  }
  enterConflict(m, fx, { ours: draft.buffer, theirs: m.saved, base, head: m.base });
}

function draftKey(d: Draft): string {
  return JSON.stringify(d);
}

/** What per-window storage must hold: the conflict draft while a conflict is open,
 *  else any edits not yet saved (or whose save may have landed), else nothing. It is
 *  written before a save is sent and cleared once the daemon's state is known, so
 *  closing the page, or a save that fails, never loses typing. */
function wantedDraft(m: Model): Draft | null {
  if (m.phase === "conflict" && m.conflict) {
    return { buffer: m.buffer, base: m.conflict.base, theirs: m.conflict.theirs, head: m.conflict.head, kind: "conflict" };
  }
  // A save of text other than what is acknowledged may have landed, or may yet
  // land, so a buffer that equals the acknowledged text (the user undid back to
  // it) is still unsaved work as far as the daemon is concerned.
  const pairs = [
    ...(m.flight ? [{ text: m.flight.text, from: m.flight.baseText }] : []),
    ...m.inDoubt.map((text) => ({ text, from: m.late.find((l) => l.text === text)?.baseText ?? m.saved })),
  ].filter((p) => p.text !== m.saved);
  if (isDirty(m) || pairs.length > 0) {
    const d: Draft = { buffer: m.buffer, base: m.saved, theirs: "", head: "", kind: "unsent" };
    if (pairs.length > 0) {
      const byText = new Map(pairs.map((p) => [p.text, p.from]));
      d.maybe = [...byText.keys()];
      d.maybeBases = [...byText.values()];
    }
    return d;
  }
  return null;
}

function syncStash(m: Model, fx: Effect[]): void {
  if (!m.loaded) return;
  const want = wantedDraft(m);
  const key = want ? draftKey(want) : null;
  if (key === m.stashed) return;
  m.stashed = key;
  fx.push(want ? { t: "stash", draft: want } : { t: "unstash" });
}

function enterConflict(m: Model, fx: Effect[], conflict: Conflict): void {
  m.phase = "conflict";
  m.conflict = conflict;
  m.leaving = false;
  disarm(m, fx);
}

function startSave(m: Model, fx: Effect[], final: boolean): void {
  disarm(m, fx);
  const seq = m.nextSeq++;
  m.phase = "saving";
  m.flight = { seq, text: m.buffer, base: m.base, baseText: m.saved, final };
  m.note = null;
  fx.push({ t: "save", seq, text: m.buffer, base: m.base, final });
}

function startRead(m: Model, fx: Effect[], kind: "reload" | "reconcile"): void {
  disarm(m, fx);
  const seq = m.nextSeq++;
  m.phase = kind === "reload" ? "reloading" : "reconciling";
  m.reloadFrom = kind === "reload" ? m.buffer : null;
  m.readSeq = seq;
  m.stale = false;
  fx.push({ t: "load", kind, seq });
}

/** Takes the daemon's text as the document, for a reload or "take theirs". */
function adopt(m: Model, fx: Effect[], doc: DocText): void {
  m.base = doc.commit;
  m.saved = doc.text;
  if (doc.text !== m.buffer) {
    m.buffer = doc.text;
    fx.push({ t: "set_buffer", text: doc.text });
  }
}

/** The doc was deleted under this tab. A clean doc simply ends; one with unsaved
 *  work or an open conflict keeps it, in storage, for the user to restore. */
function goneEvent(m: Model, fx: Effect[]): void {
  if (!m.loaded) {
    m.dead = true;
    return;
  }
  const hasWork = isDirty(m) || m.phase === "conflict" || m.inDoubt.length > 0 || m.flight !== null;
  if (!hasWork) {
    m.dead = true;
    m.closed = true;
    disarm(m, fx);
    if (m.stashed !== null) {
      m.stashed = null;
      fx.push({ t: "unstash" });
    }
    return;
  }
  m.gone = true;
  // The save that was out may have landed, and the buffer may have moved off it (an
  // undo): the work that stays with the deleted doc includes that, so the draft keeps it.
  if (m.flight && m.flight.text !== m.saved) m.inDoubt = [...m.inDoubt, m.flight.text];
  m.flight = null;
  if (m.phase === "saving" || m.phase === "reloading" || m.phase === "reconciling") m.phase = "idle";
  m.readSeq = null;
  m.reloadFrom = null;
  m.leaving = false;
  disarm(m, fx);
}

function reduce(m: Model, e: Event, fx: Effect[]): void {
  switch (e.t) {
    case "load_ok":
      if (m.phase !== "loading") return;
      m.phase = "restoring";
      m.loadFailed = null;
      m.buffer = e.doc.text;
      m.saved = e.doc.text;
      m.base = e.doc.commit;
      fx.push({ t: "set_buffer", text: e.doc.text });
      return;

    case "draft_read":
      if (m.phase !== "restoring") return;
      m.phase = "idle";
      m.loaded = true;
      if (e.draft) restoreDraft(m, fx, e.draft);
      return;

    case "load_fail":
      if (m.phase === "loading") m.loadFailed = e.error;
      return;

    case "edit":
      // Before the first load the editor is read-only, so there is nothing to keep.
      if (!m.loaded || m.closed || e.text === m.buffer) return;
      m.buffer = e.text;
      m.fatal = false;
      if (m.note?.kind !== "recovered") m.note = null;
      if (m.phase === "idle" && !m.gone && !m.unknown) {
        // Debounce: autosave runs a pause after the LAST keystroke.
        if (isDirty(m) && !hasConflictMarkers(m.buffer)) arm(m, fx, "save", AUTOSAVE_DELAY_MS);
        else if (m.timer === "save") disarm(m, fx);
      }
      return;

    case "timer": {
      const kind = m.timer;
      m.timer = null;
      m.backoff = false;
      if (kind === "save" && canSave(m) && isDirty(m)) {
        // A closed tab keeps retrying while the page lives.
        if (m.closed) startSave(m, fx, true);
        else if (!hasConflictMarkers(m.buffer)) startSave(m, fx, false);
      }
      return;
    }

    case "flush":
      if (canSave(m) && !m.closed && isDirty(m) && !hasConflictMarkers(m.buffer)) startSave(m, fx, false);
      return;

    case "save_ok": {
      if (m.phase !== "saving" || !m.flight || e.seq !== m.flight.seq) return;
      const sent = m.flight.text;
      const r = e.result;
      // Saves of ours that were in doubt (timed out, lost reply): the daemon's merge may have
      // brought one of them into this reply, undoing what the user has reverted since.
      const doubts = m.inDoubt.map((text) => ({ text, from: m.late.find((l) => l.text === text)?.baseText ?? m.flight!.baseText }));
      m.flight = null;
      m.phase = "idle";
      m.note = null;
      m.fails = 0;
      m.inDoubt = m.late.map((l) => l.text);
      m.unknown = false;
      const mergedAgentEdits = r.merged && r.text !== sent;
      const landed = mergedAgentEdits
        ? doubts.find((d) => d.text !== d.from && d.text !== sent && mergeWithMarkers(d.from, d.text, r.text) === r.text)
        : undefined;
      if (landed) {
        // Rebase what the user holds now onto the merged text from the doubted save, not from
        // what was sent: their revert of that save's edit is a real edit, and stays.
        const kept = mergeWithMarkers(landed.text, m.buffer, r.text);
        if (hasConflictMarkers(kept) || hasConflictMarkers(m.buffer)) {
          enterConflict(m, fx, { ours: m.buffer, theirs: r.text, base: landed.text, head: r.commit });
          return;
        }
        m.base = r.commit;
        m.saved = r.text;
        if (kept !== m.buffer) {
          m.buffer = kept;
          fx.push({ t: "set_buffer", text: kept });
        }
        m.note = { kind: "merged", detail: "" };
        return;
      }
      if (!mergedAgentEdits) {
        // The commit holds exactly what was sent, so it is the new base; typing
        // since then applies cleanly on top of it.
        m.base = r.commit;
        m.saved = sent;
        return;
      }
      if (m.buffer === sent) {
        m.base = r.commit;
        m.saved = r.text;
        m.buffer = r.text;
        fx.push({ t: "set_buffer", text: r.text });
        m.note = { kind: "merged", detail: "" };
        return;
      }
      // The daemon merged the agent's edits into `sent`; the user kept typing.
      // Rebase the typing since `sent` onto the merged text and move the base to
      // the commit, so the next save does not conflict with our own commit.
      const rebased = mergeWithMarkers(sent, m.buffer, r.text);
      if (hasConflictMarkers(rebased) || hasConflictMarkers(m.buffer)) {
        enterConflict(m, fx, { ours: m.buffer, theirs: r.text, base: sent, head: r.commit });
        return;
      }
      m.base = r.commit;
      m.saved = r.text;
      m.buffer = rebased;
      fx.push({ t: "set_buffer", text: rebased });
      m.note = { kind: "merged", detail: "" };
      return;
    }

    case "save_conflict": {
      if (m.phase !== "saving" || !m.flight || e.seq !== m.flight.seq) return;
      m.flight = null;
      m.fails = 0;
      m.inDoubt = m.late.map((l) => l.text);
      m.unknown = false;
      const r = e.result;
      if (m.buffer === r.theirs) {
        // Both sides hold the same text: there is nothing to choose between.
        m.phase = "idle";
        m.base = r.head;
        m.saved = r.theirs;
        return;
      }
      // Even when the tab is leaving, the draft goes to per-window storage, not
      // into the proposal doc.
      enterConflict(m, fx, { ours: r.ours, theirs: r.theirs, base: r.base, head: r.head });
      return;
    }

    case "save_fail": {
      if (m.phase !== "saving" || !m.flight || e.seq !== m.flight.seq) return;
      // The outcome is unknown, not "failed": the save may have landed. Remember its
      // text, and read the daemon's head (settle) before sending or loading anything.
      m.fails++;
      m.inDoubt = [...m.inDoubt, m.flight.text];
      if (e.outstanding) m.late = [...m.late, { seq: e.seq, text: m.flight.text, baseText: m.flight.baseText, ended: null }];
      m.flight = null;
      m.phase = "idle";
      m.unknown = true;
      m.backoff = false;
      m.note = { kind: "error", detail: e.error };
      return;
    }

    case "late_done": {
      if (m.dead || !m.late.some((l) => l.seq === e.seq && l.ended === null)) return;
      if (m.closed) {
        // A closed tab handed the doc over when it went quiet (its stored draft, with the
        // text in doubt, is the next pane's to restore and reconcile): it must not read
        // and send a corrective save of its own over what that pane has done since.
        // Its draft keeps the text in doubt (the next pane reconciles it against the head
        // when it restores), so this tab does not clear it: it may be the only copy of an undo.
        m.late = m.late.map((l) => (l.seq === e.seq ? { ...l, ended: m.nextSeq } : l));
        return;
      }
      // The request has ended, and may have landed after the read that found the head
      // unchanged: read the head again, with its text (and what it was made against)
      // still in doubt. A read issued from now on (a request id above `nextSeq`) settles it.
      m.late = m.late.map((l) => (l.seq === e.seq ? { ...l, ended: m.nextSeq } : l));
      m.unknown = true;
      return;
    }

    case "save_fatal":
      if (m.phase !== "saving" || !m.flight || e.seq !== m.flight.seq) return;
      m.flight = null;
      m.phase = "idle";
      m.fatal = true;
      m.note = { kind: "error", detail: e.error };
      return;

    case "remote":
      if (m.closed || m.gone) return;
      if (e.commit !== "" && e.commit === m.base && m.phase !== "loading" && m.phase !== "restoring") return;
      m.stale = true;
      // A save of ours may still be out and may be what changed: read the head with its text
      // in doubt (reconcile), not a plain reload that would take the daemon's text over ours.
      if (m.late.some((l) => l.ended === null)) m.unknown = true;
      return;

    case "gone":
      if (e.seq !== undefined && e.seq !== (e.of === "save" ? m.flight?.seq : m.readSeq)) return; // a late 404 of a request nobody is waiting on
      goneEvent(m, fx);
      return;

    case "revive":
      // A tab closed while its doc was gone has handed the kept work to the next pane (through its
      // stored draft): a restore that completes after the close must not bring it back to write.
      if (!m.gone || m.closed) return;
      m.gone = false;
      m.suspended = false;
      m.conflict = null;
      if (m.phase === "conflict") m.phase = "idle";
      m.unknown = false;
      m.inDoubt = [];
      m.late = [];
      m.fails = 0;
      m.fatal = false;
      m.base = e.head.commit;
      m.saved = e.head.text;
      m.note = null;
      return;

    case "suspend":
      m.suspended = true;
      m.leaving = false;
      disarm(m, fx);
      return;

    case "abandon":
      m.dead = true;
      m.suspended = true;
      m.closed = true;
      m.leaving = false;
      disarm(m, fx);
      if (!e.keepDraft && m.stashed !== null) {
        m.stashed = null;
        fx.push({ t: "unstash" });
      }
      return;

    case "reconnect":
      // Missed events are not replayed: assume the daemon moved.
      if (m.closed || m.gone) return;
      m.stale = true;
      m.backoff = false;
      return;

    case "reload_ok": {
      if (m.phase !== "reloading" || e.seq !== m.readSeq) return;
      const from = m.reloadFrom;
      m.phase = "idle";
      m.reloadFrom = null;
      m.readSeq = null;
      if (m.buffer !== from) {
        // The user typed while the reload was in flight: keep the typing; the
        // next save merges the agent's change. Stay stale until clean.
        m.stale = true;
        return;
      }
      adopt(m, fx, e.doc);
      return;
    }

    case "reload_fail":
      if (m.phase !== "reloading" || e.seq !== m.readSeq) return;
      m.phase = "idle";
      m.reloadFrom = null;
      m.readSeq = null;
      m.stale = true;
      m.backoff = true;
      if (m.timer === null && !m.closed) arm(m, fx, "retry", SAVE_RETRY_MS);
      return;

    case "reconcile_ok": {
      if (m.phase !== "reconciling" || e.seq !== m.readSeq) return;
      const head = e.doc;
      m.phase = "idle";
      m.readSeq = null;
      // A request that ended before this read was issued is settled by it; one that ended
      // while it was in flight may have landed after it, and is read for again.
      const endedLater = m.late.some((l) => l.ended !== null && l.ended >= e.seq);
      m.unknown = endedLater;
      m.note = null;
      // An unknown save landed if the daemon's head already contains its edits:
      // merging them onto the head changes nothing.
      // (a request still out was made against the text current when it was sent)
      const baseOf = (s: string): string => m.late.find((l) => l.text === s)?.baseText ?? m.saved;
      const landed = [...m.inDoubt].reverse().find((s) => s !== m.saved && mergeWithMarkers(baseOf(s), s, head.text) === head.text);
      const baseText = landed ?? m.saved;
      m.late = m.late.filter((l) => l.ended === null || l.ended >= e.seq);
      m.inDoubt = m.late.map((l) => l.text); // a request still out can land later
      if (head.text === m.buffer) {
        m.base = head.commit;
        m.saved = head.text;
        return;
      }
      if (head.commit === m.base && head.text === m.saved) return; // nothing landed, nothing moved
      const merged = mergeWithMarkers(baseText, m.buffer, head.text);
      if (hasConflictMarkers(merged) || hasConflictMarkers(m.buffer)) {
        enterConflict(m, fx, { ours: m.buffer, theirs: head.text, base: baseText, head: head.commit });
        return;
      }
      m.base = head.commit;
      m.saved = head.text;
      if (merged !== m.buffer) {
        m.buffer = merged;
        fx.push({ t: "set_buffer", text: merged });
        m.note = { kind: "merged", detail: "" };
      }
      return;
    }

    case "reconcile_fail":
      if (m.phase !== "reconciling" || e.seq !== m.readSeq) return;
      m.phase = "idle";
      m.readSeq = null;
      m.backoff = true;
      m.note = { kind: "error", detail: e.error };
      if (m.timer === null) arm(m, fx, "retry", SAVE_RETRY_MS);
      return;

    case "leave":
    case "close": {
      if (e.t === "close") {
        m.closed = true;
        disarm(m, fx);
      }
      if (!m.loaded || m.gone) return;
      if (m.phase === "conflict") {
        // Nothing goes to the daemon. The draft is already in per-window storage.
        return;
      }
      if (m.phase === "idle" && !m.unknown && !m.fatal) {
        if (isDirty(m)) startSave(m, fx, true);
        return;
      }
      if (m.phase === "idle" && m.fatal) return;
      // A save, a reload or a reconcile is in flight, or a save's outcome is
      // unknown: send the final save once the daemon's state is known, against the
      // base that establishes.
      m.leaving = true;
      return;
    }

    case "keep_mine":
      if (m.phase !== "conflict") return;
      m.conflict = null;
      m.phase = "idle";
      m.base = e.head.commit;
      m.saved = e.head.text;
      m.note = null;
      if (canSave(m) && isDirty(m) && !hasConflictMarkers(m.buffer)) startSave(m, fx, false);
      return;

    case "take_theirs":
      if (m.phase !== "conflict") return;
      m.conflict = null;
      m.phase = "idle";
      m.note = null;
      adopt(m, fx, e.head);
      return;

    case "show_both": {
      if (m.phase !== "conflict" || !m.conflict) return;
      const merged = mergeWithMarkers(m.conflict.base, m.buffer, e.head.text);
      m.conflict = null;
      m.phase = "idle";
      m.note = null;
      m.base = e.head.commit;
      m.saved = e.head.text;
      m.buffer = merged;
      fx.push({ t: "set_buffer", text: merged });
      return;
    }
  }
}

/** The drain rule. After every transition: unsaved work is mirrored to storage; an
 *  unknown save outcome is reconciled with the daemon's head before anything else; a
 *  queued final save goes out; an idle, clean, stale document reloads; an idle,
 *  dirty one without markers arms autosave. */
function settle(m: Model, fx: Effect[]): void {
  // An abandoned sync is finished: mirroring its buffer would write back the draft
  // that Discard (or a landed delete) just removed.
  if (m.dead) return;
  syncStash(m, fx);
  if (m.phase !== "idle" || !m.loaded || m.suspended || m.gone) return;
  // A read is waiting out a backoff: something must wake it. Whatever cancelled the
  // retry timer (a close, a new autosave timer), it is armed again here.
  if (m.backoff && m.timer === null) arm(m, fx, "retry", SAVE_RETRY_MS);
  if (m.unknown) {
    // Reconcile first: never reload over, or resend, a save that may have landed.
    if (!m.backoff) startRead(m, fx, "reconcile");
    return;
  }
  if (m.leaving || m.closed) {
    // A closed tab keeps sending until its edits have landed (it reaches here again
    // after each reply), and a queued final save goes out now.
    const final = m.leaving;
    m.leaving = false;
    if (isDirty(m) && !m.fatal) {
      // The page is going: send now. Otherwise a save that keeps failing is retried with
      // a growing delay, not in a loop of save, read, save.
      if (m.fails > 0 && !final) {
        if (m.timer === null) arm(m, fx, "save", retryDelay(m.fails));
      } else startSave(m, fx, true);
    }
    return;
  }
  const dirty = isDirty(m);
  if (!dirty && m.stale && !m.backoff && !m.late.some((l) => l.ended === null)) {
    startRead(m, fx, "reload");
    return;
  }
  if (dirty && !m.fatal && !hasConflictMarkers(m.buffer)) {
    if (m.timer === null) arm(m, fx, "save", m.fails > 0 ? retryDelay(m.fails) : AUTOSAVE_DELAY_MS);
  } else if (m.timer === "save") {
    disarm(m, fx);
  }
}
