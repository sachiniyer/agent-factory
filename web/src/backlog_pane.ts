// The Backlog view (backlog proposal, ui.md): the runner bar, the ordered item
// tree with filters, reorder by drag or Alt+arrows, a "New item" row, and the
// item panel. Every change goes to the daemon and comes back through
// backlog.changed; a reorder shows at once and snaps back on refusal, and a
// started item shows Starting until its session exists.

import { errorText, isMutationOutcomeUncertain } from "./api.js";
import {
  type BudgetReply, addItem, addNote, answerQuestion, deleteItem, backlogRoutes, moveItem, runNext, setBudget, setItemStatus, setSettings, startItem, updateItem,
} from "./backlog_api.js";
import type { BacklogData } from "./backlog_data.js";
import {
  type Backlog, type BacklogView, type DropZone, FILTERS, type Filter, type Item, type ListRow, type MoveTarget,
  applyMove, blocking, derivedOf, dropMove, formatID, itemByID, keyboardMove, latestRun, listRows, overlayMove, relatedIDs,
  safeHttpUrl, sources, topQueued,
} from "./backlog_model.js";
import { ItemPanel, type WriteOutcome, entryField, stateChip } from "./backlog_panel.js";
import { PREVIEW_LINES, PreviewCache, PreviewPopover, previewSession } from "./backlog_preview.js";
import { RunnerBar } from "./backlog_runner.js";
import { closedGate, webReason } from "./backlog_usage.js";
import { h } from "./dom.js";
import { icon } from "./icon.js";
import { recoveryScreen } from "./recovery.js";
import { isArchived, rowKind } from "./status.js";
import { formatDuration, formatTime } from "./time.js";
import type { SessionData } from "./types.js";

export interface BacklogHost {
  token(): string | null;
  /** Re-reads the backlogs; settles true when the read of `root` (all, when none is named) succeeded. */
  refresh(root?: string): Promise<boolean>;
  /** The newest backlog refresh started so far. */
  started(): number;
  /** Takes the budget a SetBudget reply carries, so a save is shown even when the read after it fails. */
  applyBudget?(reply: BudgetReply): void;
  notify(message: string): void;
  openSession(id: string): void;
  archiveSession(id: string): void;
  restoreSession(id: string): void;
  loadSessions(ids: string[]): Promise<void>;
  addProject?(): void;
  /** Asks the user to confirm; resolves true to go ahead. */
  confirm(title: string, body: string, confirmLabel: string): Promise<boolean>;
}

const DRAG_TYPE = "application/x-af-backlog-item";

export const EMPTY_COPY = "No items yet · Add one below, or ask an agent to make items from a proposal.";

/** Whether a started item still shows Starting: its request is out, or its
 *  run is starting, or its session has not reached the session list yet and
 *  the daemon still has the item starting or working. */
function stillStarting(it: Item | undefined, derived: string | undefined, inFlight: boolean, sessions: SessionData[]): boolean {
  if (inFlight) return true;
  // The daemon's own state, read after the start's reply, outranks the session list: a session killed
  // before it ever showed up there leaves the run recorded as started with no session to wait for.
  if (derived !== undefined && derived !== "starting" && derived !== "working") return false;
  const run = it?.runs?.at(-1);
  if (!run || run.state === "failed") return false;
  if (run.state === "starting") return true;
  return run.session_id !== "" && !sessions.some((s) => s.id === run.session_id);
}

