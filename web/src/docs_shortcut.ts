// At phone width the tab bar folds into the app ⋯ menu while a terminal tab shows,
// so each Docs tab also gets a one-tap button in the compact session header.

import { h } from "./dom.js";
import { icon } from "./icon.js";
import { type NamedTab, tabDisplayLabel, tabIcon } from "./tablabel.js";
import { TabKind } from "./types.js";

/** The bar indexes of the Docs tabs, in bar order. */
export function docsTabIndexes(tabs: readonly NamedTab[]): number[] {
  const out: number[] = [];
  tabs.forEach((tab, i) => {
    if (tab.kind === TabKind.Docs) out.push(i);
  });
  return out;
}

/** One icon button for each Docs tab, like the new-tab buttons beside it. A click
 *  opens that tab. */
export function docsShortcuts(tabs: readonly NamedTab[], open: (index: number) => void): HTMLElement[] {
  return docsTabIndexes(tabs).map((index) => {
    const tab = tabs[index];
    const name = `Open ${tabDisplayLabel(tab)}`;
    const button = h("button", { type: "button", class: "af-ghost af-term-action af-term-docs", title: name },
      icon(tabIcon(tab.kind)));
    button.setAttribute("aria-label", name);
    button.dataset.tabIndex = String(index);
    button.addEventListener("click", () => {
      button.blur();
      open(index);
    });
    return button;
  });
}
