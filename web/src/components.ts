// Shared presentation chrome. State, transport and operation policy stay with callers.
// Native DOM nodes and listeners preserve the existing focus/ownership contracts.
import { h } from "./dom.js";
import { mutationNotice } from "./recovery.js";
import { VIEWS, type View } from "./nav.js";
import { icon } from "./icon.js";
import type { ITheme } from "@xterm/xterm";

/** A disclosure owns only visibility and focus; callers own the operations.
 * Responsive callers may keep the panel inline by returning false from enabled. */
export function actionsDisclosure(label = "Session actions", enabled = () => true) {
  const trigger = h("button", { type: "button", class: "af-term-more" }, h("span", { class: "af-term-more-label" }, "Actions"), h("span", { class: "af-term-more-compact", ariaHidden: "true" }, "…"));
  trigger.setAttribute("aria-label", label);
  trigger.setAttribute("aria-expanded", "false");
  const panel = h("div", { class: "af-term-menu", role: "group" });
  panel.setAttribute("aria-label", label);
  panel.hidden = true;
  const el = h("div", { class: "af-term-more-wrap" }, trigger, panel);
  const outside = (event: MouseEvent) => {
    if (!el.contains(event.target as Node)) close();
  };
  const close = (restoreFocus = false) => {
    panel.hidden = enabled();
    trigger.setAttribute("aria-expanded", "false");
    document.removeEventListener("mousedown", outside);
    if (restoreFocus) trigger.focus();
  };
  const open = () => {
    if (!enabled()) return;
    panel.hidden = false;
    trigger.setAttribute("aria-expanded", "true");
    document.addEventListener("mousedown", outside);
  };
  trigger.addEventListener("click", () => panel.hidden ? open() : close());
  el.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && enabled() && !panel.hidden) {
      event.preventDefault();
      event.stopPropagation();
      close(true);
    }
  });
  return { el, panel, trigger, open, close, dispose: close };
}

/** The app's secondary controls use the same disclosure and Escape/focus model.
 * Desktop displays the same nodes inline; resizing closes any phone disclosure. */
export function appbarControls(controls: HTMLElement[], phone = window.matchMedia("(max-width: 768px)")) {
  const menu = actionsDisclosure("More app controls", () => phone.matches);
  menu.el.className = "af-appbar-tools-wrap";
  menu.trigger.className = "af-appbar-more";
  menu.trigger.replaceChildren(icon("ellipsis"));
  menu.trigger.title = "More app controls";
  menu.trigger.setAttribute("aria-controls", "af-appbar-tools");
  menu.panel.className = "af-appbar-tools";
  menu.panel.id = "af-appbar-tools";
  menu.panel.setAttribute("aria-label", "App controls");
  menu.panel.append(...controls);
  const sync = () => {
    const hadFocus = menu.panel.contains(document.activeElement);
    menu.close();
    menu.trigger.hidden = !phone.matches;
    menu.panel.hidden = phone.matches;
    if (phone.matches && hadFocus) menu.trigger.focus();
  };
  phone.addEventListener("change", sync);
  sync();
  return { ...menu, dispose: () => { phone.removeEventListener("change", sync); menu.dispose(); } };
}

/** Session-first follows selected content, not drawer visibility or transient keyboard ownership. */
export function isSessionFirst(phone: boolean, view: string, kind: number | null): boolean {
  return phone && view === "sessions" && kind !== null && kind >= 0 && kind <= 2;
}

/** Move only chrome, preserving live nodes and their event handlers. Reverse restore
 * keeps sibling order exact even when adjacent controls move to different groups. */
export function sessionFirstComposition(moves: [HTMLElement, HTMLElement][]) {
  let active = false;
  let homes: { node: HTMLElement; parent: Node; next: ChildNode | null }[] = [];
  return { setActive(value: boolean) {
    if (value === active) return;
    active = value;
    if (active) {
      homes = moves.map(([node]) => ({ node, parent: node.parentNode!, next: node.nextSibling }));
      for (const [node, target] of moves) target.append(node);
    } else {
      for (const { node, parent, next } of homes.reverse()) parent.insertBefore(node, next);
      homes = [];
    }
  } };
}

