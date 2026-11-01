// Per-window storage for the Docs tab's unsaved work (see docs_sync_model.ts: an
// unresolved conflict's draft, or edits no save has landed yet).
//
// The same doc can be open in two windows that share one localStorage, or in two
// panes of one window. Each pane therefore writes only its own entry (keyed by an
// owner: the window id and a pane id), and clears only its own, so one pane can
// never clear or overwrite another's draft. A pane that closes, or a window that
// unloads, releases its entries; a pane that opens a doc with no entry of its own
// adopts an orphan: an entry its owner released, or one its owner stopped refreshing
// (a crash, a killed tab). Adoption is copy-then-remove, and the source is removed
// only once the copy is stored: a race between two panes can duplicate a draft, and
// a full storage keeps the source, but neither loses one.

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
  /** The draft the pane's sync is working from, or null while the pane holds only drafts it took over
   *  and has not yet been asked to restore (`load` brings the next one forward). */
  draft: Draft | null;
  /** Other drafts of the doc this pane took over together with `draft` (orphans of several closed or
   *  dead panes): kept here so none is left under a closed pane's id and none is lost. */
  more?: Draft[];
}

const sameDraft = (a: Draft, b: Draft): boolean => JSON.stringify(a) === JSON.stringify(b);

/** Every draft an entry holds, the working one first. */
const draftsOf = (e: Entry): Draft[] => [...(e.draft ? [e.draft] : []), ...(e.more ?? [])];

/** The draft a load of this entry would restore. */
const currentDraft = (e: Entry): Draft | null => e.draft ?? e.more?.[0] ?? null;

/** `list` without repeats (the first of identical drafts is kept). */
const distinct = (list: Draft[]): Draft[] => list.reduce<Draft[]>((out, d) => (out.some((x) => sameDraft(x, d)) ? out : [...out, d]), []);

export class DraftStorage {
  /** The entries this pane owns are keyed by this id. */
  readonly owner: string;
  /** Where each doc's adopted draft came from, until this pane's own copy is the
   *  only one that matters. */
  private readonly adopted = new Map<string, string>();
  /** What this pane last stored per doc, for a bfcache restore to write back. */
  private readonly held = new Map<string, Draft>();
  private lastTouch: number | null = null;
  /** The pane is gone for good: whatever its still-closing syncs write later is
   *  released at once. */
  private closed = false;
  /** The page is unloading: writes stay released until the page is shown again. */
  private unloading = false;

  /** `scope` names the proposal ("session\nslug"); docs are keyed within it. `pane`
   *  tells apart two panes of one window. */
  constructor(
    private readonly kv: KV,
    readonly windowId: string,
    private readonly now: () => number,
    private readonly scope: string,
    pane = "0",
  ) {
    this.owner = `${windowId}.${pane}`;
  }

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

  /** Whether the entry was stored. Storage can be full or unavailable; the draft
   *  then lives only in memory, and nothing else may be removed on its account. */
  private write(key: string, entry: Entry): boolean {
    try {
      this.kv.set(key, JSON.stringify(entry));
      return true;
    } catch {
      return false;
    }
  }

  private remove(key: string): void {
    try {
      this.kv.remove(key);
    } catch {
      // Nothing to remove.
    }
  }

  /** The entries of `doc` this pane may take over: its own, and the orphans of closed
   *  or dead panes. */
  private takeable(doc: string): { key: string; entry: Entry }[] {
    const t = this.now();
    const out: { key: string; entry: Entry }[] = [];
    const prefix = `${PREFIX}\n${this.scope}\n${doc}\n`;
    for (const key of this.kv.keys()) {
      if (!key.startsWith(prefix)) continue;
      const entry = this.read(key);
      if (!entry) continue;
      if (entry.owner === this.owner || entry.released || t - entry.at > DRAFT_STALE_MS) out.push({ key, entry });
    }
    return out;
  }

