// Per-window storage for the Docs tab's unsaved work (see docs_sync_model.ts: an
// unresolved conflict's draft, or edits no save has landed yet).
//
// The same doc can be open in two windows that share one localStorage. Each window
// therefore writes only its own entry (keyed by a window id), and clears only its
// own, so one window can never clear or overwrite another's draft. A window that
// closes releases its entries; a window that opens a doc with no entry of its own
// adopts an orphan: an entry its owner released, or one its owner stopped refreshing
// (a crash, a killed tab). Adoption is copy-then-remove, so a race between two
// windows can duplicate a draft but never lose one.

import type { Draft } from "./docs_sync_model.js";

export interface KV {
  get(key: string): string | null;
  set(key: string, value: string): void;
  remove(key: string): void;
  keys(): string[];
}

const PREFIX = "af.docs.draft";

/** An entry its owner has not refreshed for this long is an orphan. */
export const DRAFT_STALE_MS = 30_000;
/** How often a window refreshes the entries it owns. */
export const DRAFT_HEARTBEAT_MS = 10_000;

interface Entry {
  owner: string;
  at: number;
  released: boolean;
  draft: Draft;
}

export class DraftStorage {
  /** `scope` names the proposal ("session\nslug"); docs are keyed within it. */
  constructor(
    private readonly kv: KV,
    readonly windowId: string,
    private readonly now: () => number,
    private readonly scope: string,
  ) {}

  private key(doc: string, owner: string): string {
    return `${PREFIX}\n${this.scope}\n${doc}\n${owner}`;
  }

  private legacyKey(doc: string): string {
    return `${PREFIX}\n${this.scope}\n${doc}`;
  }

  private read(key: string): Entry | null {
    try {
      const raw = this.kv.get(key);
      return raw === null ? null : (JSON.parse(raw) as Entry);
    } catch {
      return null;
    }
  }

  private write(key: string, entry: Entry): void {
    try {
      this.kv.set(key, JSON.stringify(entry));
    } catch {
      // Storage can be unavailable; the draft then lives only in memory.
    }
  }

  /** This window's draft for `doc`, else an orphan adopted from a closed or dead
   *  window, else null. */
  load(doc: string): Draft | null {
    const own = this.read(this.key(doc, this.windowId));
    if (own) return own.draft;
    const t = this.now();
    let best: { key: string; entry: Entry } | null = null;
    const prefix = `${PREFIX}\n${this.scope}\n${doc}\n`;
    for (const key of this.kv.keys()) {
      if (!key.startsWith(prefix)) continue;
      const entry = this.read(key);
      if (!entry || entry.owner === this.windowId) continue;
      if (!entry.released && t - entry.at <= DRAFT_STALE_MS) continue; // its owner is alive
      if (!best || entry.at > best.entry.at) best = { key, entry };
    }
    if (best) {
      this.write(this.key(doc, this.windowId), { owner: this.windowId, at: t, released: false, draft: best.entry.draft });
      this.kv.remove(best.key);
      return best.entry.draft;
    }
    // A draft stored before entries had owners.
    try {
      const raw = this.kv.get(this.legacyKey(doc));
      if (raw !== null) {
        const draft = JSON.parse(raw) as Draft;
        this.write(this.key(doc, this.windowId), { owner: this.windowId, at: t, released: false, draft });
        this.kv.remove(this.legacyKey(doc));
        return draft;
      }
    } catch {
      // Unreadable: nothing to restore.
    }
    return null;
  }

  store(doc: string, draft: Draft): void {
    this.write(this.key(doc, this.windowId), { owner: this.windowId, at: this.now(), released: false, draft });
  }

  /** Removes this window's entry only. */
  clear(doc: string): void {
    try {
      this.kv.remove(this.key(doc, this.windowId));
    } catch {
      // Nothing stored, nothing to clear.
    }
  }

  /** Every entry this window owns, for every doc of every proposal. */
  private owned(): string[] {
    const suffix = `\n${this.windowId}`;
    return this.kv.keys().filter((k) => k.startsWith(`${PREFIX}\n`) && k.endsWith(suffix));
  }

  /** The heartbeat: tells other windows this one is alive and still holds its entries. */
  touch(): void {
    const t = this.now();
    for (const key of this.owned()) {
      const e = this.read(key);
      if (e) this.write(key, { ...e, at: t, released: false });
    }
  }

  /** The window is closing: its entries can be adopted at once. */
  release(): void {
    for (const key of this.owned()) {
      const e = this.read(key);
      if (e) this.write(key, { ...e, released: true });
    }
  }
}

/** localStorage as a KV. */
export function localStorageKV(storage: Storage): KV {
  return {
    get: (k) => storage.getItem(k),
    set: (k, v) => storage.setItem(k, v),
    remove: (k) => storage.removeItem(k),
    keys: () => Array.from({ length: storage.length }, (_, i) => storage.key(i)).filter((k): k is string => k !== null),
  };
}

/** An id for this window, stable across a reload of it (sessionStorage), distinct
 *  between windows. */
export function windowId(): string {
  try {
    const known = window.sessionStorage.getItem("af.window.id");
    if (known) return known;
    const id = `w${Math.random().toString(36).slice(2, 10)}${Date.now().toString(36)}`;
    window.sessionStorage.setItem("af.window.id", id);
    return id;
  } catch {
    return `w${Math.random().toString(36).slice(2, 10)}`;
  }
}