export class BacklogPane {
  readonly el: HTMLElement;
  private data: BacklogData | null = null;
  private root: string | null = null;
  private sessions: SessionData[] = [];
  private selected: number | null = null;
  private filter: Filter = "all";
  private source: string | null = null;
  private showDone = false;
  private readonly folded = new Set<number>();
  // A reorder shown before the daemon has it. It stays until data from a refresh
  // started after the move committed arrives; gen is Infinity until then.
  private pending: { root: string; backlog: Backlog; base: Backlog; gen: number } | null = null;
  // Reorder writes go out one at a time: each target is computed from the previous
  // optimistic order, so they must commit in the order they were made.
  private moves: Promise<unknown> = Promise.resolve();
  // A refusal breaks the optimistic chain — but only within the project it
  // happened in. Each move captures its project's gen at queue time and skips
  // itself when a refusal has bumped that project's gen by the time its segment
  // runs, so a queued move whose target was computed against a refused order is
  // dropped instead of being sent against the daemon's real order. A refusal in
  // one project does not discard a reorder queued in another after switching.
  private movesGen = new Map<string, number>();
  private panel: ItemPanel | null = null;
  private dragId: number | null = null;
  private focusId: number | null = null;
  private cursor: number | null = null;
  private focusPanel = false;
  private want: { root: string; id: number } | null = null;
  private readonly requested = new Set<string>();
  private rowEls = new Map<number, HTMLElement>();
  private readonly newItem: HTMLInputElement;
  // Items started from this tab, by project and id: gen is the refresh that
  // follows the start's reply, Infinity until it comes.
  private readonly starts = new Map<string, { root: string; id: number; gen: number }>();
  private running = false;
  // Starts asked for and not yet answered by the daemon, so a second click
  // while the confirm or the request is out does nothing.
  private readonly asking = new Set<string>();
  private readonly confirmed = new Set<string>();
  private readonly runner: RunnerBar;
  private readonly headSlot = h("div", {});
  private readonly filterSlot = h("div", {});
  private readonly listSlot = h("div", {});
  private readonly emptySlot = h("div", {});
  private readonly main: HTMLElement;
  private readonly previews: PreviewCache;
  private readonly popover: PreviewPopover;
  private hoverId: number | null = null;
  // The item whose "Blocks n" was clicked: the rows it blocks stay picked.
  private pinned: number | null = null;

  constructor(private readonly host: BacklogHost) {
    this.el = h("section", { class: "af-backlog" });
    this.el.setAttribute("aria-label", "Backlog");
    this.runner = new RunnerBar(this.runnerActions());
    this.previews = new PreviewCache((id) => {
      const token = this.host.token();
      return token === null ? Promise.reject(new Error("not signed in")) : previewSession(id, PREVIEW_LINES, token);
    });
    this.popover = new PreviewPopover(this.previews);
    this.headSlot.append(this.runner.el);
    this.newItem = entryField("New item…", "New item title", (title) => this.mutate((t) => addItem(this.root ?? "", { title }, t)));
    const listWrap = h("div", { class: "af-backlog-listwrap" }, this.filterSlot, this.listSlot, this.emptySlot,
      h("div", { class: "af-backlog-new" }, icon("plus"), this.newItem));
    this.main = h("div", { class: "af-backlog-main" }, listWrap);
    const endDrag = (): void => { this.dragId = null; };
    this.listSlot.addEventListener("dragend", endDrag);
    this.listSlot.addEventListener("drop", endDrag);
    this.listSlot.addEventListener("mouseleave", () => {
      this.hoverId = null;
      this.applyHighlight();
    });
  }

  update(data: BacklogData, root: string | null, sessions: SessionData[]): void {
    if (data === this.data && root === this.root) {
      if (sessions === this.sessions) return;
      const before = this.runSig(this.sessions);
      this.sessions = sessions;
      if (this.runSig(sessions) === before && this.starts.size === 0) return;
      this.render();
      return;
    }
    if (root !== this.root) {
      this.closePanel();
      this.selected = this.want?.root === root ? this.want.id : null;
      this.focusPanel = this.selected !== null;
      this.want = null;
      this.pinned = null;
      this.hoverId = null;
      this.source = null;
      this.cursor = null;
      this.folded.clear();
    }
    // A read of the project that failed kept its pre-move snapshot, which says nothing about the move.
    if (this.pending && data.generation > this.pending.gen && !data.errors.has(this.pending.root)) this.pending = null;
    this.data = data;
    this.root = root;
    this.sessions = sessions;
    this.render();
  }

  /** The sessions as the rows' chips and the panel show them, to skip unrelated session events. */
  private runSig(sessions: SessionData[]): string {
    const items = this.view()?.backlog.items ?? [];
    const it = this.selected !== null ? items.find((x) => x.id === this.selected) : undefined;
    const chips = items.map((x) => {
      const sid = latestRun(x)?.session_id;
      const s = sid ? sessions.find((y) => y.id === sid) : undefined;
      return s ? [s.id, isArchived(s)] : null;
    });
    return JSON.stringify([chips, (it?.runs ?? []).map((r) => {
      const s = sessions.find((x) => x.id === r.session_id);
      return s ? [s.id, rowKind(s), s.branch] : null;
    })]);
  }

