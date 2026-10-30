// The save loop of one open doc in the Docs tab: autosave after a pause in
// typing, three-way merges done by the daemon, conflicts, and reloads when the
// agent changes the doc. No DOM here; the pane wires it to the editor.

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

export interface DocSyncDeps {
  load(): Promise<DocText>;
  /** `final` marks the save that dispose or page unload sends; nobody waits
   *  for its reply. */
  save(text: string, base: string, final?: boolean): Promise<SaveResult>;
  /** Replaces the editor buffer. The editor keeps the cursor and scroll. */
  setBuffer(text: string): void;
  onStatus(status: SyncStatus, detail: string): void;
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(handle: unknown): void;
}

export class DocSync {
  private buffer = "";
  /** The text at `base`, as far as this tab knows. */
  private saved = "";
  private base = "";
  private loaded = false;
  private inflight = false;
  private inflightText = "";
  private loading = false;
  private disposed = false;
  private remotePending = false;
  private timer: unknown = null;
  private conflict: Conflict | null = null;
  private status: SyncStatus = "loading";

  constructor(private readonly deps: DocSyncDeps) {}

  get text(): string {
    return this.buffer;
  }

  get currentStatus(): SyncStatus {
    return this.status;
  }

  get currentConflict(): Conflict | null {
    return this.conflict;
  }

  get baseCommit(): string {
    return this.base;
  }

  get dirty(): boolean {
    return this.buffer !== this.saved;
  }

  async start(): Promise<void> {
    this.setStatus("loading");
    this.loading = true;
    try {
      const doc = await this.deps.load();
      if (this.disposed) return;
      this.loaded = true;
      this.buffer = doc.text;
      this.saved = doc.text;
      this.base = doc.commit;
      this.deps.setBuffer(doc.text);
      this.setStatus("saved");
    } catch (e) {
      if (!this.disposed) this.setStatus("error", errorMessage(e));
      throw e;
    } finally {
      this.loading = false;
    }
  }

  /** The user changed the buffer. */
  edit(text: string): void {
    if (this.disposed || !this.loaded || text === this.buffer) {
      return;
    }
    this.buffer = text;
    if (this.conflict) {
      return;
    }
    if (hasConflictMarkers(text)) {
      this.cancelTimer();
      this.setStatus("markers");
      return;
    }
    this.setStatus(this.dirty ? "pending" : "saved");
    this.schedule(AUTOSAVE_DELAY_MS);
  }

  /** The daemon reported a new commit that touches this doc. A clean buffer
   *  reloads now; a dirty one waits for its next save, where the daemon merges. */
  remoteChanged(commit: string): void {
    if (this.disposed || !this.loaded || (commit !== "" && commit === this.base)) {
      return;
    }
    if (this.dirty || this.inflight || this.loading || this.conflict) {
      this.remotePending = true;
      return;
    }
    void this.reload();
  }

  /** Saves now if there is anything to save. */
  flush(): void {
    this.cancelTimer();
    void this.save();
  }

  /** Sends unsaved edits now, conflict markers included: autosave holds a
   *  buffer with markers, but closing the tab or the page must not lose it, and
   *  the agent can read both sides from the markers. A buffer with an open
   *  conflict is not sent, because the user has not chosen yet. */
  saveBeforeLeaving(): void {
    if (this.disposed || !this.loaded || !this.dirty || this.conflict) return;
    if (this.inflight && this.inflightText === this.buffer) return;
    this.cancelTimer();
    this.deps.save(this.buffer, this.base, true).catch(() => undefined);
  }

  dispose(): void {
    this.saveBeforeLeaving();
    this.disposed = true;
    this.cancelTimer();
  }

  private headOf(conflict: Conflict): Promise<DocText> {
    return conflict.head !== ""
      ? Promise.resolve({ text: conflict.theirs, commit: conflict.head })
      : this.deps.load();
  }

  /** Conflict banner: write my text over the agent's. */
  async keepMine(): Promise<void> {
    if (!this.conflict) return;
    const head = await this.headOf(this.conflict);
    if (this.disposed) return;
    this.conflict = null;
    this.base = head.commit;
    this.saved = head.text;
    this.flush();
  }