  /** The docs of this proposal that have a stored draft this pane may take over,
   *  whether or not the doc still exists. Nothing is adopted. */
  docs(): string[] {
    const prefix = `${PREFIX}\n${this.scope}\n`;
    const names = new Set<string>();
    for (const key of this.kv.keys()) {
      if (!key.startsWith(prefix)) continue;
      const doc = key.slice(prefix.length).split("\n")[0]!;
      if (names.has(doc)) continue;
      if (key === this.legacyKey(doc) || this.takeable(doc).length > 0) names.add(doc);
    }
    return [...names];
  }

  /** The draft `load` would return, without adopting it. */
  peek(doc: string): Draft | null {
    const best = this.takeable(doc).sort((a, b) => Number(b.entry.owner === this.owner) - Number(a.entry.owner === this.owner) || b.entry.at - a.entry.at)[0];
    if (best) return currentDraft(best.entry);
    try {
      const raw = this.kv.get(this.legacyKey(doc));
      return raw === null ? null : (JSON.parse(raw) as Draft);
    } catch {
      return null;
    }
  }

  /** This window's draft for `doc`, else an orphan adopted from a closed or dead
   *  window, else null. */
  load(doc: string): Draft | null {
    const draft = this.find(doc);
    // What a pane loads or adopts is as much its work as what it stores: the sync restores it
    // without storing it again, so a bfcache restore needs it here to write it back.
    if (draft) this.held.set(doc, draft);
    return draft;
  }

  private find(doc: string): Draft | null {
    const ownKey = this.key(doc, this.owner);
    const own = this.read(ownKey);
    // A pane that is gone takes nothing over: its closing syncs only check whether what
    // they last wrote is still stored, and must not move another pane's draft here.
    if (this.closed) return own ? currentDraft(own) : null;
    const t = this.now();
    // The orphans of closed or dead panes, newest first (a live pane's entry is not ours to take).
    const orphans: { key: string; entry: Entry }[] = [];
    const prefix = `${PREFIX}\n${this.scope}\n${doc}\n`;
    for (const key of this.kv.keys()) {
      if (!key.startsWith(prefix) || key === ownKey) continue;
      const entry = this.read(key);
      if (!entry || entry.owner === this.owner) continue;
      if (!entry.released && t - entry.at <= DRAFT_STALE_MS) continue; // its owner is alive
      orphans.push({ key, entry });
    }
    orphans.sort((a, b) => b.entry.at - a.entry.at);
    // However many orphans there are, and whether or not this pane already holds drafts of the doc, none
    // is left under the id of a pane that closed: this pane's own drafts come first, then the orphans'
    // newest first, identical ones dropped; the first is the draft restored now and the rest stay on this
    // pane's entry until a later load restores them.
    if (orphans.length > 0) {
      const all = distinct([...(own ? draftsOf(own) : []), ...orphans.flatMap((o) => draftsOf(o.entry))]);
      const primary = all[0]!;
      const more = all.slice(1);
      const entry: Entry = { owner: this.owner, at: t, released: this.unloading, draft: primary, ...(more.length ? { more } : {}) };
      if (this.write(ownKey, entry)) {
        for (const o of orphans) this.remove(o.key);
      } else if (!own) {
        this.adopted.set(doc, orphans[0]!.key); // not stored here yet: the sources stay until it is
      }
      return primary;
    }
    if (own) {
      if (own.draft) return own.draft;
      // Only drafts taken over earlier: the next one is restored now.
      const next = own.more?.[0];
      if (!next) return null;
      const rest = own.more!.slice(1);
      this.write(ownKey, { ...own, draft: next, ...(rest.length ? { more: rest } : { more: undefined }) });
      return next;
    }
    // A draft stored before entries had owners.
    try {
      const raw = this.kv.get(this.legacyKey(doc));
      if (raw !== null) {
        const draft = JSON.parse(raw) as Draft;
        if (this.write(this.key(doc, this.owner), { owner: this.owner, at: t, released: false, draft })) this.remove(this.legacyKey(doc));
        else this.adopted.set(doc, this.legacyKey(doc));
        return draft;
      }
    } catch {
      // Unreadable: nothing to restore.
    }
    return null;
  }