  /** Opens an item's panel. The Sessions view's item chip names the project it
   *  is switching to, which may not have reached update() yet. */
  select(id: number, root: string | null = this.root): void {
    if (root !== this.root) {
      this.want = root ? { root, id } : null;
      return;
    }
    this.selected = id;
    this.cursor = id;
    this.focusPanel = true;
    this.render();
  }

  private view(): BacklogView | undefined {
    const v = this.root ? this.data?.views.get(this.root) : undefined;
    if (!v) return v;
    let out = v;
    if (this.pending && this.pending.root === this.root) out = { ...out, backlog: overlayMove(out.backlog, this.pending.backlog, this.pending.base) };
    const starting = [...this.starts.values()].filter((s) => s.root === this.root);
    if (starting.length > 0) {
      const states = { ...out.states };
      for (const s of starting) states[String(s.id)] = { state: "starting" };
      out = { ...out, states };
    }
    return out;
  }

  /** Drops the starts that the daemon's data and the session list now show. */
  private pruneStarts(): void {
    const gen = this.data?.generation ?? 0;
    for (const [k, s] of this.starts) {
      const it = this.data?.views.get(s.root)?.backlog.items.find((x) => x.id === s.id);
      const derived = this.data?.views.get(s.root)?.states[String(s.id)]?.state;
      if (!stillStarting(it, derived, gen <= s.gen, this.sessions)) this.starts.delete(k);
    }
  }

  /** Asks once per closed budget gate, by gate and window, before a start
   *  by hand. */
  private async gateOK(v: BacklogView, verb: string, id: number): Promise<boolean> {
    const gate = closedGate(this.data?.budget ?? null, this.data?.usage ?? null, v.runner, Date.now());
    if (gate === null || this.confirmed.has(gate.key)) return true;
    const ok = await this.host.confirm("Budget gate closed",
      `${gate.text} ${verb} ${formatID(id)} anyway? Its use still counts.`, `${verb} anyway`);
    if (ok) this.confirmed.add(gate.key);
    return ok;
  }

  private markStarting(root: string, id: number): { gen: number } {
    const entry = { root, id, gen: Infinity };
    this.starts.set(`${root}\n${id}`, entry);
    return entry;
  }

  private settle(root: string, entries: { gen: number }[]): Promise<unknown> {
    for (const e of entries) e.gen = this.host.started();
    return this.host.refresh();
  }

  private start(root: string, id: number): void {
    const v = this.root === root ? this.view() : undefined;
    const key = `${root}\n${id}`;
    if (!v || this.asking.has(key)) return;
    this.asking.add(key);
    void this.gateOK(v, "Start", id).then((ok) => {
      const token = this.host.token();
      if (!ok || token === null) {
        this.asking.delete(key);
        return;
      }
      const entry = this.markStarting(root, id);
      this.render();
      startItem(root, id, token).then(() => {
        this.asking.delete(key);
        return this.settle(root, [entry]);
      }, (e: unknown) => {
        this.asking.delete(key);
        this.starts.delete(key);
        this.host.notify(errorText(e));
        this.render();
        void this.host.refresh();
      });
    });
  }

  private runNext(): void {
    const v = this.view();
    const root = this.root;
    if (!v || !root || this.running) return;
    this.running = true;
    this.render();
    const top = topQueued(v);
    void (top ? this.gateOK(v, "Start", top.id) : Promise.resolve(true)).then((ok) => {
      const token = this.host.token();
      if (!ok || token === null) {
        this.running = false;
        this.render();
        return;
      }
      runNext(root, token).then((r) => {
        this.running = false;
        if (r.started.length === 0 && r.reason) {
          const why = webReason(r.reason);
          this.host.notify(why.charAt(0).toUpperCase() + why.slice(1));
        }
        return this.settle(root, r.started.map((id) => this.markStarting(root, id)));
      }, (e: unknown) => {
        this.running = false;
        this.host.notify(errorText(e));
        void this.host.refresh();
      }).finally(() => this.render());
    });
  }

