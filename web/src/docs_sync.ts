// The save loop of one open doc in the Docs tab. The decisions live in the pure
// reducer in docs_sync_model.ts; this class runs its effects (loads, saves,
// timers, the editor buffer) and feeds the replies back as events. No DOM here;
// the pane wires it to the editor.

import type { DocText, SaveResult } from "./proposal_api.js";
import { hasConflictMarkers } from "./text_diff.js";
import {
  AUTOSAVE_DELAY_MS,
  SAVE_GIVE_UP_MS,
  SAVE_RETRY_MS,
  type Conflict,
  type Draft,
  type Effect,
  type Event,
  type Model,
  type SyncStatus,
  initialModel,
  isDirty,
  statusOf,
  step,
} from "./docs_sync_model.js";

export { AUTOSAVE_DELAY_MS, SAVE_GIVE_UP_MS, SAVE_RETRY_MS };

/** A save with no reply by now has an unknown outcome: it may have landed. */
export const SAVE_TIMEOUT_MS = 30_000;

export type { Conflict, Draft, SyncStatus };

export interface DocSyncDeps {
  /** Reads the doc. `initial` is the first read of a tab: it may wait for tabs that
   *  closed on this doc to finish their saves; later reads (reload, reconcile) must
   *  not, or two closing tabs could wait on each other. */
  load(initial?: boolean): Promise<DocText>;
  /** `final` marks the save that dispose or page unload sends; nobody waits
   *  for its reply. */
  save(text: string, base: string, final?: boolean): Promise<SaveResult>;
  /** Replaces the editor buffer. The editor keeps the cursor and scroll. Called
   *  only while the doc is attached. */
  setBuffer(text: string): void;
  onStatus(status: SyncStatus, detail: string): void;
  /** Per-window storage for unsaved work: an unresolved conflict's draft, or edits
   *  no save has landed yet. A draft never goes into the proposal doc. Optional:
   *  without it a draft lives only in memory. */
  draft?: {
    load(): Draft | null;
    store(draft: Draft): void;
    clear(): void;
  };
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(handle: unknown): void;
}

export class DocSync {
  private model: Model = initialModel();
  private timer: unknown = null;
  private attached = true;
  private shown: { status: SyncStatus; detail: string } | null = null;
  private starting: { resolve(): void; reject(e: unknown): void } | null = null;
  private quietWaiters: (() => void)[] = [];
  private savedWaiters: ((v: "saved" | "blocked") => void)[] = [];
  /** The draft this sync last wrote to (or read from) per-viewer storage, as JSON. */
  private storedKey: string | null = null;
  private closed = false;

  constructor(private readonly deps: DocSyncDeps) {}

  get text(): string {
    return this.model.buffer;
  }

  get currentStatus(): SyncStatus {
    return statusOf(this.model).status;
  }

  get currentConflict(): Conflict | null {
    return this.model.conflict;
  }

  get baseCommit(): string {
    return this.model.base;
  }

  get dirty(): boolean {
    return isDirty(this.model);
  }

  get loaded(): boolean {
    return this.model.loaded;
  }

  /** The sync has ended (its doc was deleted with nothing unsaved): the pane drops it. */
  get ended(): boolean {
    return this.model.dead;
  }

  /** Loads the doc. Rejects when the first load fails. */
  start(): Promise<void> {
    const done = new Promise<void>((resolve, reject) => {
      this.starting = { resolve, reject };
    });
    this.emit();
    this.run({ t: "load", kind: "initial", seq: 0 });
    return done;
  }

  /** The user changed the buffer. */
  edit(text: string): void {
    this.dispatch({ t: "edit", text });
  }

  /** The daemon reported a new commit that touches this doc. */
  remoteChanged(commit: string): void {
    this.dispatch({ t: "remote", commit });
  }

  /** The event stream reconnected: events may have been missed, so the daemon
   *  may be ahead of this tab. A clean doc reloads; a dirty one merges on save. */
  reconnect(): void {
    this.dispatch({ t: "reconnect" });
  }

  /** Saves now if there is anything to save. */
  flush(): void {
    this.dispatch({ t: "flush" });
  }

