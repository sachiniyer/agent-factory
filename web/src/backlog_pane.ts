// The Backlog view (backlog proposal, ui.md): the read-only runner bar, the
// ordered item tree with filters, reorder by drag or Alt+arrows, a "New item"
// row, and the item panel. Every change goes to the daemon and comes back
// through backlog.changed; a reorder shows at once and snaps back on refusal.

import { errorText } from "./api.js";
import { addItem, addNote, deleteItem, moveItem, setItemStatus, updateItem } from "./backlog_api.js";
import type { BacklogData } from "./backlog_data.js";
import {
  type Backlog, type BacklogView, type Budget, type DropZone, FILTERS, type Filter, type ListRow, type MoveTarget,
  applyMove, dropMove, formatID, itemByID, keyboardMove, listRows, sources,
} from "./backlog_model.js";
import { ItemPanel, entryField, stateChip } from "./backlog_panel.js";
import { h } from "./dom.js";
import { icon } from "./icon.js";
import { recoveryScreen } from "./recovery.js";
import { rowKind } from "./status.js";
import type { SessionData } from "./types.js";

export interface BacklogHost {
  token(): string | null;
  /** Re-reads the backlogs; settles true when the read of `root` (all, when none is named) succeeded. */
  refresh(root?: string): Promise<boolean>;
  /** The newest backlog refresh started so far. */
  started(): number;
  notify(message: string): void;
  openSession(id: string): void;
  archiveSession(id: string): void;
  restoreSession(id: string): void;
  loadSessions(ids: string[]): Promise<void>;
  addProject?(): void;
}

const DRAG_TYPE = "application/x-af-backlog-item";

export const EMPTY_COPY = "No items yet · Add one below, or ask an agent to make items from a proposal.";

function usd(v: number): string {
  return `$${v.toFixed(2)}`;
}

const RUNNER_STATES: Record<string, string> = {
  off: "Runner off",
  idle: "Runner on",
  waiting: "Runner waiting",
  stopped: "Runner paused",
};