  private async answer(root: string, id: number, questionId: number, text: string): Promise<boolean> {
    const v = this.root === root ? this.view() : this.data?.views.get(root);
    if (v && !await this.gateOK(v, "Answer", id)) return false;
    const token = this.host.token();
    if (token === null) return false;
    try {
      await answerQuestion(root, id, questionId, text, token);
      void this.host.refresh();
      return true;
    } catch (e) {
      this.host.notify(errorText(e));
      void this.host.refresh();
      return false;
    }
  }

  private runnerActions() {
    return {
      setRunner: (on: boolean) => this.mutate((t) => setSettings(this.root ?? "", { runner: on }, t)),
      setMaxRunning: (n: number) => this.mutate((t) => setSettings(this.root ?? "", { max_running: n }, t)),
      setOnDone: (onDone: string) => this.mutate((t) => setSettings(this.root ?? "", { on_done: onDone }, t)),
      setBudget: (u: Parameters<typeof setBudget>[0]) => this.write(this.root ?? "", (t) => setBudget(u, t).then((r) => this.host.applyBudget?.(r))),
      runNext: backlogRoutes.runBacklog ? () => this.runNext() : undefined,
      openItem: (id: number) => this.select(id),
    };
  }

  private mutate(run: (token: string) => Promise<unknown>): void {
    if (this.root) this.write(this.root, run);
  }

  /** `root` is the project the write modifies, bound by the caller: a save the serial queue starts
   *  later must be acknowledged by a read of that project, not of whichever one is selected by then.
   *  Settles "confirmed" once the write and a refresh that read the project have landed, "refused" when
   *  the write failed, and "unconfirmed" when the daemon took it but the read after it failed (the
   *  edit may be saved; the caller keeps what it shows until a snapshot says so). */
  private write(root: string, run: (token: string) => Promise<unknown>): Promise<WriteOutcome> {
    const token = this.host.token();
    if (token === null) return Promise.resolve("refused");
    return run(token).then(() => this.host.refresh(root).then((read): WriteOutcome => read ? "confirmed" : "unconfirmed"), (e: unknown) => {
      this.host.notify(errorText(e));
      return this.host.refresh(root).then((): WriteOutcome => "refused");
    });
  }

  private move(id: number, to: MoveTarget): void {
    const v = this.view();
    const root = this.root;
    if (!v || !root) return;
    const next = applyMove(v.backlog, id, to);
    const pending = next ? { root, backlog: next, base: v.backlog, gen: Infinity } : null;
    if (pending) {
      this.pending = pending;
      this.focusId = id;
      this.render();
    }
    const token = this.host.token();
    if (token === null) return;
    const gen = this.movesGen.get(root) ?? 0;
    this.moves = this.moves.then(() => {
      if ((this.movesGen.get(root) ?? 0) !== gen) return;
      return moveItem(root, id, to, token).then(() => {
        if (pending) pending.gen = this.host.started();
        return this.host.refresh();
      }, (e: unknown) => {
        if (pending) {
          // A definitive refusal invalidates the optimistic chain in this
          // project only: bump this project's gen so still-queued moves — whose
          // targets were computed against this refused order — drop themselves
          // rather than commit a phantom the user never saw. An uncertain
          // outcome (the reply was lost; the move may have committed) does not
          // bump the gen, so a successor computed against the now-real order
          // still goes out — only a definitive daemon refusal drops the chain.
          // Either way the overlay snaps back if it is the one shown here (a
          // newer move in another project keeps its own). A refusal does not
          // touch another project's chain, and focus is restored only while the
          // failed project is still on screen, so a background rejection never
          // steals the keyboard focus from an unrelated project.
          if (!isMutationOutcomeUncertain(e)) this.movesGen.set(root, (this.movesGen.get(root) ?? 0) + 1);
          if (this.pending?.root === root) this.pending = null;
          if (this.root === root) {
            this.focusId = id;
            this.render();
          }
        }
        this.host.notify(errorText(e));
        return this.host.refresh();
      });
    }).catch(() => {});
  }