  /** The user moved to another doc. Saves what is unsaved and keeps the whole
   *  state, an open conflict draft included, until the doc is shown again. */
  detach(): void {
    this.dispatch({ t: "flush" });
    this.attached = false;
  }

  /** The doc is shown again. Returns what the editor must show. */
  attach(): { text: string; status: SyncStatus; detail: string } {
    this.attached = true;
    this.shown = null;
    this.emit();
    return { text: this.model.buffer, ...statusOf(this.model) };
  }

  /** Sends unsaved edits now, conflict markers included: closing the tab or the
   *  page must not lose them. An unresolved conflict is not sent: its draft is
   *  in per-viewer storage and comes back when the doc opens. If a save is in flight the final save waits for its reply
   *  and goes out against the commit that reply establishes. */
  saveBeforeLeaving(): void {
    this.dispatch({ t: "leave" });
  }

  /** The daemon no longer has this doc (an agent or another window deleted it, or a
   *  request said so). A clean doc ends; one with unsaved work keeps it, in storage,
   *  and shows the "deleted" state until it is restored or discarded. */
  remoteDeleted(): void {
    this.dispatch({ t: "gone" });
  }

  /** The doc was added again: read its head and carry the kept work onto it. */
  async revive(): Promise<void> {
    const head = await this.deps.load();
    this.dispatch({ t: "revive", head });
  }

  /** The doc is being deleted. Stops the timers and retries; the unsaved edits stay
   *  in per-viewer storage until the delete has landed or failed. */
  suspend(): void {
    this.dispatch({ t: "suspend" });
  }

  /** The delete landed: this sync is finished, and its draft is dropped. When the
   *  delete failed, `keepDraft` leaves the draft for the tab that reopens the doc. */
  abandon(keepDraft = false): void {
    this.attached = false;
    this.closed = true;
    this.dispatch({ t: "abandon", keepDraft });
  }

  dispose(): void {
    this.attached = false;
    this.closed = true;
    this.dispatch({ t: "close" });
  }

  private async headOf(conflict: Conflict): Promise<DocText> {
    return conflict.head !== ""
      ? { text: conflict.theirs, commit: conflict.head }
      : this.deps.load();
  }

  /** Conflict banner: write my text over the agent's. */
  async keepMine(): Promise<void> {
    const head = await this.resolutionHead();
    if (head) this.dispatch({ t: "keep_mine", head });
  }

  /** Conflict banner: drop my edits and take the agent's text. */
  async takeTheirs(): Promise<void> {
    const head = await this.resolutionHead();
    if (head) this.dispatch({ t: "take_theirs", head });
  }

  /** Conflict banner: keep both versions between conflict markers, for the user
   *  to resolve. Saving waits until the markers are gone. */
  async showBoth(): Promise<void> {
    const head = await this.resolutionHead();
    if (head) this.dispatch({ t: "show_both", head });
  }

  private async resolutionHead(): Promise<DocText | null> {
    const conflict = this.model.conflict;
    if (!conflict) return null;
    const head = await this.headOf(conflict);
    return this.model.closed ? null : head;
  }

  /** Saves what is unsaved and resolves "saved" once it has landed, or "blocked"
   *  when it cannot (an open conflict, conflict markers, a failed save). Send now
   *  waits on this, so it never goes out ahead of a newer edit. */
  whenSaved(): Promise<"saved" | "blocked"> {
    this.pumpSaved();
    const now = this.savedVerdict();
    if (now) return Promise.resolve(now);
    return new Promise((resolve) => this.savedWaiters.push(resolve));
  }

  private savedVerdict(): "saved" | "blocked" | null {
    const m = this.model;
    if (!m.loaded) return m.loadFailed !== null ? "blocked" : null;
    if (m.phase === "conflict" || m.gone || m.fatal) return "blocked";
    // Clean is not saved while the outcome of a save is unknown: the daemon may hold
    // text the buffer has since moved off (an undo back to the acknowledged text), and
    // the corrective save follows the reconcile read.
    // A save that timed out can still land: until it has ended, the daemon may come to hold
    // text the buffer has moved off.
    if (!isDirty(m)) return m.phase === "saving" || m.phase === "reconciling" || m.unknown || m.late.some((l) => l.ended === null) ? null : "saved";
    if (m.phase === "idle" && !m.unknown && (hasConflictMarkers(m.buffer) || m.note?.kind === "error")) return "blocked";
    return null;
  }