/** The runner bar's words: its state, reason, the slots in use, and the budget. */
export function runnerText(view: BacklogView, budget: Budget | null): { state: string; warn: boolean; parts: string[] } {
  const r = view.runner;
  const parts = [(r.reason ?? "").replace(/\.$/, ""), `${r.used} of ${r.max} working`];
  if (budget) {
    parts.push(`Backlog share ${budget.share}% · stop at plan ${budget.stop_at}%`);
    if (budget.usd_limit > 0) parts.push(`${usd(budget.usd_spent)} of ${usd(budget.usd_limit)}`);
  }
  return { state: RUNNER_STATES[r.state] ?? "Runner on", warn: r.state === "stopped", parts: parts.filter(Boolean) };
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
  private pending: { root: string; backlog: Backlog; gen: number } | null = null;
  // Reorder writes go out one at a time: each target is computed from the previous
  // optimistic order, so they must commit in the order they were made.
  private moves: Promise<unknown> = Promise.resolve();
  private panel: ItemPanel | null = null;
  private dragId: number | null = null;
  private focusId: number | null = null;
  private cursor: number | null = null;
  private focusPanel = false;
  private want: { root: string; id: number } | null = null;
  private readonly requested = new Set<string>();
  private rowEls = new Map<number, HTMLElement>();
  private readonly newItem: HTMLInputElement;
  private readonly headSlot = h("div", { class: "af-backlog-head" });
  private readonly filterSlot = h("div", {});
  private readonly listSlot = h("div", {});
  private readonly emptySlot = h("div", {});
  private readonly main: HTMLElement;

  constructor(private readonly host: BacklogHost) {
    this.el = h("section", { class: "af-backlog" });
    this.el.setAttribute("aria-label", "Backlog");
    this.newItem = entryField("New item…", "New item title", (title) => this.mutate((t) => addItem(this.root ?? "", { title }, t)));
    const listWrap = h("div", { class: "af-backlog-listwrap" }, this.filterSlot, this.listSlot, this.emptySlot,
      h("div", { class: "af-backlog-new" }, icon("plus"), this.newItem));
    this.main = h("div", { class: "af-backlog-main" }, listWrap);
    const endDrag = (): void => { this.dragId = null; };
    this.listSlot.addEventListener("dragend", endDrag);
    this.listSlot.addEventListener("drop", endDrag);
  }

  update(data: BacklogData, root: string | null, sessions: SessionData[]): void {
    if (data === this.data && root === this.root) {
      if (sessions === this.sessions) return;
      const before = this.runSig(this.sessions);
      this.sessions = sessions;
      if (this.runSig(sessions) === before) return;
      this.render();
      return;
    }
    if (root !== this.root) {
      this.closePanel();
      this.selected = this.want?.root === root ? this.want.id : null;
      this.focusPanel = this.selected !== null;
      this.want = null;
      this.source = null;
      this.cursor = null;
      this.folded.clear();
    }
    if (this.pending && data.generation > this.pending.gen) this.pending = null;
    this.data = data;
    this.root = root;
    this.sessions = sessions;
    this.render();
  }

  /** The selected item's runs as the panel shows them, to skip unrelated session events. */
  private runSig(sessions: SessionData[]): string {
    const it = this.selected !== null ? this.view()?.backlog.items.find((x) => x.id === this.selected) : undefined;
    return JSON.stringify((it?.runs ?? []).map((r) => {
      const s = sessions.find((x) => x.id === r.session_id);
      return s ? [s.id, rowKind(s)] : null;
    }));
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
    if (v && this.pending && this.pending.root === this.root) return { ...v, backlog: this.pending.backlog };
    return v;
  }

  private mutate(run: (token: string) => Promise<unknown>): void {
    if (this.root) this.write(run);
  }

  /** Settles true once the write and the refresh after it have landed, false when the write failed. */
  private write(run: (token: string) => Promise<unknown>): Promise<boolean> {
    const token = this.host.token();
    if (token === null) return Promise.resolve(false);
    const root = this.root ?? undefined;
    // True only when the refresh after the write also read the project: an edit is
    // acknowledged by a snapshot that carries it, not by a failed read that kept the old one.
    return run(token).then(() => this.host.refresh(root), (e: unknown) => {
      this.host.notify(errorText(e));
      return this.host.refresh(root).then(() => false);
    });
  }

  private move(id: number, to: MoveTarget): void {
    const v = this.view();
    const root = this.root;
    if (!v || !root) return;
    const next = applyMove(v.backlog, id, to);
    const pending = next ? { root, backlog: next, gen: Infinity } : null;
    if (pending) {
      this.pending = pending;
      this.focusId = id;
      this.render();
    }
    const token = this.host.token();
    if (token === null) return;
    this.moves = this.moves.then(() => moveItem(root, id, to, token).then(() => {
      if (pending) pending.gen = this.host.started();
      return this.host.refresh();
    }, (e: unknown) => {
      if (pending && this.pending === pending) {
        this.pending = null;
        this.focusId = id;
        this.render();
      }
      this.host.notify(errorText(e));
      return this.host.refresh();
    }).catch(() => {}));
  }

  private render(): void {
    const v = this.view();
    const error = this.root ? this.data?.errors.get(this.root) : undefined;
    if (this.root === null || !v) {
      this.closePanel();
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
    if (this.el.firstChild !== this.headSlot) this.el.replaceChildren(this.headSlot, this.main);
    if (this.selected !== null && !itemByID(v.backlog, this.selected)) this.selected = null;
    const focusedRow = (document.activeElement as HTMLElement | null)?.closest<HTMLElement>(".af-backlog-row");
    if (focusedRow && this.focusId === null && this.listSlot.contains(focusedRow)) this.focusId = Number(focusedRow.dataset.itemId);
    const rows = listRows(v, { filter: this.filter, showDone: this.showDone, folded: this.folded, source: this.source });
    const shown = (id: number | null): boolean => id !== null && rows.some((r) => r.item.id === id);
    this.cursor = [this.focusId, this.cursor, this.selected].find(shown) ?? rows[0]?.item.id ?? null;
    const list = h("ul", { class: "af-backlog-list", role: "tree" });
    list.setAttribute("aria-label", "Items");
    this.rowEls = new Map();
    for (const row of rows) list.append(this.row(v, row));
    this.headSlot.replaceChildren(...this.runnerBar(v).childNodes);
    this.filterSlot.replaceChildren(this.filters(v));
    this.listSlot.replaceChildren(list);
    this.emptySlot.replaceChildren();
    if (v.backlog.items.length === 0) this.emptySlot.append(h("p", { class: "af-backlog-empty" }, EMPTY_COPY));
    else if (rows.length === 0) this.emptySlot.append(h("p", { class: "af-backlog-empty" }, "No items match this filter"));
    if (this.selected !== null) {
      if (this.panel?.id !== this.selected) {
        this.closePanel();
        this.panel = new ItemPanel(this.root, this.selected, this.panelActions(this.root));
        this.main.append(this.panel.el);
      }
      this.panel.render(v, this.sessions);
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
  }

  private closePanel(): void {
    this.panel?.dispose();
    this.panel?.el.remove();
    this.panel = null;
  }

  private runnerBar(v: BacklogView): HTMLElement {
    const r = runnerText(v, this.data?.budget ?? null);
    const bar = h("div", { class: "af-backlog-runner" },
      h("span", { class: `af-backlog-runner-state${r.warn ? " af-backlog-warn" : ""}` }, r.state), ...r.parts.flatMap((p) => [" · ", p]));
    const budget = this.data?.budget;
    if (budget && budget.usd_limit > 0) {
      const fill = h("span", { class: "af-backlog-meter-fill" });
      fill.style.width = `${Math.min(100, (budget.usd_spent / budget.usd_limit) * 100)}%`;
      const meter = h("span", { class: "af-backlog-meter", role: "img" }, fill);
      meter.setAttribute("aria-label", `${usd(budget.usd_spent)} of ${usd(budget.usd_limit)} spent`);
      bar.append(" ", meter);
    }
    if (budget) bar.title = `All projects · cap per run ${budget.max_per_run}% of the plan window`;
    return h("div", {}, h("span", { class: "af-backlog-heading" }, "Backlog"), bar);
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
      h("span", { class: "af-backlog-row-title" }, it.title), stateChip(v, it.id, row.reason));
    li.addEventListener("click", () => this.select(it.id));
    li.addEventListener("focus", () => this.setCursor(it.id));
    li.addEventListener("keydown", (e) => this.rowKey(e, v, row, li));
    this.wireDrag(li, v.backlog, it.id);
    return li;
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
      update: (id: number, u: Parameters<typeof updateItem>[2]) => this.write((t) => updateItem(root, id, u, t)),
      status: (id: number, s: Parameters<typeof setItemStatus>[2]) => this.write((t) => setItemStatus(root, id, s, t)),
      remove: (id: number) => {
        if (here()) this.selected = null;
        this.write((t) => deleteItem(root, id, t));
      },
      move: (id: number, to: MoveTarget) => {
        if (here()) this.move(id, to);
      },
      note: (id: number, text: string) => this.write((t) => addNote(root, id, text, t)),
      addChild: (parent: number, title: string) => this.write((t) => addItem(root, { title, parent }, t)),
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