  private render(): void {
    this.pruneStarts();
    const v = this.view();
    const error = this.root ? this.data?.errors.get(this.root) : undefined;
    if (this.root === null || !v) {
      this.closePanel();
      this.popover.hide();
      this.el.classList.remove("af-backlog-has-panel");
      if (this.root === null) {
        this.el.replaceChildren(recoveryScreen({ condition: "No project registered", action: "Add project", run: () => this.host.addProject?.() }));
      } else if (error) {
        this.el.replaceChildren(recoveryScreen({ condition: "Backlog unavailable", detail: error, failed: true,
          action: "Retry", run: () => void this.host.refresh() }));
      } else {
        this.el.replaceChildren(h("p", { class: "af-backlog-empty" }, "Loading backlog…"));
        if (!this.requested.has(this.root)) {
          this.requested.add(this.root);
          void this.host.refresh();
        }
      }
      return;
    }
    this.requested.delete(this.root);
    if (this.el.firstChild !== this.headSlot) this.el.replaceChildren(this.headSlot, this.main, this.popover.el);
    if (this.selected !== null && !itemByID(v.backlog, this.selected)) this.selected = null;
    if (this.pinned !== null && !itemByID(v.backlog, this.pinned)) this.pinned = null;
    const focusedRow = (document.activeElement as HTMLElement | null)?.closest<HTMLElement>(".af-backlog-row");
    if (focusedRow && this.focusId === null && this.listSlot.contains(focusedRow)) this.focusId = Number(focusedRow.dataset.itemId);
    const rows = listRows(v, { filter: this.filter, showDone: this.showDone, folded: this.folded, source: this.source });
    const shown = (id: number | null): boolean => id !== null && rows.some((r) => r.item.id === id);
    this.cursor = [this.focusId, this.cursor, this.selected].find(shown) ?? rows[0]?.item.id ?? null;
    const list = h("ul", { class: "af-backlog-list", role: "tree" });
    list.setAttribute("aria-label", "Items");
    this.rowEls = new Map();
    for (const row of rows) list.append(this.row(v, row));
    this.runner.update(v, this.data?.budget ?? null, this.data?.usage ?? null, this.running);
    this.filterSlot.replaceChildren(this.filters(v));
    this.listSlot.replaceChildren(list);
    this.popover.rebind(list);
    this.emptySlot.replaceChildren();
    if (v.backlog.items.length === 0) this.emptySlot.append(h("p", { class: "af-backlog-empty" }, EMPTY_COPY));
    else if (rows.length === 0) this.emptySlot.append(h("p", { class: "af-backlog-empty" }, "No items match this filter"));
    if (this.selected !== null) {
      if (this.panel?.id !== this.selected) {
        this.closePanel();
        this.panel = new ItemPanel(this.root, this.selected, this.panelActions(this.root), this.previews);
        this.main.append(this.panel.el);
      }
      this.panel.render(v, this.sessions, this.data?.usage ?? null);
    } else {
      this.closePanel();
    }
    this.el.classList.toggle("af-backlog-has-panel", this.selected !== null);
    if (this.focusPanel && this.panel) this.panel.focus();
    else if (this.focusId !== null) {
      (this.rowEls.get(this.focusId) ?? (this.cursor !== null ? this.rowEls.get(this.cursor) : undefined))?.focus({ preventScroll: true });
    }
    this.focusPanel = false;
    this.focusId = null;
    this.applyHighlight();
  }

  /** Lights up the items the hovered or focused row waits on and those that
   *  wait on it, and the rows a pinned "Blocks n" picked; the rest dim. */
  private applyHighlight(): void {
    const v = this.view();
    if (!v) return;
    const active = (document.activeElement as HTMLElement | null)?.closest?.<HTMLElement>(".af-backlog-row");
    const focused = active && this.listSlot.contains(active) ? Number(active.dataset.itemId) : null;
    const hot = this.hoverId ?? focused;
    const related = hot !== null ? relatedIDs(v.backlog, hot) : new Set<number>();
    const picked = new Set(this.pinned !== null ? blocking(v.backlog, this.pinned).map((it) => it.id) : []);
    for (const [id, el] of this.rowEls) {
      el.classList.toggle("af-backlog-row-hot", related.size > 0 && id === hot);
      el.classList.toggle("af-backlog-row-related", related.has(id));
      el.classList.toggle("af-backlog-row-picked", picked.has(id) || (picked.size > 0 && id === this.pinned));
    }
    this.listSlot.classList.toggle("af-backlog-dim", related.size > 0 || picked.size > 0);
  }

  private closePanel(): void {
    this.panel?.dispose();
    this.panel?.el.remove();
    this.panel = null;
  }

