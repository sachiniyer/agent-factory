// A model-based test harness for the Docs sync engine: an in-memory daemon, a
// network that delays and reorders replies, fake timers, a one-editor pane, and a
// seeded random driver that interleaves user edits, agent edits, events, timers,
// document switches, tab closes, outages and conflict resolutions, then checks the
// invariants (see runScenario). It drives the engine only through the public
// DocSync surface, so it can run against any engine with that surface.

import type { DocText, SaveResult } from "./proposal_api.js";
import { CONFLICT_MINE, CONFLICT_SPLIT, CONFLICT_THEIRS, hasConflictMarkers } from "./text_diff.js";

export interface SyncLike {
  readonly text: string;
  readonly currentStatus: string;
  whenQuiet?(): Promise<void>;
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
  load(): Promise<DocText>;
  save(text: string, base: string, final?: boolean): Promise<SaveResult>;
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
  private readonly texts = new Map<string, string>();
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
    return { text: this.text, commit: this.head };
  }
  setLine(line: number, token: string): string {
    const lines = this.text.split("\n");
    lines[line] = token;
    return this.commit(lines.join("\n"));
  }
  /** SaveProposalDoc: fast-forward on the head, else a per-line three-way merge. */
  save(text: string, base: string): SaveResult {
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
  stage: "sent" | "processed";
  run(): unknown;
  result?: { ok: true; value: unknown } | { ok: false; error: Error };
  resolve(v: unknown): void;
  reject(e: unknown): void;
}

export interface ScenarioOptions {
  seed: number;
  steps: number;
  /** Users and the agent may edit the same lines (conflicts happen). */
  collide: boolean;
  /** Probability that a save fails: dropped before the daemon sees it, or applied
   *  with the reply lost. The page may also close with saves in flight. */
  faults?: number;
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
  const stash = new Map<string, Draft>();
  const net: Pending[] = [];
  let now = 0;
  const timers: { at: number; fn: () => void; live: boolean }[] = [];
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
  const closing = new Map<string, SyncLike>();
  const editor = { doc: "" as string, text: "" };
  let attached = "";
  const events: { doc: string; commit: string }[] = [];
  let outage = false;

  const call = <T>(run: () => T): Promise<T> =>
    new Promise<T>((resolve, reject) => {
      net.push({ stage: "sent", run, resolve: resolve as (v: unknown) => void, reject });
    });

  function newSync(doc: string): SyncLike {
    const d = daemons.get(doc)!;
    let self: SyncLike | null = null; // the sync these deps belong to, not whatever is current for the doc
    const deps: SyncDepsLike = {
      load: async () => {
        // A reopened tab loads only after the closing tab's final save has landed.
        const prior = closing.get(doc);
        if (prior && prior !== syncs.get(doc)) await prior.whenQuiet?.();
        return call(() => d.read());
      },
      save: (text, base, final) => {
        saves.push({ doc, text, bufferHadMarkers: self ? hasConflictMarkers(self.text) : false });
        void final;
        const fault = rand() < (o.faults ?? 0) ? (rand() < 0.5 ? "drop" : "lose-reply") : null;
        return call(() => {
          if (fault === "drop") throw new Error("503 service unavailable");
          const result = d.save(text, base);
          if (fault === "lose-reply") throw new Error("network error: reply lost");
          return result;
        });
      },
      setBuffer: (text) => {
        if (attached === doc) {
          editor.text = text;
        }
      },
      onStatus: () => undefined,
      setTimer: (fn, ms) => {
        const t = { at: now + ms, fn, live: true };
        timers.push(t);
        return t;
      },
      clearTimer: (h) => {
        (h as { live: boolean }).live = false;
      },
      draft: {
        load: () => stash.get(doc) ?? null,
        store: (x) => void stash.set(doc, x),
        clear: () => void stash.delete(doc),
      },
    };
    const s = o.makeSync(deps);
    self = s;
    syncs.set(doc, s);
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
    void s.start().catch(() => undefined);
  }

  const lineSets = o.collide ? [0, 1, 2, 3, 4] : [0, 1];
  const agentLines = o.collide ? [0, 1, 2, 3, 4] : [3, 4];

  function userEdit(): void {
    const s = syncs.get(attached);
    if (!s || !isLoaded(s)) return;
    const line = pick(lineSets);
    const token = `u${++seq}`;
    const lines = editor.text.split("\n");
    if (lines.length !== LINES) return; // the buffer holds conflict markers: leave it to the resolver
    lines[line] = token;
    editor.text = lines.join("\n");
    typed.get(attached)!.set(line, token);
    s.edit(editor.text);
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

  async function advance(ms: number): Promise<void> {
    now += ms;
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
      p.stage = "processed";
      try {
        p.result = { ok: true, value: p.run() };
      } catch (e) {
        p.result = { ok: false, error: e as Error };
      }
    } else if (processed.length) {
      const p = pick(processed);
      net.splice(net.indexOf(p), 1);
      if (p.result!.ok) p.resolve(p.result!.value);
      else p.reject(p.result!.error);
    }
    await tick();
  }

