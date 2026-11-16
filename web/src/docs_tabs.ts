// The Docs strip's tab buttons (#7409). A doc keeps one button for as long as it
// is listed: a re-render updates that button in place and inserts, moves or
// removes only what changed, so a tab that survives a doc-list refresh is never
// detached from the strip. That is what keeps a click whole. Browsers drop the
// click when the pressed node leaves the document before the release — even if
// the same node is put straight back — and the pane refreshes its list after
// every proposal.changed event, so a press during an agent's edit used to come
// up on a replacement button and open nothing.

import { h } from "./dom.js";
import { orderChildren } from "./keyed-rows.js";
import type { ProposalDocRef } from "./proposal_api.js";

/** What the pane does with a tab's events. Each gets the doc as last rendered. */
export interface DocTabEvents {
  /** A listed doc's tab was clicked. */
  open(doc: ProposalDocRef): void;
  /** A deleted doc's tab (kept for its unsaved work) was clicked. */
  openGone(doc: ProposalDocRef): void;
  key(e: KeyboardEvent, doc: ProposalDocRef): void;
  menu(e: MouseEvent, doc: ProposalDocRef, tab: HTMLElement): void;
  press(e: PointerEvent, doc: ProposalDocRef, tab: HTMLElement): void;
  move(e: PointerEvent): void;
  release(): void;
}

interface Tab {
  readonly button: HTMLButtonElement;
  doc: ProposalDocRef;
}

export class DocTabs {
  private readonly listed = new Map<string, Tab>();
  private readonly gone = new Map<string, Tab>();

  constructor(private readonly strip: HTMLElement, private readonly on: DocTabEvents) {}

  /** Shows `docs` in order, then each deleted doc that is not listed again, with
   *  the doc named `current` selected. */
  render(docs: readonly ProposalDocRef[], gone: ReadonlyMap<string, ProposalDocRef>, current: string | null): void {
    const buttons: HTMLElement[] = [];
    const listedNow = new Set<string>();
    for (const doc of docs) {
      listedNow.add(doc.name);
      const tab = this.listed.get(doc.name) ?? this.listedTab(doc);
      tab.doc = doc;
      tab.button.title = doc.file;
      buttons.push(select(tab, doc.name, current));
    }
    const goneNow = new Set<string>();
    for (const [name, doc] of gone) {
      if (listedNow.has(name)) continue;
      goneNow.add(name);
      const tab = this.gone.get(name) ?? this.goneTab(name, doc);
      tab.doc = doc;
      tab.button.title = `${doc.file} was deleted; your unsaved changes are kept`;
      buttons.push(select(tab, name, current));
    }
    prune(this.listed, listedNow);
    prune(this.gone, goneNow);
    orderChildren(this.strip, buttons);
  }

  private listedTab(doc: ProposalDocRef): Tab {
    const button = h("button", { type: "button", class: "af-docs-tab", role: "tab" }, doc.name);
    const tab: Tab = { button, doc };
    button.addEventListener("click", () => this.on.open(tab.doc));
    button.addEventListener("keydown", (e) => this.on.key(e, tab.doc));
    button.addEventListener("contextmenu", (e) => this.on.menu(e, tab.doc, button));
    button.addEventListener("pointerdown", (e) => this.on.press(e, tab.doc, button));
    button.addEventListener("pointermove", (e) => this.on.move(e));
    for (const type of ["pointerup", "pointercancel", "pointerleave"] as const) {
      button.addEventListener(type, () => this.on.release());
    }
    this.listed.set(doc.name, tab);
    return tab;
  }

  private goneTab(name: string, doc: ProposalDocRef): Tab {
    const button = h("button", { type: "button", class: "af-docs-tab af-docs-tab-gone", role: "tab" }, `${name} (deleted)`);
    const tab: Tab = { button, doc };
    button.addEventListener("click", () => this.on.openGone(tab.doc));
    this.gone.set(name, tab);
    return tab;
  }
}

function select(tab: Tab, name: string, current: string | null): HTMLButtonElement {
  const selected = name === current;
  tab.button.setAttribute("aria-selected", selected ? "true" : "false");
  tab.button.tabIndex = selected ? 0 : -1;
  return tab.button;
}

function prune(tabs: Map<string, Tab>, keep: ReadonlySet<string>): void {
  for (const name of [...tabs.keys()]) {
    if (!keep.has(name)) tabs.delete(name);
  }
}