  private filters(v: BacklogView): HTMLElement {
    const row = h("div", { class: "af-backlog-filters", role: "group" });
    row.setAttribute("aria-label", "Filter items");
    const chip = (label: string, on: boolean, click: () => void): HTMLButtonElement => {
      const b = h("button", { type: "button", class: `af-backlog-filter${on ? " af-backlog-filter-on" : ""}` }, label);
      b.setAttribute("aria-pressed", String(on));
      b.addEventListener("click", () => {
        click();
        this.render();
      });
      return b;
    };
    for (const f of FILTERS) row.append(chip(f.label, this.filter === f.filter, () => { this.filter = f.filter; }));
    for (const s of sources(v.backlog)) {
      row.append(chip(`From ${s}`, this.source === s, () => { this.source = this.source === s ? null : s; }));
    }
    row.append(chip("Show done", this.showDone, () => { this.showDone = !this.showDone; }));
    return row;
  }

  private row(v: BacklogView, row: ListRow): HTMLElement {
    const it = row.item;
    const li = h("li", { class: `af-backlog-row${it.id === this.selected ? " af-backlog-row-selected" : ""}`,
      tabIndex: it.id === this.cursor ? 0 : -1, draggable: true });
    this.rowEls.set(it.id, li);
    li.dataset.itemId = String(it.id);
    li.style.paddingLeft = `calc(var(--af-space-2) + var(--af-space-4) * ${row.depth})`;
    li.setAttribute("role", "treeitem");
    li.setAttribute("aria-level", String(row.depth + 1));
    li.setAttribute("aria-selected", String(it.id === this.selected));
    if (row.parent) li.setAttribute("aria-expanded", String(!row.folded));
    const handle = h("span", { class: "af-backlog-handle", ariaHidden: "true", title: "Drag to reorder" }, "⠿");
    const fold = h("button", { type: "button", class: "af-backlog-fold", hidden: !row.parent, tabIndex: -1 }, row.folded ? "▸" : "▾");
    fold.setAttribute("aria-label", row.folded ? `Expand ${formatID(it.id)}` : `Collapse ${formatID(it.id)}`);
    fold.addEventListener("click", (e) => {
      e.stopPropagation();
      this.toggleFold(it.id, !this.folded.has(it.id));
    });
    li.append(handle, fold, h("span", { class: "af-backlog-id" }, formatID(it.id)),
      h("span", { class: "af-backlog-row-title" }, it.title), this.rowChip(v, it, row.reason, li), ...this.rowTail(v, it));
    const keys = [[".af-backlog-chip-link", "o"], [".af-backlog-blocks", "b"], [".af-backlog-row-pr", "p"]]
      .filter(([sel]) => li.querySelector(sel)).map(([, k]) => k);
    if (keys.length > 0) li.setAttribute("aria-keyshortcuts", keys.join(" "));
    li.addEventListener("click", () => this.select(it.id));
    li.addEventListener("focus", () => {
      this.setCursor(it.id);
      this.applyHighlight();
    });
    li.addEventListener("blur", () => this.applyHighlight());
    li.addEventListener("mouseenter", () => {
      this.hoverId = it.id;
      this.applyHighlight();
    });
    li.addEventListener("mouseleave", () => {
      if (this.hoverId !== it.id) return;
      this.hoverId = null;
      this.applyHighlight();
    });
    li.addEventListener("keydown", (e) => this.rowKey(e, v, row, li));
    this.wireDrag(li, v.backlog, it.id);
    return li;
  }

  /** The state chip, as a link to the latest run's session when that session
   *  is in the session list. Hovering it, or focusing its row, previews it. */
  private rowChip(v: BacklogView, it: Item, reason: string, li: HTMLElement): HTMLElement {
    const run = latestRun(it);
    const sid = run?.session_id ?? "";
    const s = sid ? this.sessions.find((x) => x.id === sid) : undefined;
    if (!run || !s) return stateChip(v, it.id, reason);
    const archived = isArchived(s);
    const started = Date.parse(run.started_at);
    let extra = "";
    if (archived) extra = " · archived";
    else if (derivedOf(v, it.id).state === "working" && !Number.isNaN(started)) extra = ` · ${formatDuration(Date.now() - started)}`;
    const heading = [run.title || sid, formatTime(run.started_at)].join(" · ");
    return stateChip(v, it.id, reason, {
      extra,
      open: () => this.host.openSession(sid),
      decorate: (el) => {
        if (archived) el.title = `${heading} · Archived`;
        else this.popover.attach(el, sid, heading, li);
      },
    });
  }

