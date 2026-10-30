// The save loop of one open doc in the Docs tab. The decisions live in the pure
// reducer in docs_sync_model.ts; this class runs its effects (loads, saves,
// timers, the editor buffer) and feeds the replies back as events. No DOM here;
// the pane wires it to the editor.

import type { DocText, SaveResult } from "./proposal_api.js";
import {
  AUTOSAVE_DELAY_MS,
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

export { AUTOSAVE_DELAY_MS, SAVE_RETRY_MS };
export type { Conflict, Draft, SyncStatus };

export interface DocSyncDeps {
  load(): Promise<DocText>;
  /** `final` marks the save that dispose or page unload sends; nobody waits
   *  for its reply. */
  save(text: string, base: string, final?: boolean): Promise<SaveResult>;
  /** Replaces the editor buffer. The editor keeps the cursor and scroll. Called
   *  only while the doc is attached. */
  setBuffer(text: string): void;
  onStatus(status: SyncStatus, detail: string): void;
  /** Per-viewer storage for an unresolved conflict's draft. A draft never goes
   *  into the proposal doc. Optional: without it a draft lives only in memory. */
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

  /** Loads the doc. Rejects when the first load fails. */
  start(): Promise<void> {
    const done = new Promise<void>((resolve, reject) => {
      this.starting = { resolve, reject };
    });
    this.emit();
    this.run({ t: "load", reload: false });
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

  /** Resolves when no save is in flight and none is queued. A tab that reopens
   *  a doc waits for the closing tab's final save, so its first load does not
   *  return text older than what the user just typed. */
  whenQuiet(): Promise<void> {
    if (this.quiet()) return Promise.resolve();
    return new Promise((resolve) => this.quietWaiters.push(resolve));
  }

  private quiet(): boolean {
    return this.model.phase !== "saving" && !this.model.leaving;
  }

  private dispatch(e: Event): void {
    const [next, effects] = step(this.model, e);
    this.model = next;
    for (const fx of effects) this.run(fx);
    this.emit();
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
        const reload = fx.reload;
        this.deps.load().then(
          (doc) => {
            if (reload) {
              this.dispatch({ t: "reload_ok", doc });
              return;
            }
            const draft = this.deps.draft?.load() ?? null;
            this.storedKey = draft ? JSON.stringify(draft) : null;
            this.dispatch({ t: "load_ok", doc, draft });
            this.starting?.resolve();
          },
          (e: unknown) => {
            this.dispatch(reload ? { t: "reload_fail", error: errorMessage(e) } : { t: "load_fail", error: errorMessage(e) });
            if (!reload) this.starting?.reject(e);
          },
        );
        return;
      }
      case "save": {
        const sent = this.deps.save(fx.text, fx.base, fx.final);
        if (!fx.track) {
          sent.catch(() => undefined);
          return;
        }
        sent.then(
          (result) => this.dispatch(result.kind === "conflict" ? { t: "save_conflict", result } : { t: "save_ok", result }),
          (e: unknown) => this.dispatch({ t: "save_fail", error: errorMessage(e) }),
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
