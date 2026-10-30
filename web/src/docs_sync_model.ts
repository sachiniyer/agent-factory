// The sync model of one open doc in the Docs tab: a pure reducer. `step` takes the
// current Model and an Event and returns the next Model and the effects the shell
// (docs_sync.ts) must run. There is no I/O, timer or DOM here, so every
// state-and-event pair can be tested, including by random interleavings
// (docs_sync_model.test.ts).
//
// The design is in the #6292 thread. In short: a document is in one `phase`; a
// save in flight, a reload in flight and a conflict are part of the state, not
// side flags; a remote change that cannot be applied yet is remembered in `stale`
// and `settle` drains it whenever the document is idle and clean; and a final
// save at leave time is queued behind a save already in flight.

import type { DocText, SaveResult } from "./proposal_api.js";
import { hasConflictMarkers, mergeWithMarkers } from "./text_diff.js";

export const AUTOSAVE_DELAY_MS = 500;
export const SAVE_RETRY_MS = 5000;

export type SyncStatus =
  | "loading"
  | "saved"
  | "pending"
  | "saving"
  | "merged"
  | "conflict"
  | "markers"
  | "error";

export interface Conflict {
  ours: string;
  theirs: string;
  base: string;
  /** The commit that `theirs` is at; "" when the daemon did not say. */
  head: string;
}

export type Phase = "loading" | "idle" | "saving" | "reloading" | "conflict";

export interface Flight {
  text: string;
  base: string;
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
  /** Set exactly when phase is "saving". */
  flight: Flight | null;
  /** Set exactly when phase is "conflict". */
  conflict: Conflict | null;
  /** The buffer when the reload started; set exactly when phase is "reloading". */
  reloadFrom: string | null;
  /** The daemon may be ahead of `base`. Drained by `settle`. */
  stale: boolean;
  /** A reload failed; no new reload until a timer fires. */
  backoff: boolean;
  /** A final save is wanted once the current one has replied. */
  leaving: boolean;
  /** The tab is gone: no timers, no reloads, only the final save. */
  closed: boolean;
  timer: "save" | "retry" | null;
  note: { kind: "merged" | "error"; detail: string } | null;
  loadFailed: string | null;
}

export type Event =
  | { t: "load_ok"; doc: DocText }
  | { t: "load_fail"; error: string }
  | { t: "edit"; text: string }
  | { t: "timer" }
  | { t: "save_ok"; result: Extract<SaveResult, { kind: "saved" }> }
  | { t: "save_conflict"; result: Extract<SaveResult, { kind: "conflict" }> }
  | { t: "save_fail"; error: string }
  | { t: "remote"; commit: string }
  | { t: "reload_ok"; doc: DocText }
  | { t: "reload_fail"; error: string }
  | { t: "reconnect" }
  | { t: "flush" }
  | { t: "leave" }
  | { t: "close" }
  | { t: "restore_draft"; draft: Draft }
  | { t: "keep_mine"; head: DocText }
  | { t: "take_theirs"; head: DocText }
  | { t: "show_both"; head: DocText };

export type Effect =
  | { t: "load"; reload: boolean }
  /** `track` false: nobody waits for the reply (a final save of an open conflict). */
  | { t: "save"; text: string; base: string; final: boolean; track: boolean }
  | { t: "set_buffer"; text: string }
  | { t: "arm"; kind: "save" | "retry"; ms: number }
  | { t: "disarm" }
  /** Keeps the conflict draft in per-viewer storage, so it survives closing the
   *  tab or the page. The proposal doc never receives it. */
  | { t: "stash"; draft: Draft }
  | { t: "unstash" };

/** An unresolved conflict's draft, as stored for this viewer. */
export interface Draft {
  buffer: string;
  /** The text the user's edits were made against. */
  base: string;
  /** The agent's side when the conflict arose. */
  theirs: string;
  /** The commit `theirs` is at. */
  head: string;
}