  /** "Blocks n" and the PR link at the row's end. */
  private rowTail(v: BacklogView, it: Item): HTMLElement[] {
    const out: HTMLElement[] = [];
    const blocked = blocking(v.backlog, it.id);
    if (blocked.length > 0) {
      const on = this.pinned === it.id;
      const b = h("button", { type: "button", class: `af-backlog-blocks${on ? " af-backlog-blocks-on" : ""}`, tabIndex: -1,
        title: blocked.map((x) => `${formatID(x.id)} ${x.title}`).join("\n") }, `Blocks ${blocked.length}`);
      b.setAttribute("aria-pressed", String(on));
      b.setAttribute("aria-label", `${formatID(it.id)} blocks ${blocked.map((x) => formatID(x.id)).join(", ")}`);
      b.addEventListener("click", (e) => {
        e.stopPropagation();
        this.pick(v, it.id);
      });
      out.push(b);
    }
    const pr = safeHttpUrl(it.pr);
    if (pr) {
      const a = h("a", { class: "af-backlog-row-pr", href: pr, target: "_blank", rel: "noopener noreferrer", tabIndex: -1,
        title: "Pull request" }, icon("external-link"));
      a.setAttribute("aria-label", `Pull request for ${formatID(it.id)}`);
      a.addEventListener("click", (e) => e.stopPropagation());
      out.push(a);
    }
    return out;
  }

  /** Pins or unpins the rows an item blocks. Pinning unfolds their parents,
   *  and drops the filters when a filter still hides one, so focus can land. */
  private pick(v: BacklogView, id: number): void {
    if (this.pinned === id) {
      this.pinned = null;
      this.focusId = id;
      this.render();
      return;
    }
    const blocked = blocking(v.backlog, id);
    if (blocked.length === 0) return;
    for (const x of blocked) {
      const seen = new Set<number>();
      for (let p = x.parent; p && !seen.has(p); p = itemByID(v.backlog, p)?.parent) {
        seen.add(p);
        this.folded.delete(p);
      }
    }
    const shown = new Set(listRows(v, { filter: this.filter, showDone: this.showDone, folded: this.folded, source: this.source })
      .map((r) => r.item.id));
    if (blocked.some((x) => !shown.has(x.id))) {
      this.filter = "all";
      this.source = null;
    }
    this.pinned = id;
    this.focusId = blocked[0].id;
    this.render();
  }

  private setCursor(id: number): void {
    this.cursor = id;
    for (const [rid, el] of this.rowEls) el.tabIndex = rid === id ? 0 : -1;
  }

  private toggleFold(id: number, fold: boolean): void {
    if (fold) this.folded.add(id);
    else this.folded.delete(id);
    this.focusId = id;
    this.render();
  }

  private rowKey(e: KeyboardEvent, v: BacklogView, row: ListRow, li: HTMLElement): void {
    if (e.target !== li) return;
    const id = row.item.id;
    const dirs: Record<string, "up" | "down" | "in" | "out"> = { ArrowUp: "up", ArrowDown: "down", ArrowRight: "in", ArrowLeft: "out" };
    if (e.altKey && dirs[e.key]) {
      e.preventDefault();
      const to = keyboardMove(v.backlog, id, dirs[e.key]);
      if (to) this.move(id, to);
      return;
    }
    if (e.altKey || e.ctrlKey || e.metaKey) return;
    const els = [...this.rowEls.values()];
    const at = els.indexOf(li);
    let next: HTMLElement | undefined;
    switch (e.key) {
      case "Enter":
      case " ":
        e.preventDefault();
        this.select(id);
        return;
      case "ArrowDown":
        next = els[at + 1];
        break;
      case "ArrowUp":
        next = els[at - 1];
        break;
      case "Home":
        next = els[0];
        break;
      case "End":
        next = els[els.length - 1];
        break;
      case "o":
      case "b":
      case "p": {
        const sel = { o: ".af-backlog-chip-link", b: ".af-backlog-blocks", p: ".af-backlog-row-pr" }[e.key];
        const target = li.querySelector<HTMLElement>(sel);
        if (!target) return;
        e.preventDefault();
        target.click();
        return;
      }
      case "Escape":
        if (this.pinned === null) return;
        e.preventDefault();
        this.pinned = null;
        this.focusId = id;
        this.render();
        return;
      case "ArrowRight":
        if (row.parent && row.folded) {
          e.preventDefault();
          this.toggleFold(id, false);
          return;
        }
        if (row.parent && els[at + 1] && Number(els[at + 1].getAttribute("aria-level")) > row.depth + 1) next = els[at + 1];
        break;
      case "ArrowLeft":
        if (row.parent && !row.folded) {
          e.preventDefault();
          this.toggleFold(id, true);
          return;
        }
        next = row.item.parent ? this.rowEls.get(row.item.parent) : undefined;
        break;
      default:
        return;
    }
    e.preventDefault();
    next?.focus();
  }