/** One stable title/tab row; patching it never reparents a terminal. */
export function terminalChrome(opts: { title: string; copyLink(): void; handoff(): void; retry(): void; closePane?(): void }) {
  const menu = actionsDisclosure();
  const action = (label: string, className: string, run: () => void) => {
    const button = h("button", { type: "button", class: `af-ghost af-term-action ${className}` }, label);
    button.addEventListener("click", () => { menu.close(); run(); });
    return button;
  };
  const title = h("span", { class: "af-term-title", title: opts.title }, opts.title);
  title.setAttribute("aria-label", opts.title);
  const titleBox = h("div", { class: "af-term-head-main" }, title, h("span", { class: "af-term-title-separator", ariaHidden: "true" }, " · "));
  const tabs = h("div", { class: "af-tabbar", role: "tablist" });
  tabs.setAttribute("aria-label", "Session tabs");
  const keyboard = h("span", { class: "af-term-keyboard" }, "Keyboard");
  keyboard.hidden = true;
  const actions = h("div", { class: "af-term-actions" });
  actions.hidden = true;
  const retry = action("Retry", "", opts.retry);
  retry.title = "Resume this session from its usage-limit wall";
  const handoff = action("Handoff", "", opts.handoff);
  handoff.title = "Continue this session under a different agent";
  const copy = action("Copy link", "af-copy-link af-copy-link-phone", opts.copyLink);
  copy.title = "Copy link";
  copy.setAttribute("aria-label", "Copy link");
  const desktopCopy = action("", "af-copy-link af-copy-link-desktop", opts.copyLink);
  desktopCopy.append(icon("link"));
  desktopCopy.title = "Copy link";
  desktopCopy.setAttribute("aria-label", "Copy link");
  const newTabSlot = h("div", { class: "af-term-new-slot" });
  const closePane = action("Close pane", "af-phone-pane-close", () => opts.closePane?.());
  closePane.hidden = true;
  menu.panel.append(newTabSlot, copy, handoff, actions, closePane);
  const head = h("div", { class: "af-term-head" }, titleBox, tabs, desktopCopy, keyboard, retry, menu.el);
  return { head, title, tabs, keyboard, retry, handoff, closePane, actions, newTabSlot, menu, dispose: menu.dispose };
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
  const style = getComputedStyle(container);
  const token = (name: string) => style.getPropertyValue(name).trim();
  return {
    ...palette,
    background: token("--af-surface"), foreground: token("--af-ink"),
    cursor: token("--af-ink"), cursorAccent: token("--af-surface"),
    selectionBackground: token("--af-surface-raised"), selectionForeground: token("--af-ink"),
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

/** Label and field share one native association across dialogs and settings. */
export function field(label: string, control: HTMLElement): HTMLElement {
  return h("label", { class: "af-modal-field" }, h("span", { class: "af-modal-label" }, label), control);
}

/** Compact inherited choices; callers update the summary without replacing fields. */
export function defaultsDisclosure() {
  const summaryText = h("span", { class: "af-defaults-summary" });
  const summary = h("summary", {}, h("span", { class: "af-defaults-fragment" }, "Edit defaults ·"), " ", summaryText);
  const body = h("div", { class: "af-defaults-body" });
  const el = h("details", { class: "af-defaults" }, summary, body);
  const setSummary = (fragments: string[]) => {
    summaryText.replaceChildren(...fragments.flatMap((fragment, index) =>
      [h("span", { class: "af-defaults-fragment" }, `${fragment}${index < fragments.length - 1 ? " ·" : ""}`), " "]));
  };
  return { el, body, setSummary };
}