  store(doc: string, draft: Draft): void {
    const ownKey = this.key(doc, this.owner);
    const more = this.carried(doc);
    if (!this.write(ownKey, { owner: this.owner, at: this.now(), released: this.closed || this.unloading, draft, ...(more.length ? { more } : {}) })) return;
    this.dropAdopted(doc); // this pane's copy now holds the work
    this.held.set(doc, draft);
    this.lastTouch = this.now();
  }

  /** The drafts this pane holds for `doc` besides the one its sync works from: the ones it took over, and
   *  those that came bundled with a source it has not yet copied (a copy that did not fit). */
  private carried(doc: string): Draft[] {
    const prev = this.read(this.key(doc, this.owner));
    const from = this.adopted.get(doc);
    const source = from === undefined ? null : this.read(from);
    return distinct([...(prev?.more ?? []), ...(source?.more ?? [])]);
  }

  /** Removes this pane's draft of the doc, and the adopted source it was still holding. Other drafts it took
   *  over with it are not the user's to lose: they stay, and the next load restores the next one. */
  clear(doc: string): void {
    this.held.delete(doc);
    const key = this.key(doc, this.owner);
    const more = this.carried(doc);
    if (more.length) {
      this.write(key, { owner: this.owner, at: this.now(), released: this.closed || this.unloading, draft: null, more });
    } else {
      this.remove(key);
    }
    this.dropAdopted(doc);
  }

  private dropAdopted(doc: string): void {
    const from = this.adopted.get(doc);
    if (from === undefined) return;
    this.adopted.delete(doc);
    this.remove(from);
  }

  /** Every entry this pane owns, for every doc of its proposal. */
  private owned(): string[] {
    const suffix = `\n${this.owner}`;
    return this.kv.keys().filter((k) => k.startsWith(`${PREFIX}\n`) && k.endsWith(suffix));
  }

  /** The heartbeat: tells other panes this one is alive and still holds its entries. */
  touch(restore = false): void {
    if (this.closed) return;
    this.unloading = false;
    const t = this.now();
    // A beat later than the staleness window means the heartbeat stalled (a throttled background
    // tab) and another window may have taken an entry as dead.
    const stalled = this.lastTouch !== null && t - this.lastTouch > DRAFT_STALE_MS;
    this.lastTouch = t;
    // A heartbeat that resumed after a stall (a throttled background tab) finds that another
    // window took an entry for dead: the sync still counts that work as stored, so it is
    // written back here, not only on a bfcache restore.
    if (restore || stalled) {
      for (const [doc, draft] of this.held) {
        const key = this.key(doc, this.owner);
        if (!this.read(key)) this.write(key, { owner: this.owner, at: t, released: false, draft });
      }
    }
    for (const key of this.owned()) {
      const e = this.read(key);
      if (e) this.write(key, { ...e, at: t, released: false });
    }
  }

  /** The page came back from the back-forward cache. Its pagehide released its entries, and
   *  another window may have adopted one and removed it; the sync here still holds that work
   *  as a stashed draft and will not store it again. Entries that are gone are written back
   *  (two copies of a draft are harmless, a lost one is not), then the rest are refreshed. */
  revive(): void {
    if (this.closed) return;
    this.unloading = false;
    this.touch(true);
  }

  /** The pane is gone for good. Its entries can be adopted at once, and what its syncs
   *  still write while they finish is released too (a late conflict draft must not
   *  look like a live pane's for the next 30 seconds). */
  close(): void {
    this.closed = true;
    this.release();
  }

  /** The pane is closing, or its window is: its entries can be adopted at once. */
  release(): void {
    this.unloading = true; // the final saves that follow write released entries too
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

/** A new id for a page. Random, never read back from storage: a duplicated browser tab
 *  starts with a copy of the original's sessionStorage, so a stored id would give two live
 *  pages one identity (and one set of draft entries to clear). A reload gets a new id, and
 *  adopts the old page's released entries like any pane. */
export function newWindowId(random: () => number = Math.random, now: () => number = Date.now): string {
  return `w${random().toString(36).slice(2, 10)}${now().toString(36)}`;
}

let pageId: string | null = null;

/** This page's id, fixed for the life of the page. */
export function windowId(): string {
  pageId ??= newWindowId();
  return pageId;
}