  async function drain(): Promise<void> {
    for (let i = 0; i < 400 && net.length; i++) {
      for (const p of net.filter((x) => x.stage === "sent")) {
        p.stage = "processed";
        try {
          p.result = { ok: true, value: p.run() };
        } catch (e) {
          p.result = { ok: false, error: e as Error };
        }
      }
      for (const p of net.filter((x) => x.stage === "processed")) {
        net.splice(net.indexOf(p), 1);
        if (p.result!.ok) p.resolve(p.result!.value);
        else p.reject(p.result!.error);
      }
      await tick();
    }
  }

  /** The page closes: every sync gets its pagehide, requests in flight reach the
   *  daemon or not, replies are lost, and only per-viewer storage and the daemon
   *  survive. The user then opens the page again on the same doc. */
  async function pageClose(): Promise<void> {
    const doc = attached;
    if (!doc) return;
    for (const s of syncs.values()) s.saveBeforeLeaving();
    await tick();
    for (const p of net.filter((x) => x.stage === "sent")) {
      if (rand() < 0.5) {
        try {
          p.run();
        } catch {
          // The daemon rejected or the reply would have failed: the page is gone either way.
        }
      }
    }
    net.length = 0;
    for (const t of timers) t.live = false;
    events.length = 0;
    for (const s of syncs.values()) s.dispose();
    syncs.clear();
    closing.clear();
    attached = "";
    editor.text = "";
    const unsaved = [...typed.get(doc)!.values()].filter((tok) => !daemons.get(doc)!.everHad(tok) && !excused.has(tok));
    await open(doc);
    await drain();
    const back = syncs.get(doc)!;
    // I6: unsaved edits that survive only in storage come back, and say so.
    if (unsaved.length && !["recovered", "conflict", "saving", "saved", "merged"].includes(back.currentStatus)) {
      fail("recovered-draft-not-shown", `${doc}: reopened as ${back.currentStatus} with unsaved ${unsaved.join(",")}`);
    }
    if (unsaved.length && back.currentStatus === "pending") {
      fail("recovered-draft-not-shown", `${doc}: reopened as pending, not recovered`);
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

  function deliverEvent(): void {
    if (!events.length) return;
    const e = events.splice(Math.floor(rand() * events.length), 1)[0]!;
    syncs.get(e.doc)?.remoteChanged(e.commit);
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
    const s = syncs.get(attached);
    if (!s || !isLoaded(s) || hasConflictMarkers(editor.text)) return;
    const lines = editor.text.split("\n");
    if (lines.length !== LINES) return;
    if (!o.collide) {
      // I2: with no collisions, the editor always shows the latest keystroke of
      // every line the user typed, never an older version of it.
      for (const [line, tok] of typed.get(attached)!) {
        if (excused.has(tok)) continue; // the user chose "take theirs"
        if (lines[line] !== tok) {
          fail("editor-older-than-typed", `${attached} line ${line}: editor has ${lines[line]}, last typed ${tok} (status ${s.currentStatus}, sync text ${JSON.stringify(s.text)}, stash ${JSON.stringify([...stash])})`);
        }
      }
    }
  }

  await open("goals");
  await drain();

  for (let step = 0; step < o.steps && !violation; step++) {
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
        const wasConflict = s.currentStatus === "conflict";
        const draft = s.text;
        const savesBefore = saves.length;
        s.dispose();
        closing.set(doc, s);
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
        await drain();
        const back = syncs.get(doc)!;
        if (wasConflict && (back.currentStatus !== "conflict" || back.text !== draft)) {
          fail("conflict-draft-lost-on-close", `${doc}: reopened as ${back.currentStatus}, draft ${JSON.stringify(draft)}, now ${JSON.stringify(back.text)}, daemon ${JSON.stringify(daemons.get(doc)!.text)}`);
        }
      }
    } else if (r < 0.845 && (o.faults ?? 0) > 0) {
      await pageClose();
    } else if (r < 0.855) {
      sendNow();
    } else if (r < 0.86) outage = !outage;
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
  for (let round = 0; round < 12 && !violation; round++) {
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
      const surfaces = [d.text, shown, s.text, stash.get(doc)?.buffer ?? ""].join("\n");
      // I1: no user keystroke is ever lost.
      for (const tok of typed.get(doc)!.values()) {
        if (excused.has(tok)) continue;
        const kept = surfaces.split("\n").includes(tok) || (o.collide && d.everHad(tok));
        if (!kept) {
          fail("keystroke-lost", `${doc}: ${tok} is nowhere (daemon ${JSON.stringify(d.text)}, editor ${JSON.stringify(shown)})`);
        }
      }
      // I4: after a reconnect the pane matches the daemon.
      if (shown !== d.text && s.text !== d.text) {
        fail("pane-differs-from-daemon", `${doc}: pane ${JSON.stringify(s.text)} vs daemon ${JSON.stringify(d.text)}`);
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
  return violation;
}
