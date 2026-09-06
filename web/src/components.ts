// Shared presentation chrome. State, transport and operation policy stay with callers.
// Native DOM nodes and listeners preserve the existing focus/ownership contracts.
import { h } from "./dom.js";
import { mutationNotice } from "./recovery.js";
import { VIEWS, type View } from "./nav.js";

/** Stable top-level navigation, patched by AppShell as the selected view changes. */
export function viewNavigation(onSelect: (view: View) => void): {
  el: HTMLElement; tabs: Map<View, HTMLElement>;
} {
  const el = h("div", { class: "af-viewnav", role: "tablist" });
  el.setAttribute("aria-label", "Views");
  const tabs = new Map<View, HTMLElement>();
  const labels: Record<View, string> = { sessions: "Sessions", tasks: "Tasks", config: "Config" };
  for (const view of VIEWS) {
    const tab = h("button", { type: "button", class: "af-viewtab", role: "tab" }, labels[view]);
    tab.dataset.view = view;
    tab.addEventListener("click", () => onSelect(view));
    tabs.set(view, tab);
    el.append(tab);
  }
  return { el, tabs };
}

/** A live modal: its root element plus in-place patch controls index.ts drives
 *  around the async submit. close() removes it from the DOM. */
export interface ModalHandle {
  el: HTMLElement;
  setBusy(busy: boolean): void;
  setError(msg: string | null): void;
  close(): void;
}

/** Builds the shared modal chrome: a backdrop, a titled card, a body slot, an
 *  error line, and a footer with a cancel + a primary action button. Returns the
 *  pieces the specific modals wire their behavior onto. Clicking the backdrop or
 *  pressing Escape cancels; Enter is left to the form's own submit. */
export function modalChrome(opts: {
  title: string;
  confirmLabel: string;
  confirmClass: string;
  onCancel: () => void;
}): {
  handle: ModalHandle;
  body: HTMLElement;
  confirmBtn: HTMLButtonElement;
  cancelBtn: HTMLButtonElement;
  errorLine: HTMLElement;
} {
  const body = h("div", { class: "af-modal-body" });
  const errorLine = h("div", { class: "af-modal-error", role: "alert" });
  errorLine.hidden = true;

  const cancelBtn = h("button", { type: "button", class: "af-ghost" }, "Cancel");
  const confirmBtn = h("button", { type: "submit", class: opts.confirmClass }, opts.confirmLabel);
  const footer = h("div", { class: "af-modal-foot" }, cancelBtn, confirmBtn);

  const card = h(
    "div",
    { class: "af-modal-card", role: "dialog" },
    h("h2", { class: "af-modal-title" }, opts.title),
    body,
    errorLine,
    footer,
  );
  card.setAttribute("aria-modal", "true");
  card.setAttribute("aria-label", opts.title);
  // Programmatically focusable, but NOT in the tab order (-1, not 0). Every modal
  // that opens with a text field focuses that field; one that deliberately does
  // not (add-project with its picker, #2788) still has to put focus INSIDE the
  // dialog, or Tab and Enter keep driving the control behind the overlay that
  // opened it. Focusing the card itself is the dialog-pattern answer: it is
  // announced (role/aria-modal/aria-label above) and raises no virtual keyboard.
  card.tabIndex = -1;
  // Stop a click inside the card from bubbling to the backdrop's cancel handler.
  card.addEventListener("click", (e) => e.stopPropagation());

  const backdrop = h("div", { class: "af-modal-backdrop" }, card);
  backdrop.addEventListener("click", () => opts.onCancel());

  cancelBtn.addEventListener("click", () => opts.onCancel());

  const handle: ModalHandle = {
    el: backdrop,
    setBusy(busy: boolean) {
      confirmBtn.disabled = busy;
      cancelBtn.disabled = busy;
      card.classList.toggle("af-modal-busy", busy);
    },
    setError(msg: string | null) {
      if (msg) {
        errorLine.replaceChildren(mutationNotice(`${opts.title} failed`, msg, `Review the details, then ${opts.confirmLabel.toLowerCase()} again.`));
        errorLine.hidden = false;
      } else {
        errorLine.textContent = "";
        errorLine.hidden = true;
      }
    },
    close() {
      backdrop.remove();
    },
  };
  return { handle, body, confirmBtn, cancelBtn, errorLine };
}