export function initialModel(): Model {
  return {
    phase: "loading",
    loaded: false,
    buffer: "",
    saved: "",
    base: "",
    flight: null,
    conflict: null,
    reloadFrom: null,
    stale: false,
    backoff: false,
    leaving: false,
    closed: false,
    timer: null,
    note: null,
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
  if (m.phase === "conflict") return { status: "conflict", detail: "" };
  if (m.phase === "saving") return { status: "saving", detail: "" };
  if (m.note?.kind === "error") return { status: "error", detail: m.note.detail };
  if (hasConflictMarkers(m.buffer)) return { status: "markers", detail: "" };
  if (isDirty(m)) return { status: "pending", detail: "" };
  if (m.note?.kind === "merged") return { status: "merged", detail: "" };
  return { status: "saved", detail: "" };
}

/** Returns the next model and the effects to run. The input is not modified. */
export function step(prev: Model, e: Event): [Model, Effect[]] {
  const m: Model = { ...prev };
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

function stash(m: Model, fx: Effect[]): void {
  const c = m.conflict;
  if (c) fx.push({ t: "stash", draft: { buffer: m.buffer, base: c.base, theirs: c.theirs, head: c.head } });
}

function enterConflict(m: Model, fx: Effect[], conflict: Conflict): void {
  m.phase = "conflict";
  m.conflict = conflict;
  m.leaving = false;
  disarm(m, fx);
  stash(m, fx);
}

function startSave(m: Model, fx: Effect[], final: boolean): void {
  disarm(m, fx);
  m.phase = "saving";
  m.flight = { text: m.buffer, base: m.base, final };
  m.note = null;
  fx.push({ t: "save", text: m.buffer, base: m.base, final, track: true });
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

function reduce(m: Model, e: Event, fx: Effect[]): void {
  switch (e.t) {
    case "load_ok":
      if (m.phase !== "loading") return;
      m.phase = "idle";
      m.loaded = true;
      m.loadFailed = null;
      m.buffer = e.doc.text;
      m.saved = e.doc.text;
      m.base = e.doc.commit;
      fx.push({ t: "set_buffer", text: e.doc.text });
      return;

    case "load_fail":
      if (m.phase === "loading") m.loadFailed = e.error;
      return;

    case "edit":
      // Before the first load the editor is read-only, so there is nothing to keep.
      if (!m.loaded || m.closed || e.text === m.buffer) return;
      m.buffer = e.text;
      m.note = null;
      if (m.phase === "conflict" && m.conflict) stash(m, fx);
      if (m.phase === "idle") {
        // Debounce: autosave runs a pause after the LAST keystroke.
        if (isDirty(m) && !hasConflictMarkers(m.buffer)) arm(m, fx, "save", AUTOSAVE_DELAY_MS);
        else if (m.timer === "save") disarm(m, fx);
      }
      return;

    case "timer": {
      const kind = m.timer;
      m.timer = null;
      m.backoff = false;
      if (kind === "save" && m.phase === "idle" && m.loaded && !m.closed && isDirty(m) && !hasConflictMarkers(m.buffer)) {
        startSave(m, fx, false);
      }
      return;
    }

    case "flush":
      if (m.phase === "idle" && m.loaded && !m.closed && isDirty(m) && !hasConflictMarkers(m.buffer)) {
        startSave(m, fx, false);
      }
      return;

    case "save_ok": {
      if (m.phase !== "saving" || !m.flight) return;
      const sent = m.flight.text;
      const r = e.result;
      m.flight = null;
      m.phase = "idle";
      m.note = null;
      const mergedAgentEdits = r.merged && r.text !== sent;
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
      if (m.phase !== "saving" || !m.flight) return;
      m.flight = null;
      const r = e.result;
      // Even when the tab is leaving, the draft goes to per-viewer storage, not
      // into the proposal doc.
      enterConflict(m, fx, { ours: r.ours, theirs: r.theirs, base: r.base, head: r.head });
      return;
    }

    case "save_fail":
      if (m.phase !== "saving") return;
      m.flight = null;
      m.phase = "idle";
      m.note = { kind: "error", detail: e.error };
      if (!m.closed) arm(m, fx, "save", SAVE_RETRY_MS);
      return;

    case "remote":
      if (m.closed || (!m.loaded && m.phase !== "loading")) return;
      if (e.commit !== "" && e.commit === m.base) return;
      m.stale = true;
      return;

    case "reconnect":
      // Missed events are not replayed: assume the daemon moved.
      if (m.closed) return;
      m.stale = true;
      m.backoff = false;
      return;

    case "reload_ok": {
      if (m.phase !== "reloading") return;
      const from = m.reloadFrom;
      m.phase = "idle";
      m.reloadFrom = null;
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
      if (m.phase !== "reloading") return;
      m.phase = "idle";
      m.reloadFrom = null;
      m.stale = true;
      m.backoff = true;
      if (m.timer === null && !m.closed) arm(m, fx, "retry", SAVE_RETRY_MS);
      return;

    case "leave":
    case "close": {
      if (e.t === "close") {
        m.closed = true;
        disarm(m, fx);
      }
      if (!m.loaded) return;
      if (m.phase === "conflict") {
        // Nothing goes to the daemon. The draft is already in per-viewer storage.
        stash(m, fx);
        return;
      }
      if (m.phase === "idle") {
        if (isDirty(m)) startSave(m, fx, true);
        return;
      }
      // A save or a reload is in flight: send the final save when it replies,
      // against the base that reply establishes.
      m.leaving = true;
      return;
    }

    case "restore_draft": {
      // The tab opened with a stored draft from an earlier, unresolved conflict.
      if (m.phase !== "idle" || !m.loaded || e.draft.buffer === m.saved) {
        if (e.draft.buffer === m.saved) fx.push({ t: "unstash" });
        return;
      }
      m.buffer = e.draft.buffer;
      fx.push({ t: "set_buffer", text: e.draft.buffer });
      enterConflict(m, fx, {
        ours: e.draft.buffer,
        theirs: m.saved,
        base: e.draft.base,
        head: m.base,
      });
      return;
    }

    case "keep_mine":
      if (m.phase !== "conflict") return;
      fx.push({ t: "unstash" });
      m.conflict = null;
      m.phase = "idle";
      m.base = e.head.commit;
      m.saved = e.head.text;
      m.note = null;
      if (isDirty(m) && !hasConflictMarkers(m.buffer)) startSave(m, fx, false);
      return;

    case "take_theirs":
      if (m.phase !== "conflict") return;
      fx.push({ t: "unstash" });
      m.conflict = null;
      m.phase = "idle";
      m.note = null;
      adopt(m, fx, e.head);
      return;

    case "show_both": {
      if (m.phase !== "conflict" || !m.conflict) return;
      const merged = mergeWithMarkers(m.conflict.base, m.buffer, e.head.text);
      fx.push({ t: "unstash" });
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

/** The drain rule. After every transition: a queued final save goes out; an idle,
 *  clean, stale document reloads; an idle, dirty one without markers arms autosave. */
function settle(m: Model, fx: Effect[]): void {
  if (m.phase !== "idle" || !m.loaded) return;
  if (m.leaving) {
    m.leaving = false;
    if (isDirty(m)) startSave(m, fx, true);
    return;
  }
  if (m.closed) return;
  const dirty = isDirty(m);
  if (!dirty && m.stale && !m.backoff) {
    m.phase = "reloading";
    m.reloadFrom = m.buffer;
    m.stale = false;
    disarm(m, fx);
    fx.push({ t: "load", reload: true });
    return;
  }
  if (dirty && !hasConflictMarkers(m.buffer)) {
    if (m.timer === null) arm(m, fx, "save", AUTOSAVE_DELAY_MS);
  } else if (m.timer === "save") {
    disarm(m, fx);
  }
}