  /** Conflict banner: drop my edits and take the agent's text. */
  async takeTheirs(): Promise<void> {
    if (!this.conflict) return;
    const head = await this.headOf(this.conflict);
    if (this.disposed) return;
    this.conflict = null;
    this.adopt(head);
    this.setStatus("saved");
  }

  /** Conflict banner: keep both versions between conflict markers, for the user
   *  to resolve. Saving waits until the markers are gone. */
  async showBoth(): Promise<void> {
    const conflict = this.conflict;
    if (!conflict) return;
    const head = await this.headOf(conflict);
    if (this.disposed) return;
    this.conflict = null;
    const merged = mergeWithMarkers(conflict.base, this.buffer, head.text);
    this.base = head.commit;
    this.saved = head.text;
    this.buffer = merged;
    this.deps.setBuffer(merged);
    if (hasConflictMarkers(merged)) {
      this.setStatus("markers");
    } else {
      this.setStatus("pending");
      this.schedule(AUTOSAVE_DELAY_MS);
    }
  }

  private adopt(doc: DocText): void {
    this.base = doc.commit;
    this.saved = doc.text;
    if (doc.text !== this.buffer) {
      this.buffer = doc.text;
      this.deps.setBuffer(doc.text);
    }
  }

  private async reload(): Promise<void> {
    this.remotePending = false;
    this.loading = true;
    const before = this.buffer;
    try {
      const doc = await this.deps.load();
      if (this.disposed) return;
      if (this.buffer !== before || this.inflight) {
        // The user typed while the reload was in flight: keep the edits and let
        // the next save merge the agent's change.
        this.remotePending = true;
        return;
      }
      this.adopt(doc);
    } catch {
      this.remotePending = true;
    } finally {
      this.loading = false;
    }
  }

  private async save(): Promise<void> {
    if (this.disposed || !this.loaded || this.conflict || this.inflight) {
      return;
    }
    if (!this.dirty) {
      if (this.status !== "merged") this.setStatus("saved");
      if (this.remotePending) void this.reload();
      return;
    }
    if (hasConflictMarkers(this.buffer)) {
      this.setStatus("markers");
      return;
    }
    const sent = this.buffer;
    const sentBase = this.base;
    this.inflight = true;
    this.inflightText = sent;
    this.setStatus("saving");
    let result: SaveResult;
    try {
      result = await this.deps.save(sent, sentBase);
    } catch (e) {
      this.inflight = false;
      if (this.disposed) return;
      this.setStatus("error", errorMessage(e));
      this.schedule(SAVE_RETRY_MS);
      return;
    }
    this.inflight = false;
    if (this.disposed) return;
    if (result.kind === "conflict") {
      this.conflict = { ours: result.ours, theirs: result.theirs, base: result.base, head: result.head };
      this.cancelTimer();
      this.setStatus("conflict");
      return;
    }
    const mergedAgentEdits = result.merged && result.text !== sent;
    if (this.buffer === sent) {
      this.base = result.commit;
      this.saved = result.text;
      if (result.text !== sent) {
        this.buffer = result.text;
        this.deps.setBuffer(result.text);
      }
      this.setStatus(mergedAgentEdits ? "merged" : "saved");
      if (this.remotePending) void this.reload();
      return;
    }
    // The user typed during the save. Without a merge, the commit holds exactly
    // what was sent, so it is the new base. With a merge, the buffer lacks the
    // agent's edits, so keep the old base: the next save then merges again, and
    // the lines both sides already share merge cleanly.
    if (!mergedAgentEdits) {
      this.base = result.commit;
      this.saved = sent;
    }
    this.setStatus("pending");
    this.schedule(AUTOSAVE_DELAY_MS);
  }

  private schedule(ms: number): void {
    this.cancelTimer();
    this.timer = this.deps.setTimer(() => {
      this.timer = null;
      void this.save();
    }, ms);
  }

  private cancelTimer(): void {
    if (this.timer !== null) {
      this.deps.clearTimer(this.timer);
      this.timer = null;
    }
  }

  private setStatus(status: SyncStatus, detail = ""): void {
    this.status = status;
    this.deps.onStatus(status, detail);
  }
}

function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