  private wireDrag(li: HTMLElement, b: Backlog, id: number): void {
    const zoneAt = (e: DragEvent): DropZone => {
      const r = li.getBoundingClientRect();
      const y = (e.clientY - r.top) / Math.max(1, r.height);
      const target = itemByID(b, id);
      const nestable = target && target.status !== "running" && (target.runs ?? []).length === 0;
      if (nestable && y > 0.3 && y < 0.7) return "into";
      return y < 0.5 ? "before" : "after";
    };
    const clear = (): void => li.classList.remove("af-backlog-drop-before", "af-backlog-drop-after", "af-backlog-drop-into");
    const ours = (e: DragEvent): boolean => this.dragId !== null && (e.dataTransfer?.types ?? []).includes(DRAG_TYPE);
    li.addEventListener("dragstart", (e) => {
      this.dragId = id;
      e.dataTransfer?.setData(DRAG_TYPE, String(id));
      e.dataTransfer?.setData("text/plain", formatID(id));
      if (e.dataTransfer) e.dataTransfer.effectAllowed = "move";
    });
    li.addEventListener("dragend", () => { this.dragId = null; });
    li.addEventListener("dragover", (e) => {
      if (!ours(e) || dropMove(b, this.dragId ?? 0, id, zoneAt(e)) === null) return;
      e.preventDefault();
      clear();
      li.classList.add(`af-backlog-drop-${zoneAt(e)}`);
    });
    li.addEventListener("dragleave", (e) => {
      if (!li.contains(e.relatedTarget as Node | null)) clear();
    });
    li.addEventListener("drop", (e) => {
      clear();
      const drag = ours(e) ? this.dragId : null;
      this.dragId = null;
      if (drag === null) return;
      e.preventDefault();
      const to = dropMove(b, drag, id, zoneAt(e));
      if (to) this.move(drag, to);
    });
  }

  /** Actions bound to one project, so a panel never writes to another. */
  private panelActions(root: string) {
    const here = (): boolean => this.root === root;
    return {
      update: (id: number, u: Parameters<typeof updateItem>[2]) => this.write(root, (t) => updateItem(root, id, u, t)),
      status: (id: number, s: Parameters<typeof setItemStatus>[2]) => this.write(root, (t) => setItemStatus(root, id, s, t)),
      remove: (id: number) => {
        if (here()) this.selected = null;
        this.write(root, (t) => deleteItem(root, id, t));
      },
      move: (id: number, to: MoveTarget) => {
        if (here()) this.move(id, to);
      },
      // A note is appended once the daemon accepts it, whether or not the read after it landed.
      note: (id: number, text: string) => this.write(root, (t) => addNote(root, id, text, t)).then((o) => o !== "refused"),
      start: (id: number) => this.start(root, id),
      answer: (id: number, questionId: number, text: string) => this.answer(root, id, questionId, text),
      addChild: (parent: number, title: string) => this.write(root, (t) => addItem(root, { title, parent }, t)),
      select: (id: number) => this.select(id, root),
      close: () => {
        if (!here()) return;
        const id = this.selected;
        this.selected = null;
        this.focusId = id;
        this.render();
      },
      openSession: (id: string) => this.host.openSession(id),
      archiveSession: (id: string) => this.host.archiveSession(id),
      restoreSession: (id: string) => this.host.restoreSession(id),
      loadSessions: (ids: string[]) => this.host.loadSessions(ids),
    };
  }
}
