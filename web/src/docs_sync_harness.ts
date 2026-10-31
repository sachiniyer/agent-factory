// A model-based test harness for the Docs sync engine: an in-memory daemon, a
// network that delays and reorders replies, fake timers, a one-editor pane, and a
// seeded random driver that interleaves user edits, agent edits, events, timers,
// document switches, tab closes, outages and conflict resolutions, then checks the
// invariants (see runScenario). It drives the engine only through the public
// DocSync surface, so it can run against any engine with that surface.

import type { DocText, SaveResult } from "./proposal_api.js";
import { DRAFT_STALE_MS, DraftStorage, newWindowId, type KV } from "./docs_draft_store.js";
import { ClosingTabs } from "./docs_closing.js";
import { missingFrom, RosterOrder } from "./docs_roster.js";
import { CONFLICT_MINE, CONFLICT_SPLIT, CONFLICT_THEIRS, hasConflictMarkers } from "./text_diff.js";

export interface SyncLike {
  readonly text: string;
  readonly currentStatus: string;
  readonly ended?: boolean;
  readonly isQuiet?: boolean;
  readonly debugModel?: string;
  whenQuiet?(): Promise<void>;
  /** The daemon no longer has the doc (a remote delete, or a 404). */
  remoteDeleted?(): void;
  /** The doc was added again: carry the kept work onto it. */
  revive?(): Promise<void>;
  /** The doc is being deleted: stop retrying and timers, keep the draft. */
  suspend?(): void;
  /** The delete landed: drop the sync and its draft. `keepDraft` when it failed. */
  abandon?(keepDraft?: boolean): void;
  /** Flushes unsaved edits and resolves when they have landed ("saved") or cannot
   *  ("blocked": a conflict, markers, or a failed save). */
  whenSaved?(): Promise<"saved" | "blocked">;
  start(): Promise<void>;
  edit(text: string): void;
  remoteChanged(commit: string): void;
  flush(): void;
  saveBeforeLeaving(): void;
  dispose(): void;
  keepMine(): Promise<void>;
  takeTheirs(): Promise<void>;
  showBoth(): Promise<void>;
  /** Engines without these model the old pane: dispose on a switch, nothing on reconnect. */
  detach?(): void;
  attach?(): { text: string };
  reconnect?(): void;
}

export interface Draft {
  buffer: string;
  base: string;
  theirs: string;
  head: string;
  kind?: "conflict" | "unsent";
}

export interface SyncDepsLike {
  load(initial?: boolean): Promise<DocText>;
  save(text: string, base: string, final?: boolean, fence?: { writer: string; seq: number }): Promise<SaveResult>;
  setBuffer(text: string): void;
  onStatus(status: string, detail: string): void;
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(handle: unknown): void;
  draft?: { load(): Draft | null; store(d: Draft): void; clear(): void };
}

export type MakeSync = (deps: SyncDepsLike) => SyncLike;

const LINES = 5;
const tick = (): Promise<void> => new Promise((r) => setImmediate(r));

/** A small deterministic PRNG (mulberry32). */
export function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

class Daemon {
  deleted = false;
  private readonly texts = new Map<string, string>();
  /** The daemon is restarting: requests are refused. */
  down = 0;
  head = "";
  private n = 0;
  readonly commits: string[] = [];
  constructor(initial: string) {
    this.commit(initial);
  }
  private commit(text: string): string {
    const id = `c${++this.n}`;
    this.texts.set(id, text);
    this.head = id;
    this.commits.push(id);
    return id;
  }
  get text(): string {
    return this.texts.get(this.head)!;
  }
  /** Whether any commit of this doc ever held the line `token`. */
  everHad(token: string): boolean {
    return [...this.texts.values()].some((t) => t.split("\n").includes(token));
  }
  read(): DocText {
    if (this.deleted) throw new Error("404 doc not found");
    return { text: this.text, commit: this.head };
  }
  /** The daemon's head goes back to the commit before it (a restore from a backup). */
  rollback(): string | null {
    if (this.commits.length < 2) return null;
    const prev = this.commits[this.commits.length - 2]!;
    this.commits.push(prev);
    this.head = prev;
    return prev;
  }
  /** The doc is added again from its template: a new commit with the original text. */
  /** Counts re-adds: a request made against an earlier incarnation of the doc must not land in this one. */
  epoch = 0;
  readd(initial: string): void {
    this.epoch++;
    this.deleted = false;
    this.commit(initial);
  }
  setLine(line: number, token: string): string {
    const lines = this.text.split("\n");
    lines[line] = token;
    return this.commit(lines.join("\n"));
  }
  /** SaveProposalDoc: fast-forward on the head, else a per-line three-way merge. */
  /** The daemon's writer fence: a save at or below the highest seq that has committed for its
   *  writer is refused, and the mark moves only when a save commits. */
  private readonly fenced = new Map<string, number>();
  save(text: string, base: string, fence?: { writer: string; seq: number }): SaveResult {
    if (this.deleted) throw new Error("404 doc not found");
    if (fence && !process.env.AF_NO_FENCE) {
      const last = this.fenced.get(fence.writer);
      if (last !== undefined && fence.seq <= last) throw Object.assign(new Error("409 proposal_stale_save"), { status: 409 });
    }
    const result = this.saveUnfenced(text, base);
    if (fence && result.kind === "saved") this.fenced.set(fence.writer, Math.max(this.fenced.get(fence.writer) ?? 0, fence.seq));
    return result;
  }
  private saveUnfenced(text: string, base: string): SaveResult {
    if (base === this.head) {
      return { kind: "saved", text, commit: text === this.text ? this.head : this.commit(text), merged: false };
    }
    const b = (this.texts.get(base) ?? "").split("\n");
    const o = text.split("\n");
    const t = this.text.split("\n");
    const out: string[] = [];
    for (let i = 0; i < Math.max(b.length, o.length, t.length); i++) {
      const [bl, ol, tl] = [b[i], o[i], t[i]];
      if (ol === tl) out.push(ol ?? "");
      else if (ol === bl) out.push(tl ?? "");
      else if (tl === bl) out.push(ol ?? "");
      else return { kind: "conflict", ours: text, theirs: this.text, base: b.join("\n"), head: this.head };
    }
    const merged = out.join("\n");
    return { kind: "saved", text: merged, commit: merged === this.text ? this.head : this.commit(merged), merged: true };
  }
}

interface Pending {
  /** The window that made the request: a closing window takes only its own with it. */
  owner: "a" | "g";
  /** "held": the request is still on its way to the daemon, and reaches it when released. */
  stage: "sent" | "processed" | "held";
  /** The reply never arrives (the client times out). */
  hang?: boolean;
  run(): unknown;
  result?: { ok: true; value: unknown } | { ok: false; error: Error };
  resolve(v: unknown): void;
  reject(e: unknown): void;
}

/** A sync of an engine that can be tracked as closed (whenQuiet and abandon). */
type Closable = SyncLike & { whenQuiet(): Promise<void>; abandon(keepDraft: boolean): void };

export interface ScenarioOptions {
  seed: number;
  steps: number;
  /** Users and the agent may edit the same lines (conflicts happen). */
  collide: boolean;
  /** Probability that a save fails: dropped before the daemon sees it, or applied
   *  with the reply lost. The page may also close with saves in flight. */
  faults?: number;
  /** Also exercises every external event: remote delete and re-add, daemon restarts,
   *  a second window on the same doc, a head that goes backwards, refusals. */
  chaos?: boolean;
  makeSync: MakeSync;
}

export interface Violation {
  invariant: string;
  detail: string;
  /** The operations that led here, for reproducing a seed. */
  trace: string[];
}

