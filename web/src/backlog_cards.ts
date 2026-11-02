// The Backlog list as group cards (backlog-v2 groups): pure counts and the
// card parts the pane wraps around its rows. A group is a top-level parent;
// its descendants are the card's rows.

import {
  type BacklogView, type Item, type ListRow, type State,
  archivedBy, derivedOf, formatID, isAncestor, isParent, itemByID, needsYouLabel,
} from "./backlog_model.js";
import { PAUSED_GLYPH } from "./backlog_group_panel.js";
import { h } from "./dom.js";

export type Segment =
  | { kind: "group"; head: ListRow; rows: ListRow[] }
  | { kind: "loose"; rows: ListRow[] };

/** Splits the list rows into group cards and runs of top-level items with no
 *  children, keeping tree order. */
export function segments(rows: readonly ListRow[]): Segment[] {
  const out: Segment[] = [];
  for (const row of rows) {
    const last = out.at(-1);
    if (row.depth === 0 && row.parent) out.push({ kind: "group", head: row, rows: [] });
    else if (row.depth > 0 && last?.kind === "group") last.rows.push(row);
    else if (last?.kind === "loose") last.rows.push(row);
    else out.push({ kind: "loose", rows: [row] });
  }
  return out;
}

/** Each shown row's place in its card, counted from 1, so the numbers match
 *  what the card shows under the current filters. */
export function positions(rows: readonly ListRow[]): Map<number, number> {
  return new Map(rows.map((r, i) => [r.item.id, i + 1]));
}

export interface Counts {
  done: number;
  total: number;
  working: number;
  needsYou: number;
}

const working = (s: State): boolean => s === "working" || s === "starting";

function count(view: BacklogView, items: Item[]): Counts {
  const c: Counts = { done: 0, total: 0, working: 0, needsYou: 0 };
  for (const it of items) {
    const s = derivedOf(view, it.id).state;
    if (s === "dropped") continue;
    c.total++;
    if (s === "done") c.done++;
    if (working(s)) c.working++;
    if (s === "needs_you") c.needsYou++;
  }
  return c;
}

/** The group's leaf items, without dropped ones. Parents roll their
 *  children up, so counting them too would count work twice. */
export function groupCounts(view: BacklogView, group: number): Counts {
  const b = view.backlog;
  return count(view, b.items.filter((it) => isAncestor(b, group, it.id) && !isParent(b, it.id)));
}

const plural = (n: number, one: string): string => `${n} ${one}${n === 1 ? "" : "s"}`;

function tail(c: Counts): string[] {
  return [...(c.working > 0 ? [`${c.working} working`] : []), ...(c.needsYou > 0 ? [needsYouLabel(c.needsYou)] : [])];
}

export function progressText(c: Counts): string {
  return [`${c.done} of ${c.total} done`, ...tail(c)].join(" · ");
}

/** The page header line: groups, items, working and needs-you, leaving out
 *  archived groups. */
export function overview(view: BacklogView): string {
  const b = view.backlog;
  const live = b.items.filter((it) => !archivedBy(b, it.id));
  const groups = live.filter((it) => !it.parent && isParent(b, it.id)).length;
  const c = count(view, live.filter((it) => !isParent(b, it.id)));
  return [plural(groups, "group"), plural(c.total, "item"), ...tail(c)].join(" · ");
}

export interface Waits {
  /** Items it still waits on, as the daemon derives them: not done, dropped included. */
  open: number;
  dropped: number;
  /** Places of the shown items in the same card that it waits on. */
  after: { pos: number; dropped: boolean }[];
}

/** What a card row waits on. A missing item does not block, as in the
 *  daemon's waitReason. */
export function waitsInGroup(view: BacklogView, it: Item, pos: ReadonlyMap<number, number>): Waits {
  const out: Waits = { open: 0, dropped: 0, after: [] };
  for (const w of it.waits_on ?? []) {
    if (!itemByID(view.backlog, w)) continue;
    const s = derivedOf(view, w).state;
    if (s !== "done") out.open++;
    if (s === "dropped") out.dropped++;
    const p = pos.get(w);
    if (p !== undefined) out.after.push({ pos: p, dropped: s === "dropped" });
  }
  out.after.sort((x, y) => x.pos - y.pos);
  return out;
}

/** The line under a card row's title: id, the shown items in the card it
 *  comes after, and how many items it still waits on. */
export function metaLine(view: BacklogView, it: Item, pos: ReadonlyMap<number, number>): HTMLElement {
  const { open, dropped, after } = waitsInGroup(view, it, pos);
  const parts: Node[] = [h("span", { class: "af-backlog-id" }, formatID(it.id))];
  if (after.length > 0) {
    parts.push(h("span", { ariaHidden: "true" }, "·"), h("span", {}, "after"), ...after.map((a) => a.dropped
      ? h("span", { class: "af-backlog-pos-chip af-backlog-pos-dropped" }, `#${a.pos} dropped`)
      : h("span", { class: "af-backlog-pos-chip" }, `#${a.pos}`)));
  }
  if (open > 0) {
    const text = `waiting on ${plural(open, "item")}${dropped > 0 ? ` (${dropped} dropped)` : ""}`;
    parts.push(h("span", { ariaHidden: "true" }, "·"), h("span", { class: dropped > 0 ? "af-backlog-warn" : "" }, text));
  }
  return h("span", { class: "af-backlog-meta" }, ...parts);
}

export function presentational<T extends HTMLElement>(el: T): T {
  el.setAttribute("role", "none");
  return el;
}

/** The progress line. It sits in the tree only for layout: the header row
 *  names it with aria-describedby. */
export function progress(c: Counts, id: string): HTMLElement {
  const pct = c.total === 0 ? 0 : Math.round((c.done / c.total) * 100);
  const fill = h("span", { class: "af-backlog-progress-fill" });
  fill.style.width = `${pct}%`;
  return hiddenLine(h("li", { class: "af-backlog-progress", id },
    h("span", { class: "af-backlog-progress-bar" }, fill),
    h("span", { class: "af-backlog-reason" }, progressText(c))));
}

function hiddenLine(el: HTMLElement): HTMLElement {
  el.setAttribute("aria-hidden", "true");
  return presentational(el);
}

export function pausedNotice(id: string): HTMLElement {
  return hiddenLine(h("li", { class: "af-backlog-notice", id },
    h("span", { ariaHidden: "true" }, PAUSED_GLYPH), " Paused — the runner starts nothing in this group until you unpause it."));
}
