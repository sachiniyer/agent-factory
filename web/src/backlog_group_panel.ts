// The group parts of the item panel (backlog-v2 proposal, ui.md "Item panel"):
// Pause / Unpause and Archive / Unarchive, Dependencies and History.

import { h } from "./dom.js";
import { type BacklogView, type Item, STATE_GLYPHS, STATE_LABELS, derivedOf, formatID } from "./backlog_model.js";
import { groupDependencies, groupHistory } from "./backlog_groups.js";
import { formatTime } from "./time.js";

export interface GroupChange {
  paused?: boolean;
  archived?: boolean;
}

export const PAUSED_GLYPH = "‖";

function action(label: string, key: string, run: () => void): HTMLButtonElement {
  const b = h("button", { type: "button", class: "af-ghost" }, label);
  b.dataset.action = key;
  b.addEventListener("click", run);
  return b;
}

/** Pause / Unpause on any parent; Archive on a group. An archived group only
 *  unarchives: it comes back paused. */
export function groupButtons(it: Item, group: boolean, change: (id: number, c: GroupChange) => void): HTMLButtonElement[] {
  if (it.archived) return [action("Unarchive", "unarchive", () => change(it.id, { archived: false }))];
  const out = [it.paused
    ? action("Unpause", "unpause", () => change(it.id, { paused: false }))
    : action("Pause", "pause", () => change(it.id, { paused: true }))];
  if (group) out.push(action("Archive", "archive", () => change(it.id, { archived: true })));
  return out;
}

function link(view: BacklogView, id: number, it: Item | undefined, select: (id: number) => void): HTMLElement {
  if (!it) return h("span", { class: "af-backlog-reason" }, `${formatID(id)} (missing)`);
  const d = derivedOf(view, id);
  const b = h("button", { type: "button", class: "af-backlog-item-link" }, `${formatID(id)} ${it.title}`);
  b.addEventListener("click", () => select(id));
  return h("span", { class: "af-backlog-dep-end" }, b,
    h("span", { class: `af-backlog-reason af-backlog-state-${d.state}` }, `(${STATE_LABELS[d.state]})`));
}

function section(title: string, ...children: Node[]): HTMLElement {
  return h("section", { class: "af-backlog-section" }, h("h3", { class: "af-backlog-section-title" }, title), ...children);
}

export function groupSections(view: BacklogView, group: Item, select: (id: number) => void): HTMLElement[] {
  const deps = groupDependencies(view.backlog, group.id);
  const depList = deps.length === 0
    ? h("p", { class: "af-backlog-reason" }, "No dependencies")
    : h("ul", { class: "af-backlog-deps" }, ...deps.map((d) => h("li", { class: "af-backlog-dep" },
      link(view, d.from.id, d.from, select), h("span", { class: "af-backlog-reason" }, "waits on"),
      link(view, d.toID, d.to, select),
      ...(d.inside ? [] : [h("span", { class: "af-backlog-reason" }, "· outside the group")]))));
  const done = groupHistory(view, group.id);
  const history = done.length === 0
    ? h("p", { class: "af-backlog-reason" }, "Nothing done or dropped yet")
    : h("ul", { class: "af-backlog-history" }, ...done.map((it) => {
      const d = derivedOf(view, it.id);
      const b = h("button", { type: "button", class: "af-backlog-item-link" }, `${formatID(it.id)} ${it.title}`);
      b.addEventListener("click", () => select(it.id));
      return h("li", { class: "af-backlog-dep" }, b,
        h("span", { class: `af-backlog-chip af-backlog-state-${d.state}` }, h("span", { class: "af-backlog-glyph", ariaHidden: "true" }, STATE_GLYPHS[d.state]), STATE_LABELS[d.state]),
        h("span", { class: "af-backlog-reason" }, formatTime(it.updated_at)));
    }));
  return [section("Dependencies", depList), section("History", history)];
}