/** Runs one random interleaving. Returns the first invariant violation, or null. */
export async function runScenario(o: ScenarioOptions): Promise<Violation | null> {
  const rand = rng(o.seed);
  const pick = <T>(xs: readonly T[]): T => xs[Math.floor(rand() * xs.length)]!;
  const names = ["goals", "api"];
  const daemons = new Map(names.map((n) => [n, new Daemon(Array.from({ length: LINES }, (_, i) => `${n}${i}`).join("\n"))]));
  const kvMap = new Map<string, string>();
  let storageFull = false;
  const kv: KV = {
    get: (k) => kvMap.get(k) ?? null,
    set: (k, v) => {
      // A full storage refuses the reopened page a new entry (an existing one is replaced
      // in place). Only that page is held to it: another pane whose own stash fails
      // holds its work in memory only, which is not what is being tested.
      if (storageFull && !kvMap.has(k) && k.endsWith(`\n${storageA.owner}`)) throw new Error("QuotaExceededError");
      if (process.env.AF_KVLOG && v.includes(process.env.AF_KVLOG)) console.log("KV set", k.split("\n").slice(-2).join("/"), v.slice(0, 160).replace(/\n/g, "|"));
      kvMap.set(k, v);
    },
    remove: (k) => {
      if (process.env.AF_KVLOG && (kvMap.get(k) ?? "").includes(process.env.AF_KVLOG)) console.log("KV remove", k.split("\n").slice(-2).join("/"));
      kvMap.delete(k);
    },
    keys: () => [...kvMap.keys()],
  };
  let winSeq = 0;
  let storageA = new DraftStorage(kv, `wA${winSeq}`, () => now, "s\nplan");
  // The second pane is sometimes in the same window as the first: the same window id
  // and its own pane id. (AF_SAME_OWNER gives it the first pane's owner too: the
  // engine before panes had owners of their own.)
  const sameWindow = !!o.chaos && rand() < 0.5;
  let ghostSeq = 0;
  // The second window gets its id from the same function the page uses. (AF_DUP_TAB: a
  // duplicated browser tab, whose copied sessionStorage gave it the first window's id, and
  // whose panes count from the start again.)
  const newGhostStorage = (): DraftStorage => {
    const dup = !!process.env.AF_DUP_TAB;
    const win = sameWindow || dup ? "wA0" : newWindowId(rand, () => 0);
    const pane = (process.env.AF_SAME_OWNER && sameWindow) || dup ? "0" : `g${++ghostSeq}`;
    return new DraftStorage(kv, win, () => now, "s\nplan", pane);
  };
  let ghostStorage = newGhostStorage();
  const storageOf = new Map<SyncLike, DraftStorage>();
  /** The owners of panes that have closed. */
  const closedOwners = new Set<string>();
  /** Every unsaved buffer held in storage for `doc`, whichever window owns it. */
  const stashedBuffers = (doc: string): string[] =>
    [...kvMap.entries()]
      .filter(([k]) => k.startsWith(`af.docs.draft\ns\nplan\n${doc}\n`))
      .map(([, v]) => (JSON.parse(v) as { draft: Draft }).draft.buffer);
  const initialText = (n: string): string => Array.from({ length: LINES }, (_, i) => `${n}${i}`).join("\n");
  // The values each line had before the user typed over it, for undo.
  const lineStack = new Map<string, Map<number, string[]>>(names.map((n) => [n, new Map()]));
  const net: Pending[] = [];
  let now = 0;
  const timers: { at: number; fn: () => void; live: boolean; owner: "a" | "g" }[] = [];
  /** Every sync whose tab was closed, whether or not the pane still tracks it. */
  const closedSyncs = new Set<SyncLike>();
  const saveBurst = new Map<string, { at: number; n: number }>();
  /** The daemon's write path fails (5xx) while reads succeed. */
  let writesBroken = false;
  let flushing = false;
  const saves: { doc: string; text: string; bufferHadMarkers: boolean }[] = [];
  let seq = 0;
  let violation: Violation | null = null;
  const trace: string[] = [];
  const fail = (invariant: string, detail: string): void => {
    violation ??= { invariant, detail, trace };
  };

  // What a user typed, per doc and line, and what the agent last wrote there.
  const typed = new Map<string, Map<number, string>>(names.map((n) => [n, new Map()]));
  const excused = new Set<string>(); // tokens knowingly discarded by "take theirs"

  const syncs = new Map<string, SyncLike>();
  /** Tabs that closed with a save still in flight, per doc (the pane keeps this map). */
  const closing = new ClosingTabs<Closable>();
  const editor = { doc: "" as string, text: "" };
  let attached = "";
  const events: { doc: string; commit: string }[] = [];
  let outage = false;

  const call = <T>(run: () => T | Promise<T>, owner: "a" | "g" = "a", held = false): Promise<T> =>
    new Promise<T>((resolve, reject) => {
      net.push({ owner, stage: held ? "held" : "sent", hang: held, run, resolve: resolve as (v: unknown) => void, reject });
    });

  const httpErr = (status: number, message: string): Error => Object.assign(new Error(message), { status });

  function newSync(doc: string, storage: () => DraftStorage = () => storageA, register = true, synthetic?: () => { text: string; commit: string } | null): SyncLike {
    const isGhost = !register;
    const owner: "a" | "g" = isGhost ? "g" : "a";
    const d = daemons.get(doc)!;
    // The pane's own storage: a pane that closes keeps writing to it while its syncs
    // finish, whatever page is open by then.
    const st = storage();
    let self: SyncLike | null = null; // the sync these deps belong to, not whatever is current for the doc
    const deps: SyncDepsLike = {
      load: async (initial) => {
        // A reopened tab loads only after the closing tab's final save has landed.
        const priors = initial && self ? closing.priors(doc, self as Closable) : [];
        await Promise.all(priors.map((p) => p.whenQuiet()));
        const stored = initial && synthetic ? synthetic() : null;
        if (stored) return stored;
        return call(() => {
          if (d.down > 0) {
            d.down--;
            throw new Error("connect ECONNREFUSED");
          }
          if (d.deleted) throw httpErr(404, "404 doc not found");
          return d.read();
        }, owner);
      },
      save: (text, base, final, fence) => {
        saves.push({ doc, text, bufferHadMarkers: self ? hasConflictMarkers(self.text) : false });
        void final;
        // I12: once a newer tab has loaded the doc, a tab that closed on it writes no more.
        const current = syncs.get(doc);
        if (!isGhost && self && current && current !== self && current.text !== "" && closedSyncs.has(self)) {
          fail("closed-tab-wrote-after-reopen", `${doc}: a closed tab sent a save after the doc was reopened and loaded`);
        }
        // I10: a sync that knows its doc is gone sends nothing.
        if (self && self.currentStatus === "gone") fail("saved-while-gone", `${doc}: a deleted doc's sync sent a save`);
        if (process.env.AF_STASHLOG || (process.env.AF_KVLOG && text.includes(process.env.AF_KVLOG))) console.log("SAVE", isGhost ? "ghost" : "A", doc, JSON.stringify(text), base, "final=" + final);
        // I18: a save that keeps failing is retried with a delay, not in a loop at one instant.
        // (A flush is the user asking to save now: the pane's Send now. It is the user's loop, not the engine's.)
        const burst = saveBurst.get(`${doc}${owner}`);
        if (flushing) saveBurst.delete(`${doc}${owner}`);
        else if (burst && burst.at === now) burst.n++;
        else saveBurst.set(`${doc}${owner}`, { at: now, n: 1 });
        if ((saveBurst.get(`${doc}${owner}`)?.n ?? 0) > 12) fail("save-loop-without-backoff", `${doc}: ${saveBurst.get(`${doc}${owner}`)!.n} saves at one instant`);
        const faulty = rand() < (o.faults ?? 0);
        const kinds = o.chaos ? (["drop", "lose-reply", "refuse", "hang-applied", "hang-lost"] as const) : (["drop", "lose-reply"] as const);
        const fault = nextSaveFault ?? (writesBroken ? "drop" : faulty ? pick(kinds) : null);
        nextSaveFault = null;
        const pending = call(() => {
          if (d.down > 0) {
            d.down--;
            throw new Error("connect ECONNREFUSED");
          }
          if (fault === "drop") throw httpErr(503, "503 service unavailable");
          if (fault === "refuse") throw httpErr(413, "413 doc is larger than the limit");
          if (fault === "hang-lost") return new Promise<never>(() => undefined);
          if (fault === "drop-late" && !process.env.AF_NO_DROP_LATE) {
            // The connection drops before the reply, and the daemon (which does not cancel a save when its
            // client leaves) commits the request some seconds later.
            const epoch = d.epoch;
            timers.push({
              at: now + 5000, // (a fixed delay: this event takes nothing from the seeded stream)
              live: true,
              owner,
              fn: () => {
                if (d.epoch !== epoch) return; // the doc was deleted and added again since: that request died with the old doc
                try {
                  const r = d.save(text, base, fence);
                  if (r.kind === "saved" && !outage) events.push({ doc, commit: r.commit });
                } catch {
                  // refused (stale or gone): nothing committed
                }
              },
            });
            throw new Error("Failed to fetch");
          }
          const result = d.save(text, base, fence);
          if (fault === "lose-reply") throw new Error("network error: reply lost");
          if (fault === "hang-applied") return new Promise<never>(() => undefined);
          return result;
        }, owner, fault === "slow");
        return pending;
      },
      setBuffer: (text) => {
        if (isGhost) {
          if (ghostBox.cur) ghostBox.cur.text = text;
        } else if (attached === doc) {
          editor.text = text;
        }
      },
      onStatus: () => undefined,
      setTimer: (fn, ms) => {
        const t = { at: now + ms, fn, live: true, owner };
        timers.push(t);
        return t;
      },
      clearTimer: (h) => {
        (h as { live: boolean }).live = false;
      },
      draft: {
        load: () => st.load(doc),
        store: (x) => st.store(doc, x),
        clear: () => st.clear(doc),
      },
    };
    const s = o.makeSync(deps);
    const rawFlush = s.flush.bind(s);
    s.flush = () => {
      flushing = true;
      try {
        rawFlush();
      } finally {
        flushing = false;
      }
    };
    if (s.whenSaved) {
      const rawWhenSaved = s.whenSaved.bind(s);
      s.whenSaved = () => {
        flushing = true; // Send now asks for a save right away, too
        try {
          return rawWhenSaved();
        } finally {
          flushing = false;
        }
      };
    }
    self = s;
    storageOf.set(s, st);
    if (register) syncs.set(doc, s);
    return s;
  }

  async function open(doc: string): Promise<void> {
    let s = syncs.get(doc);
    const prev = attached ? syncs.get(attached) : undefined;
    if (prev && attached !== doc) {
      if (prev.detach) prev.detach();
      else {
        prev.dispose();
        syncs.delete(attached);
      }
    }
    attached = doc;
    if (s) {
      if (s.attach) editor.text = s.attach().text;
      else editor.text = s.text;
      editor.doc = doc;
      return;
    }
    s = newSync(doc);
    editor.doc = doc;
    editor.text = "";
    const fresh = s;
    void s.start().then(() => {
      // The pane's rule: once the new tab has loaded and restored the stored draft, the
      // tabs that closed on this doc are redundant writers and are abandoned (draft kept).
      if (process.env.AF_NO_ABANDON) return; // (the pane before it abandoned them, for the failing run)
      // The pane's own bookkeeping (AF_NO_OPEN_GUARD: a loader closed meanwhile takes over too).
      closing.takeOver(doc, fresh as Closable, !!process.env.AF_NO_OPEN_GUARD || syncs.get(doc) === fresh);
    }).catch(() => undefined);
  }

  const lineSets = o.collide ? [0, 1, 2, 3, 4] : [0, 1];
  const agentLines = o.collide ? [0, 1, 2, 3, 4] : [3, 4];

  /** Forces the next save to fail in a given way (for scripted events). */
  let nextSaveFault: "lose-reply" | "hang-applied" | "slow" | "drop-late" | null = null;

  function userEdit(forced?: number): number | null {
    const s = syncs.get(attached);
    if (!s || !isLoaded(s) || s.ended) return null; // an ended tab is dropped by the pane
    const line = forced ?? pick(lineSets);
    const token = `u${++seq}`;
    const lines = editor.text.split("\n");
    if (lines.length !== LINES) return null; // the buffer holds conflict markers: leave it to the resolver
    const stackFor = lineStack.get(attached)!;
    stackFor.set(line, [...(stackFor.get(line) ?? []), lines[line]!]);
    lines[line] = token;
    editor.text = lines.join("\n");
    typed.get(attached)!.set(line, token);
    s.edit(editor.text);
    if (s.text !== editor.text) fail("edit-ignored", `${attached}: the editor shows ${JSON.stringify(editor.text)} but the sync holds ${JSON.stringify(s.text)} (status ${s.currentStatus}, ${s.debugModel})`);
    return line;
  }

  /** The user undoes a line back to what it was before they typed over it. */
  function userUndo(forced?: number): void {
    const s = syncs.get(attached);
    if (!s || !isLoaded(s) || s.ended) return;
    const stackFor = lineStack.get(attached)!;
    const candidates = [...stackFor.entries()].filter(([, v]) => v.length > 0).map(([k]) => k);
    if (!candidates.length) return;
    const line = forced !== undefined && candidates.includes(forced) ? forced : pick(candidates);
    const prev = stackFor.get(line)!.pop()!;
    const lines = editor.text.split("\n");
    if (lines.length !== LINES) return;
    lines[line] = prev;
    editor.text = lines.join("\n");
    typed.get(attached)!.set(line, prev);
    s.edit(editor.text);
  }

  /** The doc is deleted while it has unsaved edits, then added again from its
   *  template. The old sync must stop: no retry may write into the new doc. */
  async function deleteAndReadd(doc: string): Promise<void> {
    const s = syncs.get(doc);
    const olds = [...(closing.get(doc) ?? [])]; // tabs that closed earlier and may still be retrying
    if (!s && !olds.length) return;
    const d = daemons.get(doc)!;
    for (const x of [s, ...olds]) {
      if (!x) continue;
      if (x.suspend) x.suspend();
      else x.dispose(); // an engine without suspend: the old pane just disposed and deleted
    }
    // The delete can fail (the daemon refuses, or is down): the doc stays, and the work
    // typed before the delete went out is owed to the tab that reopens it.
    const failing = !!o.chaos && rand() < 0.4;
    // The editor is read-only while the delete is in flight, so nothing is typed. (The
    // pane before that took keystrokes the suspended sync ignored.)
    if (process.env.AF_EDIT_DURING_DELETE && attached === doc && isLoaded(s!)) {
      const lines = editor.text.split("\n");
      if (lines.length === LINES) {
        const token = `u${++seq}`;
        lines[0] = token;
        editor.text = lines.join("\n"); // the editor takes it; the pane's sync is null and never hears of it
        typed.get(doc)!.set(0, token);
      }
    }
    if (!failing) d.deleted = true;
    await advance(1000);
    await drain();
    if (failing) {
      for (const x of [s, ...olds]) x?.abandon?.(true);
      syncs.delete(doc);
      closing.delete(doc);
      if (attached === doc) {
        attached = "";
        editor.text = "";
      }
      await open(doc);
      await drain();
      return;
    }
    for (const x of [s, ...olds]) x?.abandon?.();
    syncs.delete(doc);
    closing.delete(doc);
    if (attached === doc) {
      attached = "";
      editor.text = "";
    }
    typed.get(doc)!.clear();
    lineStack.get(doc)!.clear();
    storageA.clear(doc);
    const savesAfterDelete = saves.length;
    d.readd(initialText(doc));
    await advance(30_000);
    await drain();
    const late = saves.slice(savesAfterDelete).filter((x) => x.doc === doc);
    // I7: a deleted doc's sync stops retrying, so nothing is written into the new doc.
    if (late.length) fail("deleted-doc-sync-kept-saving", `${doc}: ${late.length} save(s) after the delete, text ${JSON.stringify(late[0]!.text)}`);
    if (d.text !== initialText(doc)) fail("deleted-doc-overwrote-new-doc", `${doc}: new doc holds ${JSON.stringify(d.text)}`);
    await open(doc);
    await drain();
  }

  function isLoaded(s: SyncLike): boolean {
    return editor.text !== "" || s.text !== "";
  }

  function agentEdit(): void {
    const doc = pick(names);
    const d = daemons.get(doc)!;
    const commit = d.setLine(pick(agentLines), `a${++seq}`);
    if (!outage) events.push({ doc, commit });
  }

  /** The page goes into the back-forward cache: its pagehide releases the drafts, another window
   *  adopts them and resolves its copy, and the page comes back. What it held unsaved must be in
   *  storage again, because its sync stashed it once and will not store it a second time.
   *  (AF_NO_BFCACHE_REVIVE is the pane that only refreshes the entries that still exist.) */
  function bfcacheReturn(adopted = false, stall = false): void {
    if (adopted) {
      // The page holds these as drafts it adopted from a closed window, not ones it stored: the
      // sync restored them without storing them again.
      for (const doc of names) {
        const key = `af.docs.draft\ns\nplan\n${doc}\n`;
        const raw = kvMap.get(key + storageA.owner);
        if (!raw) continue;
        const e = JSON.parse(raw) as { owner: string; released: boolean };
        kvMap.delete(key + storageA.owner);
        kvMap.set(key + "wGONE", JSON.stringify({ ...e, owner: "wGONE", released: true }));
        storageA.load(doc);
      }
    }
    const mine = names.flatMap((doc) => {
      const own = kvMap.get(`af.docs.draft\ns\nplan\n${doc}\n${storageA.owner}`);
      return own ? [{ doc, buffer: (JSON.parse(own) as { draft: Draft }).draft.buffer }] : [];
    });
    if (!mine.length) return;
    if (stall) {
      // (Entries a closed page left are another window's to adopt first; leave the run alone while any remain.)
      if (ghostBox.cur) return; // a live second window would look stale too, and its draft is its own
      if ([...kvMap.keys()].some((k) => closedOwners.has(k.split("\n").pop()!))) return;
      // No pagehide: the page's heartbeat was throttled for longer than the staleness window, so
      // its entries look dead (and are not released).
      for (const doc of names) {
        const k = `af.docs.draft\ns\nplan\n${doc}\n${storageA.owner}`;
        const raw = kvMap.get(k);
        if (raw) kvMap.set(k, JSON.stringify({ ...(JSON.parse(raw) as object), at: now, released: false }));
      }
      now += DRAFT_STALE_MS + 1;
    } else storageA.release();
    const other = new DraftStorage(kv, `wBF${++winSeq}`, () => now, "s\nplan");
    for (const { doc } of mine) {
      if (other.load(doc)) other.clear(doc);
    }
    if (process.env.AF_NO_BFCACHE_REVIVE) storageA.touch();
    else if (stall) storageA.touch();
    else storageA.revive();
    for (const { doc, buffer } of mine) {
      if (!stashedBuffers(doc).includes(buffer)) fail("bfcache-draft-lost", `${doc}: the page came back from the back-forward cache without ${JSON.stringify(buffer)} in storage`);
    }
  }

  async function advance(ms: number): Promise<void> {
    now += ms;
    // Windows that are alive refresh their drafts (the heartbeat).
    storageA.touch();
    if (ghostBox.cur) ghostStorage.touch();
    for (;;) {
      const due = timers.filter((t) => t.live && t.at <= now).sort((a, b) => a.at - b.at)[0];
      if (!due) break;
      due.live = false;
      due.fn();
      await tick();
    }
  }

  async function netStep(): Promise<void> {
    const sent = net.filter((p) => p.stage === "sent");
    const processed = net.filter((p) => p.stage === "processed");
    if (sent.length && (rand() < 0.5 || !processed.length)) {
      const p = pick(sent);
      runPending(p);
    } else if (processed.length) {
      const p = pick(processed);
      deliver(p);
    }
    await tick();
  }

  /** The daemon handles a request. A reply that never comes marks it hung. */
  function runPending(p: Pending): void {
    p.stage = "processed";
    try {
      const v = p.run();
      if (v instanceof Promise) {
        p.hang = true;
        p.result = { ok: true, value: undefined };
      } else {
        p.result = { ok: true, value: v };
      }
    } catch (e) {
      p.result = { ok: false, error: e as Error };
    }
  }

  /** The reply reaches the client. A hung request stays outstanding until the page closes. */
  function deliver(p: Pending): void {
    if (p.hang) return;
    net.splice(net.indexOf(p), 1);
    if (p.result!.ok) p.resolve(p.result!.value);
    else p.reject(p.result!.error);
  }

  async function drain(): Promise<void> {
    for (let i = 0; i < 400 && net.some((x) => !x.hang || x.stage === "sent"); i++) {
      for (const p of net.filter((x) => x.stage === "sent")) runPending(p);
      for (const p of net.filter((x) => x.stage === "processed")) deliver(p);
      await tick();
    }
  }

  /** The page closes: every sync gets its pagehide, requests in flight reach the
   *  daemon or not, replies are lost, and only per-viewer storage and the daemon
   *  survive. The user then opens the page again on the same doc. */
  async function pageClose(): Promise<void> {
    if (process.env.AF_STASHLOG) console.log("PAGECLOSE");
    const doc = attached;
    if (!doc) return;
    // pagehide: the pane's drafts are released first (a listener registered before the
    // panes'), and the final saves that follow write released entries.
    closedOwners.add(storageA.owner);
    storageA.release();
    for (const s of syncs.values()) s.saveBeforeLeaving();
    await tick();
    for (const p of net.filter((x) => x.stage === "sent" && x.owner === "a")) {
      if (rand() < 0.5) {
        try {
          p.run();
        } catch {
          // The daemon rejected or the reply would have failed: the page is gone either way.
        }
      }
    }
    for (const p of net.filter((x) => x.owner === "a")) net.splice(net.indexOf(p), 1);
    for (const t of timers) if (t.owner === "a") t.live = false;
    events.length = 0;
    for (const s of syncs.values()) s.dispose();
    syncs.clear();
    closing.clear();
    attached = "";
    editor.text = "";
    // The window is gone: it releases its drafts; the page that opens next is a new
    // window with a new id, and adopts them.
    if (ghostBox.cur) pageWorkTransferred = true;
    // (A page that unloads is released at pagehide, and nothing closes it after: the final
    // saves' writes must stay released on their own.)
    const oldOwner = storageA.owner;
    storageA = new DraftStorage(kv, `wA${++winSeq}`, () => now, "s\nplan");
    const unsaved = [...typed.get(doc)!.values()].filter((tok) => !daemons.get(doc)!.everHad(tok) && !excused.has(tok));
    // Storage may be full as the new page adopts the old one's draft: the copy does not
    // fit, and the original must stay.
    storageFull = !!o.chaos && rand() < 0.3;
    const fullAtReopen = storageFull;
    // The pane lists the daemon's docs, so a doc the daemon no longer has is not opened:
    // the unsaved work stored for it comes back as a deleted doc instead.
    if (daemons.get(doc)!.deleted) {
      recoverStoredGone();
      for (let i = 0; i < 5; i++) await tick();
      await drain();
      storageFull = false;
      // I13: work stored for a doc the daemon no longer has is reachable: the new pane
      // shows it as a deleted doc, to restore or discard. Stored and shown nowhere is lost.
      const d = daemons.get(doc)!;
      const stored = stashedBuffers(doc).join("\n").split("\n");
      const stranded = [...typed.get(doc)!.values()].filter((tok) => !excused.has(tok) && !d.everHad(tok) && stored.includes(tok));
      if (stranded.length && syncs.get(doc)?.currentStatus !== "gone") {
        fail("stored-draft-of-deleted-doc-unreachable", `${doc}: ${stranded.join(",")} is stored for a doc the daemon no longer has, and the reopened pane shows ${syncs.get(doc)?.currentStatus ?? "nothing"}`);
      }
      for (let i = 0; i < 3; i++) {
        await advance(6000);
        await drain();
      }
      return;
    }
    await open(doc);
    await drain();
    recoverStoredGone();
    storageFull = false;
    // I19: the page that opens next takes over what the closed page left for this doc (its
    // entry is released, however late its final save wrote it), unless storage was full.
    const left = [...kvMap.keys()].filter((k) => k.startsWith(`af.docs.draft\ns\nplan\n${doc}\n`) && k.endsWith(`\n${oldOwner}`));
    if (!fullAtReopen && left.length && syncs.get(doc) && isLoaded(syncs.get(doc)!)) fail("closed-page-draft-not-adopted", `${doc}: the closed page's draft was left behind when the page reopened ${JSON.stringify(left)} all=${JSON.stringify([...kvMap.entries()].map(([k,v])=>[k.split("\n").slice(-2).join("/"), (JSON.parse(v) as {released:boolean,at:number}).released, (JSON.parse(v) as {at:number}).at]))} now=${now}`);
    const back = syncs.get(doc)!;
    // I6: unsaved edits that survive only in storage come back, and say so.
    if (unsaved.length && isLoaded(back) && !["recovered", "conflict", "saving", "saved", "merged"].includes(back.currentStatus)) {
      fail("recovered-draft-not-shown", `${doc}: reopened as ${back.currentStatus} with unsaved ${unsaved.join(",")}`);
    }
    if (unsaved.length && isLoaded(back) && back.currentStatus === "pending") {
      fail("recovered-draft-not-shown", `${doc}: reopened as pending, not recovered`);
    }
    for (let i = 0; i < 3; i++) {
      await advance(6000);
      await drain();
    }
  }

  /** A newly opened pane finds stored drafts of docs the daemon no longer has, and
   *  opens each as a deleted doc from its draft. (AF_NO_RECOVER_GONE is the pane that
   *  never looked.) */
  function recoverStoredGone(): void {
    if (process.env.AF_NO_RECOVER_GONE) return;
    for (const doc of storageA.docs()) {
      if (!daemons.get(doc)?.deleted || syncs.has(doc)) continue;
      const pane = storageA;
      const s = newSync(doc, () => pane, true, () => {
        const stored = pane.peek(doc);
        return stored ? { text: stored.base, commit: "" } : null;
      });
      void s.start().then(() => s.remoteDeleted?.()).catch(() => undefined);
    }
  }

  /** Manual mode's "Send now": it must not go out while a newer edit is unsaved.
   *  The pane flushes every doc and sends only when all have landed. */
  function sendNow(): void {
    const want = new Map<string, string[]>(
      names.map((n) => [n, [...typed.get(n)!.values()]]),
    );
    const live = [...syncs.entries()];
    const check = (doc: string): void => {
      if (o.chaos) return; // other windows and deletes also write; the property is checked without them
      const d = daemons.get(doc)!;
      for (const tok of want.get(doc) ?? []) {
        if (excused.has(tok)) continue;
        // A later keystroke on the same line supersedes it: that one is not owed.
        if (![...typed.get(doc)!.values()].includes(tok)) continue;
        if (!d.everHad(tok)) fail("send-now-sent-unsaved-edit", `${doc}: ${tok} was typed before Send now but is not in the daemon (daemon ${JSON.stringify(d.text)}, pane ${JSON.stringify(syncs.get(doc)?.text)}, status ${syncs.get(doc)?.currentStatus}, verdict-path ${syncs.get(doc)?.whenSaved ? "whenSaved" : "none"})`);
      }
    };
    for (const [doc, sync] of live) {
      if (!sync.whenSaved) {
        check(doc); // an engine that cannot flush sends at once
        continue;
      }
      void sync.whenSaved().then((verdict) => {
        if (verdict === "saved") check(doc);
      });
    }
  }

  // --- the events only a chaotic daemon and a second window produce -----------

  /** A second window on `goals`: its own sync, editor and draft storage, sharing the
   *  daemon and localStorage. */
  const ghostBox: { cur: { sync: SyncLike; text: string } | null } = { cur: null };
  const ghostTyped = new Map<number, string>();
  const ghostTokens = new Set<string>();
  /** The second window closed with unsaved work: any window may now adopt and change it. */
  let ghostWorkTransferred = false;
  /** The page closed while the second window was open: it may have adopted the page's
   *  released draft of `goals`, and that window's user is free to change it. */
  let pageWorkTransferred = false;

  const ghostText = (): string => (ghostBox.cur ? ghostBox.cur.sync.text : "");

  function ghostOpen(): void {
    if (ghostBox.cur) return;
    if (process.env.AF_STASHLOG) console.log("GHOSTOPEN now=" + now);
    const sync = newSync("goals", () => ghostStorage, false);
    ghostBox.cur = { sync, text: "" };
    void sync.start().then(() => {
      if (ghostBox.cur && ghostBox.cur.sync === sync) ghostBox.cur.text = sync.text;
      if (process.env.AF_STASHLOG) console.log("GHOST LOADED");
    }).catch((e) => { if (process.env.AF_STASHLOG) console.log("GHOST LOAD FAILED", String(e)); });
  }

  function ghostEdit(): void {
    const g = ghostBox.cur;
    // (The second window's user does not type over work it just recovered: that is a
    // choice, and would make the work that window adopted untraceable.)
    if (!g || g.sync.text === "" || g.sync.ended || g.sync.currentStatus === "recovered" || g.sync.currentStatus === "conflict") return;
    const lines = g.sync.text.split("\n");
    if (lines.length !== LINES) return;
    const line = pick([0, 1, 2, 3, 4]);
    const token = `g${++seq}`;
    lines[line] = token;
    ghostTyped.set(line, token);
    ghostTokens.add(token);
    g.sync.edit(lines.join("\n"));
  }

  async function ghostClose(): Promise<void> {
    const g = ghostBox.cur;
    if (!g) return;
    const wasConflict = g.sync.currentStatus === "conflict";
    if (g.sync.currentStatus !== "saved" && g.sync.currentStatus !== "merged") ghostWorkTransferred = true;
    const before = g.sync.text;
    g.sync.dispose();
    closedOwners.add(ghostStorage.owner);
    ghostStorage.close();
    ghostStorage = newGhostStorage();
    ghostBox.cur = null;
    await tick();
    if (wasConflict) {
      // The ghostBox.cur's draft is its own: window A cannot have cleared it.
      const kept = stashedBuffers("goals").includes(before) || ghostStorage.load("goals")?.buffer === before;
      if (!kept && before !== daemons.get("goals")!.text) fail("window-draft-cleared-by-another-window", `ghostBox.cur conflict draft ${JSON.stringify(before)} is gone`);
    }
  }

  async function ghostResolve(): Promise<void> {
    const g = ghostBox.cur;
    if (!g || g.sync.currentStatus !== "conflict") return;
    await (pick([0, 1]) === 0 ? g.sync.keepMine() : g.sync.showBoth());
  }

  function remoteDelete(): void {
    const doc = pick(names);
    const d = daemons.get(doc)!;
    if (d.deleted) return;
    d.deleted = true;
    // What was already saved goes with the deleted doc; only unsaved work is owed.
    for (const tok of typed.get(doc)!.values()) if (d.everHad(tok)) excused.add(tok);
    // Sometimes the tab is told (the doc list refresh); sometimes it learns from a 404.
    if (rand() < 0.5) {
      syncs.get(doc)?.remoteDeleted?.();
      if (doc === "goals") ghostBox.cur?.sync.remoteDeleted?.();
    }
  }

  /** The user types, the doc is deleted under them, and the page closes before anything
   *  is restored or discarded: the work must come back when the page reopens. */
  async function deleteThenClose(): Promise<void> {
    const doc = attached;
    if (!doc || daemons.get(doc)!.deleted || userEdit() === null) return;
    const d = daemons.get(doc)!;
    d.deleted = true;
    for (const tok of typed.get(doc)!.values()) if (d.everHad(tok)) excused.add(tok);
    if (rand() < 0.5) syncs.get(doc)?.remoteDeleted?.();
    await pageClose();
  }

  function remoteReadd(): void {
    for (const doc of names) {
      const d = daemons.get(doc)!;
      if (!d.deleted) continue;
      d.readd(initialText(doc));
      return;
    }
  }

  async function reviveGone(): Promise<void> {
    for (const doc of names) {
      const d = daemons.get(doc)!;
      const all: SyncLike[] = [...(syncs.has(doc) ? [syncs.get(doc)!] : []), ...(doc === "goals" && ghostBox.cur ? [ghostBox.cur.sync] : [])];
      for (const s of all) {
        if (s.currentStatus !== "gone") continue;
        if (d.deleted) d.readd(initialText(doc));
        void s.revive?.().catch(() => undefined); // its read needs the network: drained by the caller
      }
    }
  }

  function daemonRestart(): void {
    // Requests in flight are lost with the daemon; for a while it refuses new ones.
    for (const p of net) {
      if (p.stage === "sent" && rand() < 0.5) runPending(p);
    }
    for (const p of net.splice(0)) {
      if (p.stage === "processed" && !p.hang) p.reject(new Error("connect ECONNRESET"));
      else p.reject(new Error("connect ECONNRESET"));
    }
    for (const d of daemons.values()) d.down = 2 + Math.floor(rand() * 4);
  }

  function rollbackHead(): void {
    const doc = pick(names);
    const d = daemons.get(doc)!;
    if (d.deleted) return;
    const c = d.rollback();
    if (c && !outage) events.push({ doc, commit: c });
  }

  /** The user types, the connection drops before the reply (the browser only says fetch failed), the user
   *  undoes the line, and the daemon, which does not cancel a save when its client leaves, commits the
   *  request seconds later. The undo must stand over the landed save.
   *  (AF_NO_DROP_LATE leaves the request uncommitted, which is what an engine that trusts a rejection assumes.) */
  async function dropThenLand(): Promise<void> {
    const s = syncs.get(attached);
    if (!s || !isLoaded(s) || s.ended) return;
    if (userEdit(0) === null) return;
    const landed = editor.text.split("\n")[0];
    nextSaveFault = "drop-late";
    s.flush();
    await tick();
    for (const p of net.filter((x) => x.stage === "sent" && x.owner === "a")) {
      runPending(p);
      break;
    }
    for (const p of net.filter((x) => x.stage === "processed" && !x.hang && x.owner === "a")) {
      deliver(p);
      break;
    }
    await tick();
    userUndo(0);
    const doc = attached;
    const want = editor.text.split("\n")[0];
    // The request commits, its change event reaches the tab, and everything settles.
    await advance(6000);
    while (events.length) deliverEvent();
    await drain();
    await advance(12_000);
    await drain();
    const now0 = syncs.get(doc);
    if (!now0 || now0 !== s || !isLoaded(s) || s.currentStatus === "conflict" || s.currentStatus === "gone" || hasConflictMarkers(s.text)) return;
    const shown = (attached === doc ? editor.text : s.text).split("\n")[0];
    // (An agent edit of the same line may legitimately win the merge; the landed save's own text must not come back.)
    if (landed !== want && shown === landed && s.text.split("\n")[0] === landed) fail("undo-overwritten-by-landed-save", `${doc}: the user undid line 0 from ${JSON.stringify(landed)} to ${JSON.stringify(want)}, and after the dropped save landed the pane shows ${JSON.stringify(shown)} again`);
  }

  /** The user types, the save lands but its reply is lost, and the user changes their
   *  mind (undoes the line) before the tab has found out what happened. */
  async function ambiguousUndo(): Promise<void> {
    const s = syncs.get(attached);
    if (!s || !isLoaded(s) || s.ended) return;
    const line = userEdit();
    if (line === null) return;
    nextSaveFault = "lose-reply";
    s.flush();
    await tick();
    // The save reaches the daemon and the reply is lost; the tab's read of the head is
    // not answered yet. Now the user undoes.
    for (const p of net.filter((x) => x.stage === "sent" && x.owner === "a")) {
      runPending(p);
      break;
    }
    for (const p of net.filter((x) => x.stage === "processed" && !x.hang && x.owner === "a")) {
      deliver(p);
      break;
    }
    await tick();
    const doubted = typed.get(attached)!.get(line);
    userUndo(line);
    // I10: Send now waits on whenSaved(). It must not say "saved" while the daemon still
    // holds the save whose reply was lost, whatever the buffer shows: the pane would send
    // that text to the agent.
    const undone = syncs.get(attached);
    if (undone?.whenSaved && doubted !== undefined) {
      const doc = attached;
      void undone.whenSaved().then((v) => {
        if (v === "saved" && daemons.get(doc)!.text.split("\n")[line] === doubted && undone.text.split("\n")[line] !== doubted) {
          fail("saved-reported-while-daemon-holds-undone-text", `${doc} line ${line}: whenSaved said saved but the daemon still holds ${doubted}`);
        }
      });
    }
    const want = typed.get(attached)!.get(line);
    // Sometimes the page closes now, with the reply still lost, and the agent has edited
    // another line meanwhile: the draft's maybe-text is no longer the head, only contained
    // in it. (AF_REOPEN_EXACT_ONLY is the restore that needed an exact match.)
    if (!o.collide && rand() < 0.4) {
      agentEdit();
      await pageClose();
    }
    await drain();
    for (let i = 0; i < 2; i++) {
      await advance(6000);
      await drain();
    }
    // I9: the undo is not replaced by the save that may have landed. (Only checked
    // where nobody else writes: no collisions, no second window.)
    const after = syncs.get(attached);
    if (!o.collide && !ghostBox.cur && after && !after.ended && ["saved", "pending", "merged", "recovered", "saving"].includes(after.currentStatus)) {
      const got = after.text.split("\n")[line];
      if (got !== want) fail("ambiguous-save-overwrote-undo", `${attached} line ${line}: expected ${want}, the tab shows ${got} (status ${after.currentStatus})`);
    }
  }

  /** A save is in flight when the doc is deleted: the daemon answers it 404, and the reply
   *  is delayed. The tab learns of the delete from the doc list, the user restores the
   *  doc, and only then does the old 404 arrive: it must not mark the restored doc gone. */
  async function lateNotFound(): Promise<void> {
    const doc = attached;
    const s = syncs.get(doc);
    const d = daemons.get(doc)!;
    if (!doc || !s || !isLoaded(s) || s.ended || d.deleted || userEdit() === null) return;
    s.flush();
    await tick();
    const sent = net.filter((x) => x.stage === "sent" && x.owner === "a");
    if (!sent.length) return;
    d.deleted = true;
    d.down = 0; // the daemon is up: the restore's own read must be able to answer
    for (const p of sent) runPending(p); // the daemon answers 404; the replies are still on the wire
    s.remoteDeleted?.();
    await tick();
    if (s.currentStatus !== "gone") {
      d.readd(initialText(doc));
      return;
    }
    d.readd(initialText(doc));
    let revived = false; // its read can fail (the daemon is down): the doc then stays deleted, and the user tries again
    void s.revive?.().then(() => { revived = true; }, () => undefined);
    await tick();
    // The restore's read answers first (the one request sent after the doc came back)...
    for (const p of net.filter((x) => x.stage === "sent" && x.owner === "a")) {
      runPending(p);
      deliver(p);
    }
    await tick();
    await tick();
    // ...and only then do the old 404s arrive.
    for (const p of net.filter((x) => x.stage === "processed" && !x.hang && x.owner === "a")) deliver(p);
    await tick();
    await drain();
    // I15: a reply to a request from before the restore does not undo the restore.
    if (revived && s.currentStatus === "gone" && !d.deleted) fail("revived-doc-marked-gone-again", `${doc}: a 404 to a request from before the restore marked the restored doc deleted`);
  }

  /** A tab is closed while its first read is out, after the user's work was stored: the
   *  loader restores the draft and sends it, and a tab opened next must wait for that. */
  async function closeWhileLoading(): Promise<void> {
    const doc = attached;
    const s = syncs.get(doc);
    if (!doc || !s || !isLoaded(s) || s.ended || daemons.get(doc)!.deleted || userEdit() === null) return;
    const close = (x: SyncLike): void => {
      x.dispose();
      closedSyncs.add(x);
      closing.track(doc, x as Closable);
      syncs.delete(doc);
    };
    close(s);
    await tick();
    await open(doc); // a new tab: its read goes out
    const loading = syncs.get(doc);
    if (loading) close(loading); // closed before the read answered
    attached = "";
    editor.text = "";
    // The daemon answers some of what is out, in any order: the closed loader's read may
    // have arrived (it restores its draft and sends it) while its save is still out.
    for (let i = 0, n = 2 + Math.floor(rand() * 10); i < n; i++) {
      await netStep();
      await tick();
    }
    // I20: a closed tab that can still write stays tracked until it is quiet, wherever its
    // read had got to, so the tab opened next waits for it.
    const stillWriting = (): boolean => !!loading && loading.isQuiet === false && !closing.get(doc)?.has(loading as Closable);
    if (stillWriting()) fail("closed-loader-untracked", `${doc}: a tab closed while loading can still write, and is no longer tracked`);
    await open(doc); // and another, while the second one's save may still be out
    if (stillWriting()) fail("closed-loader-untracked", `${doc}: a tab closed while loading can still write, and is no longer tracked`);
    await drain();
  }

  /** The daemon's writes fail for a while (reads still work), then recover. */
  function toggleWriteOutage(): void {
    writesBroken = !writesBroken;
  }

  /** The doc list reaches the pane by several routes whose replies can arrive in any order:
   *  a refresh goes out, the user adds a doc (its own fetch goes out and is used), and the
   *  older refresh's reply, which lacks the new doc, arrives last. A doc missing from a list
   *  is taken as deleted, so the old reply must not be used. The roster code is the pane's. */
  const roster = new RosterOrder();
  /** The pane's use of a list reply: dropped if an older ticket than one used, else a doc it lacks is deleted. */
  function applyList(ticket: number, listed: string[]): void {
    if (!process.env.AF_NO_ROSTER_ORDER && !roster.accept(ticket)) return;
    for (const gone of missingFrom([...syncs.keys()], listed)) syncs.get(gone)?.remoteDeleted?.();
  }

  /** A doc is deleted under unsaved work (the tab keeps it), a list request goes out and is
   *  answered while it is gone, the user restores the doc, and only then does the list reply
   *  arrive: it lacks the restored doc, and must not mark it deleted again. (AF_NO_ROSTER_INVALIDATE
   *  is the pane before Restore doc invalidated the lists requested earlier.) */
  async function listAfterRestore(): Promise<void> {
    const doc = attached;
    const s = syncs.get(doc);
    const d = daemons.get(doc)!;
    if (!doc || !s || !isLoaded(s) || s.ended || d.deleted || userEdit() === null) return;
    d.deleted = true;
    d.down = 0;
    s.remoteDeleted?.();
    await tick();
    if (s.currentStatus !== "gone") {
      d.readd(initialText(doc));
      return;
    }
    const ticket = roster.begin();
    const staleList = names.filter((n) => !daemons.get(n)!.deleted); // answered while the doc was gone
    d.readd(initialText(doc)); // Restore doc adds it again...
    if (!process.env.AF_NO_ROSTER_INVALIDATE) roster.invalidate();
    let revived = false;
    void s.revive?.().then(() => { revived = true; }, () => undefined);
    await drain();
    await tick();
    applyList(ticket, staleList); // ...and only now does the older list arrive
    await tick();
    // I22: a list from before the restore does not mark the restored doc deleted again.
    if (revived && s.currentStatus === "gone" && !d.deleted) fail("restored-doc-marked-gone-by-old-list", `${doc}: a doc list from before Restore doc marked the restored doc deleted again`);
  }

  async function listReorder(): Promise<void> {
    const doc = "notes";
    if (!daemons.has(doc)) {
      daemons.set(doc, new Daemon(initialText(doc)));
      daemons.get(doc)!.deleted = true;
    }
    const d = daemons.get(doc)!;
    if (!d.deleted || syncs.has(doc)) return;
    const listNow = (): string[] => names.filter((n) => !daemons.get(n)!.deleted).concat(d.deleted ? [] : [doc]);
    const openDocs = (): string[] => [...syncs.keys()];
    const apply = (ticket: number, listed: string[]): void => {
      if (!process.env.AF_NO_ROSTER_ORDER && !roster.accept(ticket)) return;
      for (const gone of missingFrom(openDocs(), listed)) syncs.get(gone)?.remoteDeleted?.();
    };
    void applyList;
    const oldTicket = roster.begin(); // a refresh goes out...
    const oldList = listNow(); // ...and the daemon answers it before the doc exists
    const reorder = rand() < 0.8;
    if (!reorder) apply(oldTicket, oldList);
    d.readd(initialText(doc)); // the user adds the doc
    const newTicket = roster.begin();
    apply(newTicket, listNow());
    const s = newSync(doc); // and opens it, and types
    void s.start().catch(() => undefined);
    await drain();
    for (let i = 0; i < 5; i++) await tick();
    if (!isLoaded(s)) {
      s.dispose();
      syncs.delete(doc);
      return;
    }
    s.edit(`${s.text}\nu${++seq}`);
    if (reorder) apply(oldTicket, oldList); // the older reply arrives last
    await tick();
    // I21: a doc the daemon has is not marked deleted by an older list.
    if (s.currentStatus === "gone") fail("list-reply-marked-live-doc-deleted", `${doc}: an older doc list, arriving after the add, marked the new doc deleted`);
    s.dispose();
    syncs.delete(doc);
    await drain();
    for (let i = 0; i < 3; i++) {
      await advance(6000);
      await drain();
    }
    d.deleted = true; // the doc is not part of the scenario's other docs: the next run adds it afresh
    storageA.clear(doc);
  }

  /** A save is in flight, the user undoes back to the acknowledged text, and the doc is
   *  deleted before the reply: the deleted doc's tab keeps the undone buffer in storage,
   *  because the daemon may hold the text that was sent. */
  async function undoThenGone(): Promise<void> {
    const doc = attached;
    const s = syncs.get(doc);
    const d = daemons.get(doc)!;
    if (!doc || !s || !isLoaded(s) || s.ended || d.deleted) return;
    const line = userEdit();
    if (line === null) return;
    s.flush();
    await tick();
    if (!net.some((x) => x.stage === "sent" && x.owner === "a")) return;
    userUndo(line);
    d.deleted = true;
    s.remoteDeleted?.();
    await tick();
    if (s.currentStatus === "gone") {
      const owner = storageOf.get(s)!.owner;
      const stored = [...kvMap.keys()].some((k) => k.startsWith(`af.docs.draft\ns\nplan\n${doc}\n`) && k.endsWith(`\n${owner}`));
      // I17: what stays with a deleted doc stays in storage, an undo back to the old text included.
      if (!stored) fail("gone-doc-work-not-stored", `${doc}: the tab keeps work for a deleted doc but nothing is stored for it`);
    }
    d.readd(initialText(doc));
    await reviveGone();
    await drain();
  }

  /** A save takes longer than the client waits (30 s) to reach the daemon. The client gives
   *  up on it and reads the head, which has not changed; the user undoes; then the request
   *  lands, and the daemon's change event reaches the tab. The undo must stand. */
  async function lateLanding(): Promise<void> {
    const doc = attached;
    const s = syncs.get(doc);
    if (!doc || !s || !isLoaded(s) || s.ended || o.collide || ghostBox.cur || daemons.get(doc)!.deleted) return;
    const line = userEdit();
    if (line === null) return;
    nextSaveFault = "slow";
    s.flush();
    await tick();
    if (!net.some((x) => x.stage === "held" && x.owner === "a")) {
      nextSaveFault = null;
      return;
    }
    userUndo(line);
    const want = typed.get(doc)!.get(line);
    await advance(31_000); // the client's timeout fires; its read of the head runs and finds nothing changed
    await drain();
    // Sometimes the tab is closed now, a new tab opens, and the user edits in it before the
    // old request lands: the closed tab handed the doc over and must not write any more.
    const mode = rand();
    const reopened = mode < 0.35; // closed, reopened and edited before the old request lands
    const closedOnly = mode >= 0.35 && mode < 0.55; // closed; the doc is reopened only after it landed
    const quietReopen = mode >= 0.55 && mode < 0.8; // closed and reopened with no edit, before it lands
    if (reopened || closedOnly || quietReopen) {
      s.dispose();
      closedSyncs.add(s);
      closing.track(doc, s as Closable);
      syncs.delete(doc);
      attached = "";
      editor.text = "";
    }
    if (reopened || quietReopen) {
      await open(doc);
      await drain();
      if (reopened) userEdit();
    }
    for (const p of net.filter((x) => x.stage === "held" && x.owner === "a")) {
      p.stage = "sent";
      p.hang = false;
      runPending(p); // the request finally reaches the daemon, and lands
      deliver(p);
    }
    s.remoteChanged(daemons.get(doc)!.read().commit); // the daemon's change event
    syncs.get(doc)?.remoteChanged(daemons.get(doc)!.read().commit); // reaches the reopened tab too
    await drain();
    if (closedOnly) {
      // The closed tab's request landed, and no pane is open: the undo must still be
      // recoverable, and the pane that opens next must show it.
      await open(doc);
      await drain();
    }
    for (let i = 0; i < 3; i++) {
      await advance(6000);
      await drain();
    }
    // I16: a save that timed out and lands later does not overwrite the undo.
    const after = syncs.get(doc);
    if (!reopened && after && !after.ended && ["saved", "pending", "merged", "recovered", "saving"].includes(after.currentStatus)) {
      const got = after.text.split("\n")[line];
      if (got !== want) fail("late-landing-save-overwrote-undo", `${doc} line ${line}: expected ${want}, the tab shows ${got} (status ${after.currentStatus})`);
    }
  }

  /** The doc is deleted under unsaved work and the user discards it: nothing may write
   *  the discarded work back into storage, now or on any later event. */
  async function discardDeleted(): Promise<void> {
    const doc = attached;
    const s = syncs.get(doc);
    if (!doc || !s || !isLoaded(s) || s.ended || daemons.get(doc)!.deleted || userEdit() === null) return;
    const d = daemons.get(doc)!;
    d.deleted = true;
    s.remoteDeleted?.();
    await drain();
    if (s.currentStatus !== "gone") return;
    const owner = storageOf.get(s)!.owner;
    s.abandon?.(false);
    syncs.delete(doc);
    attached = "";
    editor.text = "";
    typed.get(doc)!.clear();
    lineStack.get(doc)!.clear();
    s.edit(s.text + "x"); // a late keystroke or event reaching the dead sync
    await tick();
    await advance(6000);
    await drain();
    const written = [...kvMap.keys()].filter((k) => k.startsWith(`af.docs.draft\ns\nplan\n${doc}\n`) && k.endsWith(`\n${owner}`));
    if (written.length) fail("discarded-draft-written-back", `${doc}: discarded work is in storage again (${written.length} entry)`);
  }

  /** The user types and the doc is deleted in the same breath, before any save. */
  async function deleteWhileTyping(): Promise<void> {
    const line = userEdit();
    if (line === null) return;
    const doc = attached;
    const d = daemons.get(doc)!;
    if (d.deleted) return;
    const owed = typed.get(doc)!.get(line)!;
    d.deleted = true;
    for (const tok of typed.get(doc)!.values()) if (d.everHad(tok)) excused.add(tok);
    if (rand() < 0.5) syncs.get(doc)?.remoteDeleted?.();
    // The deleting side re-adds the doc (a rename lands as a delete and an add), and the
    // user restores their work onto it. Nothing else happens meanwhile.
    for (let i = 0; i < 2; i++) {
      await advance(6000);
      await drain();
    }
    d.readd(initialText(doc));
    await reviveGone();
    for (let i = 0; i < 4; i++) {
      await drain();
      await advance(6000);
    }
    await drain();
    // I11: work that was unsaved when its doc was deleted is not lost, and reaches the new doc.
    const s = syncs.get(doc);
    if (s && !s.ended && !excused.has(owed)) {
      const have = [d.text, s.text, ...stashedBuffers(doc)].join("\n").split("\n").includes(owed);
      if (!have) fail("work-lost-with-deleted-doc", `${doc}: ${owed} typed before the delete is nowhere (daemon ${JSON.stringify(d.text)}, pane ${JSON.stringify(s.text)}, status ${s.currentStatus})`);
    } else if (!s || s.ended) {
      const have = [d.text, ...stashedBuffers(doc)].join("\n").split("\n").includes(owed);
      if (!have && !excused.has(owed)) fail("work-lost-with-deleted-doc", `${doc}: ${owed} typed before the delete is nowhere and its tab ended`);
    }
  }

  function deliverEvent(): void {
    if (!events.length) return;
    const e = events.splice(Math.floor(rand() * events.length), 1)[0]!;
    syncs.get(e.doc)?.remoteChanged(e.commit);
    if (e.doc === "goals") ghostBox.cur?.sync.remoteChanged(e.commit);
  }

  function mineOf(text: string): string {
    const out: string[] = [];
    let mode: "both" | "mine" | "theirs" = "both";
    for (const line of text.split("\n")) {
      if (line === CONFLICT_MINE) mode = "mine";
      else if (line === CONFLICT_SPLIT && mode === "mine") mode = "theirs";
      else if (line === CONFLICT_THEIRS) mode = "both";
      else if (mode !== "theirs") out.push(line);
    }
    return out.join("\n");
  }

  async function resolveOne(doc: string): Promise<void> {
    const s = syncs.get(doc);
    if (!s) return;
    if (s.currentStatus === "conflict") {
      const choice = pick(["keep", "both", "theirs"] as const);
      const wasAttached = attached === doc;
      if (choice === "keep") await s.keepMine();
      else if (choice === "both") await s.showBoth();
      else {
        for (const tok of typed.get(doc)!.values()) excused.add(tok);
        await s.takeTheirs();
      }
      await drainSoft();
      if (wasAttached && attached === doc && s.attach === undefined) editor.text = s.text;
    }
  }

  async function drainSoft(): Promise<void> {
    await tick();
    await netStepIfAny();
  }
  async function netStepIfAny(): Promise<void> {
    if (net.length) await netStep();
  }

  // Keep the editor equal to the attached sync after any step that may have
  // changed it through setBuffer (the harness editor is what setBuffer writes).
  function syncEditorFromEngine(): void {
    // The engine writes through setBuffer; nothing to do. Present for symmetry.
  }
  void syncEditorFromEngine;

  function checkEveryStep(): void {
    // I14: a pane that has closed owns no entry that looks alive: whatever its syncs write
    // while they finish, the pane that opens next can adopt at once.
    for (const [key, raw] of kvMap) {
      if (!closedOwners.has(key.split("\n").pop()!)) continue;
      if (!(JSON.parse(raw) as { released: boolean }).released) fail("closed-pane-entry-looks-live", `${key.split("\n").slice(-2).join("/")} was written after its pane closed, and is not released`);
    }
    const s = syncs.get(attached);
    if (!s || !isLoaded(s) || hasConflictMarkers(editor.text)) return;
    const lines = editor.text.split("\n");
    if (lines.length !== LINES) return;
    if (!o.collide && !o.chaos) {
      // I2: with no collisions, the editor always shows the latest keystroke of
      // every line the user typed, never an older version of it.
      for (const [line, tok] of typed.get(attached)!) {
        if (excused.has(tok)) continue; // the user chose "take theirs"
        if (lines[line] !== tok) {
          fail("editor-older-than-typed", `${attached} line ${line}: editor has ${lines[line]}, last typed ${tok} (status ${s.currentStatus}, sync text ${JSON.stringify(s.text)}, stash ${JSON.stringify(stashedBuffers(attached))})`);
        }
      }
    }
  }

  await open("goals");
  await drain();

  for (let step = 0; step < o.steps && !violation; step++) {
    // The late-landing interleavings are rare by chance, so they are reached on purpose at fixed steps.
    if (o.chaos && (o.faults ?? 0) > 0 && (step === 30 || step === 70 || step === 110)) await lateLanding();
    if (o.chaos && (o.faults ?? 0) > 0 && (step === 60 || step === 100) && !o.collide) await dropThenLand();
    // The page holds adopted drafts, then goes through the back-forward cache.
    if (o.chaos && (o.faults ?? 0) > 0 && (step === 40 || step === 80 || step === 120)) {
      bfcacheReturn(true);
    }
    // The heartbeat stalls past the staleness window, another window adopts the draft, and it resumes.
    if (o.chaos && (o.faults ?? 0) > 0 && (step === 50 || step === 90 || step === 130)) bfcacheReturn(false, true);
    const r = rand();
    trace.push(`step ${step} r=${r.toFixed(3)} attached=${attached} status=${syncs.get(attached)?.currentStatus} text=${JSON.stringify(syncs.get(attached)?.text)}`);
    if (r < 0.28) userEdit();
    else if (r < 0.4) agentEdit();
    else if (r < 0.5) await advance(Math.floor(rand() * 900));
    else if (r < 0.65) await netStep();
    else if (r < 0.72) deliverEvent();
    else if (r < 0.78) {
      const other = attached === "goals" ? "api" : "goals";
      const before = syncs.get(attached);
      const conflictBefore = before?.currentStatus === "conflict" ? before.text : null;
      const doc = attached;
      await open(other);
      await tick();
      // I3: an unresolved conflict draft survives a document switch.
      if (conflictBefore !== null && before?.detach) {
        await open(doc);
        const back = syncs.get(doc)!;
        if (back.currentStatus !== "conflict" || back.text !== conflictBefore) {
          fail("conflict-draft-lost-on-switch", `${doc}: status ${back.currentStatus}, text ${JSON.stringify(back.text)} vs ${JSON.stringify(conflictBefore)}`);
        }
      } else if (conflictBefore !== null && !before?.detach) {
        // An engine without detach loses the draft: model it by re-opening.
        await open(doc);
        const back = syncs.get(doc)!;
        await drain();
        if (back.currentStatus !== "conflict" || back.text !== conflictBefore) {
          fail("conflict-draft-lost-on-switch", `${doc}: reopened as ${back.currentStatus}`);
        }
      }
    } else if (r < 0.82) {
      // Close the tab (or navigate away) and open it again.
      const doc = attached;
      const s = syncs.get(doc);
      if (s) {
        if (process.env.AF_STASHLOG) console.log("CLOSEREOPEN", doc);
        const wasConflict = s.currentStatus === "conflict";
        const draft = s.text;
        const savesBefore = saves.length;
        s.dispose();
        closedSyncs.add(s);
        closing.track(doc, s as Closable);
        syncs.delete(doc);
        await tick();
        if (wasConflict && saves.length !== savesBefore) {
          fail("conflict-markers-written-on-leave", `closing ${doc} in conflict sent a save`);
        }
        attached = "";
        await open(doc);
        // The tab reopens on a stored draft while the first load is still in
        // flight: a reconnect or a remote change arrives before the load returns.
        if (rand() < 0.6) syncs.get(doc)?.reconnect?.();
        if (rand() < 0.4) syncs.get(doc)?.remoteChanged("");
        // A closed tab that is still retrying holds the load back until its save lands.
        for (let i = 0; i < 3; i++) {
          await drain();
          await advance(6000);
        }
        await drain();
        const back = syncs.get(doc)!;
        // A draft equal to what the daemon holds is no conflict: nothing is lost.
        const vacuous = draft === daemons.get(doc)!.text;
        // (A tab whose load failed has not restored yet; the draft must still be stored.)
        const notBack = isLoaded(back) ? back.currentStatus !== "conflict" || back.text !== draft : !stashedBuffers(doc).includes(draft);
        if (wasConflict && !vacuous && notBack) {
          fail("conflict-draft-lost-on-close", `${doc}: reopened as ${back.currentStatus}, draft ${JSON.stringify(draft)}, now ${JSON.stringify(back.text)}, daemon ${JSON.stringify(daemons.get(doc)!.text)}`);
        }
      }
    } else if (r < 0.845 && (o.faults ?? 0) > 0) {
      await pageClose();
    } else if (r < 0.855) {
      sendNow();
    } else if (r < 0.87 && (o.faults ?? 0) > 0) {
      userUndo();
    } else if (r < 0.875 && (o.faults ?? 0) > 0) {
      await deleteAndReadd(attached);
    } else if (o.chaos && r >= 0.93 && r < 0.9375) await deleteWhileTyping();
    else if (o.chaos && r >= 0.9375 && r < 0.945) await ambiguousUndo();
    else if (o.chaos && r >= 0.945 && r < 0.95) remoteDelete();
    else if (o.chaos && r >= 0.95 && r < 0.955) remoteReadd();
    else if (o.chaos && r >= 0.955 && r < 0.96) await reviveGone();
    else if (o.chaos && r >= 0.96 && r < 0.9615) daemonRestart();
    else if (o.chaos && r >= 0.9615 && r < 0.9625) await lateNotFound();
    else if (o.chaos && (o.faults ?? 0) > 0 && r >= 0.9625 && r < 0.9645) await deleteThenClose();
    else if (o.chaos && (o.faults ?? 0) > 0 && r >= 0.9645 && r < 0.9655) await lateLanding();
    else if (o.chaos && (o.faults ?? 0) > 0 && r >= 0.9655 && r < 0.966) await undoThenGone();
    else if (o.chaos && (o.faults ?? 0) > 0 && r >= 0.9660 && r < 0.9665) await closeWhileLoading();
    else if (o.chaos && r >= 0.9690 && r < 0.9693) await listReorder();
    else if (o.chaos && r >= 0.9693 && r < 0.9697) await listAfterRestore();
    else if (o.chaos && o.collide && r >= 0.9665 && r < 0.9690) rollbackHead();
    else if (o.chaos && !o.collide && r >= 0.9665 && r < 0.9690) await discardDeleted();
    else if (o.chaos && r >= 0.9697 && r < 0.9699) bfcacheReturn();
    else if (o.chaos && r >= 0.9695 && r < 0.9697) toggleWriteOutage();
    else if (o.chaos && r >= 0.9699 && r < 0.97) toggleWriteOutage();
    else if (o.chaos && r >= 0.97 && r < 0.98) {
      if (!ghostBox.cur) ghostOpen();
      else if (rand() < 0.5) ghostEdit();
      else await ghostResolve();
    } else if (o.chaos && r >= 0.98 && r < 0.99) ghostEdit();
    else if (o.chaos && r >= 0.99) await ghostClose();
    else if (r < 0.86) outage = !outage;
    else if (r < 0.89) {
      if (outage) outage = false;
      for (const s of syncs.values()) s.reconnect?.();
    } else if (r < 0.93) syncs.get(attached)?.flush();
    else if (o.collide) await resolveOne(pick(names));
    checkEveryStep();
  }

  // Quiesce: no more outages, deliver everything, resolve conflicts and markers,
  // reconnect, and let every timer and reload run to completion.
  outage = false;
  writesBroken = false;
  for (const d of daemons.values()) d.down = 0;
  for (const p of net.filter((x) => x.stage === "held")) {
    p.stage = "sent";
    p.hang = false;
  }
  // A request that never answers is given up on after five minutes.
  await advance(310_000); // timeouts fire...
  await drain();
  await advance(310_000); // ...and then the give-ups they armed
  await drain();
  for (let round = 0; round < 12 && !violation; round++) {
    if (o.chaos) {
      // Deleted docs are added again, and the kept work is carried onto them.
      remoteReadd();
      remoteReadd();
      await reviveGone();
      if (ghostBox.cur && ghostBox.cur.sync.currentStatus === "conflict") await ghostResolve();
      if (ghostBox.cur && hasConflictMarkers(ghostBox.cur.sync.text)) ghostBox.cur.sync.edit(mineOf(ghostBox.cur.sync.text));
      ghostBox.cur?.sync.reconnect?.();
      // A tab whose first load failed shows "Retry"; the user presses it.
      for (const [doc, s] of [...syncs]) {
        if (s.text !== "" && !s.ended) continue; // (a tab whose doc is gone ends; the pane drops it and the doc list reopens)
        s.dispose();
        syncs.delete(doc);
        const wasAttached = attached === doc;
        if (wasAttached) attached = "";
        await open(doc);
        if (!wasAttached) {
          const only = syncs.get(doc);
          if (only?.detach) only.detach();
        }
      }
      // A refused save is retried by editing again.
      for (const [doc, s] of syncs) {
        if (s.currentStatus !== "error") continue;
        const t = s.text;
        s.edit(`${t} `);
        s.edit(t);
        if (attached === doc) editor.text = t;
      }
      if (ghostBox.cur && (ghostBox.cur.sync.text === "" || ghostBox.cur.sync.ended)) {
        ghostBox.cur.sync.dispose();
        ghostBox.cur = null;
        ghostOpen();
      }
      if (ghostBox.cur && ghostBox.cur.sync.currentStatus === "error") {
        const t = ghostBox.cur.sync.text;
        ghostBox.cur.sync.edit(`${t} `);
        ghostBox.cur.sync.edit(t);
      }
    }
    await drain();
    while (events.length) deliverEvent();
    await advance(6000);
    await drain();
    for (const doc of names) {
      const s = syncs.get(doc);
      if (!s) continue;
      if (s.currentStatus === "conflict") await resolveOne(doc);
      await drain();
      if (hasConflictMarkers(s.text)) {
        const resolved = mineOf(s.text);
        if (attached === doc) editor.text = resolved;
        s.edit(resolved);
        await drain();
      }
    }
    for (const s of syncs.values()) s.reconnect?.();
    await drain();
    await advance(6000);
  }
  // A request that is still out is given up on five minutes after it timed out: let the
  // timeouts and then the give-ups run, and settle what they leave.
  for (let k = 0; k < 2; k++) {
    await advance(310_000);
    await drain();
    for (const s of syncs.values()) s.reconnect?.();
    await drain();
    await advance(6000);
    await drain();
  }
  await drain();
  await advance(6000);
  await drain();
  checkEveryStep();

  if (!violation) {
    for (const doc of names) {
      const s = syncs.get(doc);
      const d = daemons.get(doc)!;
      if (!s) continue;
      if (s.currentStatus === "conflict" || hasConflictMarkers(s.text)) continue; // unresolved by an engine that cannot
      const shown = attached === doc ? editor.text : s.text;
      const surfaces = [d.text, shown, s.text, ...stashedBuffers(doc), ghostText()].join("\n");
      // I1: no user keystroke is ever lost.
      for (const tok of typed.get(doc)!.values()) {
        if (excused.has(tok)) continue;
        if (pageWorkTransferred && doc === "goals") continue; // another window adopted this page's draft
        const kept = surfaces.split("\n").includes(tok) || ((o.collide || o.chaos) && d.everHad(tok));
        if (!kept) {
          fail("keystroke-lost", `${doc}: ${tok} is nowhere (daemon ${JSON.stringify(d.text)}, editor ${JSON.stringify(shown)})`);
        }
      }
      // I4: after a reconnect the pane matches the daemon.
      if (shown !== d.text && s.text !== d.text) {
        fail("pane-differs-from-daemon", `${doc}: pane ${JSON.stringify(s.text)} (status ${s.currentStatus}, closing ${[...(closing.get(doc) ?? [])].map((c) => `${c.currentStatus}/${String(c.isQuiet)}/${c.debugModel}`).join("|")}, net ${net.length}, deleted ${d.deleted}, down ${d.down}) vs daemon ${JSON.stringify(d.text)}`);
      }
    }
    if (o.chaos) {
      // I8: an unresolved conflict's draft is in storage, in its own window's entry.
      const open: { doc: string; s: SyncLike }[] = [...syncs].map(([doc, s]) => ({ doc, s }));
      if (ghostBox.cur) open.push({ doc: "goals", s: ghostBox.cur.sync });
      for (const { doc, s } of open) {
        if (s.currentStatus === "conflict" && !stashedBuffers(doc).includes(s.text)) {
          fail("conflict-draft-not-in-storage", `${doc}: an open conflict has no stored draft`);
        }
      }
      // The second window's keystrokes are not lost either, and it matches the daemon.
      if (ghostBox.cur && !ghostWorkTransferred && ghostBox.cur.sync.currentStatus !== "conflict" && ghostBox.cur.sync.currentStatus !== "gone") {
        const d = daemons.get("goals")!;
        const surfaces = [d.text, ghostBox.cur.sync.text, ...stashedBuffers("goals"), syncs.get("goals")?.text ?? ""].join("\n").split("\n");
        for (const tok of ghostTyped.values()) {
          if (!surfaces.includes(tok) && !d.everHad(tok)) fail("ghost-keystroke-lost", `goals: ${tok} typed in the second window is nowhere`);
        }
        if (ghostBox.cur.sync.text !== d.text && !hasConflictMarkers(ghostBox.cur.sync.text)) {
          fail("ghost-pane-differs-from-daemon", `second window (status ${ghostBox.cur.sync.currentStatus}, ended ${String(ghostBox.cur.sync.ended)}, net ${net.length}, closing ${[...(closing.get("goals") ?? [])].map((c) => c.currentStatus).join("|")}) ${JSON.stringify(ghostBox.cur.sync.text)} vs daemon ${JSON.stringify(d.text)}`);
        }
      }
    }
    // I5: no save carried conflict markers that the user's own buffer did not hold.
    for (const sv of saves) {
      if (hasConflictMarkers(sv.text) && !sv.bufferHadMarkers) {
        fail("markers-written-to-doc", `${sv.doc}: a save carried markers the buffer did not hold`);
      }
    }
  }
  for (const s of syncs.values()) s.dispose();
  ghostBox.cur?.sync.dispose();
  return violation;
}