  /** Starts the save of anything unsaved that is waiting on a timer. */
  private pumpSaved(): void {
    const m = this.model;
    if (
      m.phase === "idle" && isDirty(m) && !hasConflictMarkers(m.buffer) && m.note?.kind !== "error" &&
      !m.closed && !m.unknown && !m.gone && !m.fatal && !m.suspended && m.loaded &&
      // after failures the retry timer sends it, with its growing delay: a waiter that pumps on
      // every reply would make a loop of save, read, save
      m.fails === 0
    ) {
      this.dispatch({ t: "flush" });
    }
  }

  /** Resolves when no save is in flight and none is queued. A tab that reopens
   *  a doc waits for the closing tab's final save, so its first load does not
   *  return text older than what the user just typed. */
  whenQuiet(): Promise<void> {
    if (this.quiet()) return Promise.resolve();
    return new Promise((resolve) => this.quietWaiters.push(resolve));
  }

  get debugModel(): string {
    const { phase, loaded, flight, unknown, backoff, leaving, closed, gone, fatal, timer, stale, suspended, dead } = this.model;
    return JSON.stringify({ phase, loaded, flight: flight ? flight.seq : null, unknown, backoff, leaving, closed, gone, fatal, timer, stale, suspended, dead });
  }

  /** Whether no save is in flight or queued (see whenQuiet). */
  get isQuiet(): boolean {
    return this.quiet();
  }

  private quiet(): boolean {
    const m = this.model;
    if (m.dead) return true;
    if (m.phase === "saving" || m.leaving) return false;
    // A closed tab whose first read is still out will restore its stored draft when it
    // arrives, and send it: a tab that reopens the doc must wait for that too.
    if (m.closed && !m.loaded && m.loadFailed === null && !m.gone) return false;
    // A closed tab that is still retrying (unsent edits, a save whose outcome is
    // unknown, a read in flight) is not quiet: a tab that reopens the doc must not
    // load, or write, while the old one can still write.
    if (m.closed && !m.gone && !m.fatal && ((m.unknown && m.phase !== "conflict") || m.phase === "reconciling" || (m.phase === "idle" && isDirty(m)))) return false;
    return true;
  }

  private dispatch(e: Event): void {
    const [next, effects] = step(this.model, e);
    this.model = next;
    for (const fx of effects) this.run(fx);
    this.emit();
    if (this.savedWaiters.length) {
      this.pumpSaved();
      const v = this.savedVerdict();
      if (v) {
        const waiters = this.savedWaiters;
        this.savedWaiters = [];
        for (const w of waiters) w(v);
      }
    }
    if (this.quiet() && this.quietWaiters.length) {
      const waiters = this.quietWaiters;
      this.quietWaiters = [];
      for (const w of waiters) w();
    }
  }

  private emit(): void {
    if (!this.attached) return;
    const now = statusOf(this.model);
    if (this.shown && this.shown.status === now.status && this.shown.detail === now.detail) return;
    this.shown = now;
    this.deps.onStatus(now.status, now.detail);
  }

