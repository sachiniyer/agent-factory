// Shared presentation chrome. State, transport and operation policy stay with callers.
// Native DOM nodes and listeners preserve the existing focus/ownership contracts.
import { h } from "./dom.js";
import { VIEWS, type View } from "./nav.js";
import { icon } from "./icon.js";
import type { ITheme } from "@xterm/xterm";

/** A disclosure owns only visibility and focus; callers own the operations. */
export function actionsDisclosure() {
  const trigger = h("button", { type: "button", class: "af-term-more" }, h("span", { class: "af-term-more-label" }, "Actions"), h("span", { class: "af-term-more-compact", ariaHidden: "true" }, "…"));
  trigger.setAttribute("aria-label", "Session actions");
  trigger.setAttribute("aria-expanded", "false");
  const panel = h("div", { class: "af-term-menu", role: "group" });
  panel.setAttribute("aria-label", "Session actions");
  panel.hidden = true;
  const el = h("div", { class: "af-term-more-wrap" }, trigger, panel);
  const outside = (event: MouseEvent) => {
    if (!el.contains(event.target as Node)) close();
  };
  const close = (restoreFocus = false) => {
    panel.hidden = true;
    trigger.setAttribute("aria-expanded", "false");
    document.removeEventListener("mousedown", outside);
    if (restoreFocus) trigger.focus();
  };
  trigger.addEventListener("click", () => {
    if (!panel.hidden) return close();
    panel.hidden = false;
    trigger.setAttribute("aria-expanded", "true");
    document.addEventListener("mousedown", outside);
  });
  el.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && !panel.hidden) {
      event.preventDefault();
      event.stopPropagation();
      close(true);
    }
  });
  return { el, panel, trigger, close, dispose: close };
}

/** One stable title/tab row; patching it never reparents a terminal. */
export function terminalChrome(opts: { title: string; copyLink(): void; handoff(): void; retry(): void }) {
  const menu = actionsDisclosure();
  const action = (label: string, className: string, run: () => void) => {
    const button = h("button", { type: "button", class: `af-ghost af-term-action ${className}` }, label);
    button.addEventListener("click", () => { menu.close(); run(); });
    return button;
  };
  const title = h("span", { class: "af-term-title" }, opts.title);
  const titleBox = h("div", { class: "af-term-head-main" }, title, h("span", { class: "af-term-title-separator", ariaHidden: "true" }, " · "));
  const tabs = h("div", { class: "af-tabbar", role: "tablist" });
  tabs.setAttribute("aria-label", "Session tabs");
  const pr = h("a", { class: "af-pr-badge", target: "_blank", rel: "noopener noreferrer" });
  pr.hidden = true;
  const keyboard = h("span", { class: "af-term-keyboard" }, "Keyboard");
  keyboard.hidden = true;
  const actions = h("div", { class: "af-term-actions" });
  actions.hidden = true;
  const retry = action("Retry", "", opts.retry);
  retry.title = "Resume this session from its usage-limit wall";
  const handoff = action("Handoff", "", opts.handoff);
  handoff.title = "Continue this session under a different agent";
  const copy = action("Copy link", "af-copy-link", opts.copyLink);
  copy.title = "Copy link to this session";
  copy.setAttribute("aria-label", "Copy link");
  const newTabSlot = h("div", { class: "af-term-new-slot" });
  menu.panel.append(newTabSlot, copy, handoff);
  const head = h("div", { class: "af-term-head" }, titleBox, tabs, pr, keyboard, retry, actions, menu.el);
  return { head, title, tabs, pr, keyboard, retry, handoff, actions, newTabSlot, menu, dispose: menu.dispose };
}

/** Split leaves share the same title/close treatment as the main tab row. */
export function paneChrome(onClose: () => void) {
  const glyph = h("span", { class: "af-pane-glyph", ariaHidden: "true" });
  const label = h("span", { class: "af-pane-label" });
  const keyboard = h("span", { class: "af-pane-keyboard" }, "Keyboard");
  const close = h("button", { type: "button", class: "af-pane-close", title: "Close pane" }, icon("x"));
  close.setAttribute("aria-label", "Close pane");
  close.addEventListener("click", (event) => { event.stopPropagation(); onClose(); });
  return { head: h("div", { class: "af-pane-head" }, glyph, label, keyboard, close), glyph, label };
}

/** xterm requires resolved colors. ANSI colors remain the terminal's own palette. */
export function terminalSurface(container: HTMLElement, palette: ITheme): ITheme {
  if (!container.closest(".af-main-term[data-af-theme]")) return palette;
  const style = getComputedStyle(container);
  const token = (name: string) => style.getPropertyValue(name).trim();
  return {
    ...palette,
    background: token("--af-surface"), foreground: token("--af-ink"),
    cursor: token("--af-ink"), cursorAccent: token("--af-surface"),
    selectionBackground: token("--af-surface-raised"),
  };
}

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
  const errorLine = h("p", { class: "af-modal-error", role: "alert" });
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
        errorLine.textContent = msg;
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