  private run(fx: Effect): void {
    switch (fx.t) {
      case "load": {
        const { kind, seq } = fx;
        this.deps.load(kind === "initial").then(
          (doc) => {
            if (kind === "initial") {
              this.dispatch({ t: "load_ok", doc });
              const draft = this.deps.draft?.load() ?? null;
              this.storedKey = draft ? JSON.stringify(draft) : null;
              this.dispatch({ t: "draft_read", draft });
              this.starting?.resolve();
            } else if (kind === "reload") {
              this.dispatch({ t: "reload_ok", seq, doc });
            } else {
              this.dispatch({ t: "reconcile_ok", seq, doc });
            }
          },
          (e: unknown) => {
            if (kind !== "initial" && statusOf404(e)) {
              this.dispatch({ t: "gone", of: "read", seq });
              return;
            }
            const error = errorMessage(e);
            if (kind === "initial") {
              this.dispatch({ t: "load_fail", error });
              this.starting?.reject(e);
            } else if (kind === "reload") {
              this.dispatch({ t: "reload_fail", seq, error });
            } else {
              this.dispatch({ t: "reconcile_fail", seq, error });
            }
          },
        );
        return;
      }
      case "save": {
        const { seq } = fx;
        let settled = false;
        // A save with no reply is not a failed save: it may have landed. After the
        // timeout its outcome is unknown, and a reply that comes later is ignored
        // (its request id no longer matches).
        let timedOut = false;
        let giveUp: unknown = null;
        const ended = (): void => {
          if (giveUp !== null) this.deps.clearTimer(giveUp);
          giveUp = null;
          this.dispatch({ t: "late_done", seq });
        };
        const timeout = this.deps.setTimer(() => {
          if (settled) return;
          settled = true;
          timedOut = true;
          // Still on the wire: it can land after the head has been read again.
          this.dispatch({ t: "save_fail", seq, error: "the daemon did not answer in time", outstanding: true });
          giveUp = this.deps.setTimer(ended, SAVE_GIVE_UP_MS);
        }, SAVE_TIMEOUT_MS);
        this.deps.save(fx.text, fx.base, fx.final).then(
          (result) => {
            if (settled) {
              if (timedOut) ended();
              return;
            }
            settled = true;
            this.deps.clearTimer(timeout);
            this.dispatch(result.kind === "conflict" ? { t: "save_conflict", seq, result } : { t: "save_ok", seq, result });
          },
          (e: unknown) => {
            if (settled) {
              if (timedOut) ended();
              return;
            }
            settled = true;
            this.deps.clearTimer(timeout);
            if (statusOf404(e)) this.dispatch({ t: "gone", of: "save", seq });
            else if (refused(e)) this.dispatch({ t: "save_fatal", seq, error: errorMessage(e) });
            else this.dispatch({ t: "save_fail", seq, error: errorMessage(e) });
          },
        );
        return;
      }
      case "set_buffer":
        if (this.attached) this.deps.setBuffer(fx.text);
        return;
      case "arm":
        this.cancelTimer();
        this.timer = this.deps.setTimer(() => {
          this.timer = null;
          this.dispatch({ t: "timer" });
        }, fx.ms);
        return;
      case "disarm":
        this.cancelTimer();
        return;
      case "stash":
        if (this.ownsStorage()) {
          this.deps.draft?.store(fx.draft);
          this.storedKey = JSON.stringify(fx.draft);
        }
        return;
      case "give_up":
        // The requests of an adopted draft (made by a pane that has closed) may still be out:
        // after the same wait as for our own, they count as ended and the head is read again.
        this.deps.setTimer(() => {
          for (const seq of fx.seqs) this.dispatch({ t: "late_done", seq });
        }, fx.ms);
        return;
      case "unstash":
        if (this.ownsStorage()) {
          this.deps.draft?.clear();
          this.storedKey = null;
        }
        return;
    }
  }

  /** A closed tab may still be finishing a save, while a newer tab has taken over
   *  the doc's draft. It then touches storage only while storage still holds what
   *  it wrote itself. */
  private ownsStorage(): boolean {
    if (!this.closed || !this.deps.draft) return true;
    const now = this.deps.draft.load();
    return (now ? JSON.stringify(now) : null) === this.storedKey;
  }

  private cancelTimer(): void {
    if (this.timer !== null) {
      this.deps.clearTimer(this.timer);
      this.timer = null;
    }
  }
}

function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** An error that carries an HTTP status (ApiError). */
function httpStatus(e: unknown): number | null {
  const status = (e as { status?: unknown } | null)?.status;
  return typeof status === "number" ? status : null;
}

function statusOf404(e: unknown): boolean {
  return httpStatus(e) === 404;
}

/** A 4xx other than a timeout or a rate limit: the daemon refused, and did not apply, the save. */
function refused(e: unknown): boolean {
  const status = httpStatus(e);
  return status !== null && status >= 400 && status < 500 && status !== 408 && status !== 429;
}
