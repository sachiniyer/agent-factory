// Shared presentation chrome. State, transport and operation policy stay with callers.
// Native DOM nodes and listeners preserve the existing focus/ownership contracts.
import { h } from "./dom.js";
import { mutationNotice } from "./recovery.js";
import { VIEWS, type View } from "./nav.js";
import { icon } from "./icon.js";
import type { ITheme } from "@xterm/xterm";

/** A disclosure owns only visibility and focus; callers own the operations.
 * Responsive callers may keep the panel inline by returning false from enabled. */
export function actionsDisclosure(label = "Session actions", enabled = () => true, onDismiss = () => {}) {
  const trigger = h("button", { type: "button", class: "af-term-more" }, h("span", { class: "af-term-more-label" }, "Actions"), h("span", { class: "af-term-more-compact", ariaHidden: "true" }, "…"));
  trigger.setAttribute("aria-label", label);
  trigger.setAttribute("aria-expanded", "false");
  const panel = h("div", { class: "af-term-menu", role: "group" });
  panel.setAttribute("aria-label", label);
  panel.hidden = true;
  const el = h("div", { class: "af-term-more-wrap" }, trigger, panel);
  const outside = (event: MouseEvent) => {
    if (!el.contains(event.target as Node)) dismiss();
  };
  const close = (restoreFocus = false) => {
    panel.hidden = enabled();
    trigger.setAttribute("aria-expanded", "false");
    document.removeEventListener("mousedown", outside);
    if (restoreFocus) trigger.focus();
  };
  // User dismissal can retire carried state; layout/picker closes must not.
  const dismiss = (restoreFocus = false) => {
    close(restoreFocus);
    onDismiss();
  };
  const open = () => {
    if (!enabled()) return;
    panel.hidden = false;
    trigger.setAttribute("aria-expanded", "true");
    document.addEventListener("mousedown", outside);
  };
  trigger.addEventListener("click", () => panel.hidden ? open() : dismiss());
  el.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && enabled() && !panel.hidden) {
      event.preventDefault();
      event.stopPropagation();
      dismiss(true);
    }
  });
  return { el, panel, trigger, open, close, dismiss, dispose: close };
}

/** Opens the panel above its trigger when the space below, inside the nearest
 * clipping ancestor and the viewport, cannot hold it and the space above is larger. */
export function placeMenu(trigger: HTMLElement, panel: HTMLElement): void {
  panel.classList.remove("af-menu-up");
  const anchor = trigger.getBoundingClientRect();
  let top = 0;
  let bottom = window.innerHeight;
  for (let node = trigger.parentElement; node; node = node.parentElement) {
    const overflow = getComputedStyle(node).overflowY;
    if (overflow === "visible" || overflow === "") continue;
    const clip = node.getBoundingClientRect();
    top = Math.max(top, clip.top);
    bottom = Math.min(bottom, clip.bottom);
  }
  const box = panel.getBoundingClientRect();
  const needed = box.height + Math.max(0, box.top - anchor.bottom);
  const below = bottom - anchor.bottom;
  const above = anchor.top - top;
  if (needed > below && above > below) panel.classList.add("af-menu-up");
}

/** The app's secondary controls use the same disclosure and Escape/focus model.
 * Desktop displays the same nodes inline; resizing closes any phone disclosure. */
export function appbarControls(
  controls: HTMLElement[],
  phone = window.matchMedia("(max-width: 768px)"),
  beforeSync: () => void = () => {},
  onDismiss: () => void = () => {},
  emptyFocus: () => HTMLElement | null = () => null,
) {
  const menu = actionsDisclosure("More app controls", () => phone.matches, onDismiss);
  menu.el.className = "af-appbar-tools-wrap";
  menu.trigger.className = "af-appbar-more";
  menu.trigger.replaceChildren(icon("ellipsis"));
  menu.trigger.title = "More app controls";
  menu.trigger.setAttribute("aria-controls", "af-appbar-tools");
  menu.panel.className = "af-appbar-tools";
  menu.panel.id = "af-appbar-tools";
  menu.panel.setAttribute("aria-label", "App controls");
  menu.panel.append(...controls);
  const hasContent = () => Array.from(menu.panel.children).some((child) => !(child as HTMLElement).hidden);
  const syncTrigger = () => {
    const empty = !hasContent();
    const strand = empty && menu.trigger.getAttribute("aria-expanded") === "true";
    const hadFocus = strand && menu.panel.contains(document.activeElement);
    if (strand) menu.close();
    menu.trigger.hidden = !phone.matches || empty;
    if (hadFocus) emptyFocus()?.focus();
  };
  const sync = () => {
    beforeSync();
    const hadFocus = menu.panel.contains(document.activeElement);
    menu.close();
    syncTrigger();
    menu.panel.hidden = phone.matches;
    if (phone.matches && hadFocus && !menu.trigger.hidden) menu.trigger.focus();
  };
  // Children move in and out (session-first composition) and toggle `hidden`
  // (install), so an empty More menu can appear at any time.
  const observer = typeof MutationObserver === "undefined" ? null : new MutationObserver(syncTrigger);
  observer?.observe(menu.panel, { childList: true, subtree: true, attributes: true, attributeFilter: ["hidden"] });
  phone.addEventListener("change", sync);
  sync();
  return {
    ...menu,
    dispose: () => { observer?.disconnect(); phone.removeEventListener("change", sync); menu.dispose(); },
  };
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

/** The terminal head's title element is owned by the title alone: its text, its
 *  tooltip (which exists so a TRUNCATED title stays readable, #2224/#3981) and its
 *  aria-label are all the session title. One writer, shared by the initial render
 *  and every patch. */
export function applyTerminalTitle(el: Pick<HTMLElement, "textContent" | "title" | "setAttribute">, title: string): void {
  el.textContent = title;
  el.title = title;
  el.setAttribute("aria-label", title);
}

/** Where the session runs (#5571 W6.4) has its OWN element and tooltip, in the
 *  Actions panel (a details surface), so it cannot collide with the head's title,
 *  tab and meta contracts (#2458, #2224, #3981). Shown only for an off-box
 *  session, like the rail row: a local session says nothing extra. `detail` is
 *  placementDetail(); "" or a local placement hides the element. */
export function applyTerminalPlacement(el: Pick<HTMLElement, "textContent" | "title" | "hidden">, detail: string, remote: boolean): void {
  const shown = remote && detail !== "";
  el.hidden = !shown;
  el.textContent = shown ? detail : "";
  el.title = shown ? `Where this session runs: ${detail}` : "";
}

/** One stable title/tab row; patching it never reparents a terminal. */
export function terminalChrome(opts: { title: string; placement?: { detail: string; remote: boolean }; copyLink(): void; handoff(): void; retry(): void; markDelivered(): void; clearAttention(): void; newTab(kind: "shell" | "vscode"): void; closePane?(): void; fontSizeStep?(delta: number): void }) {
  const menu = actionsDisclosure("More session actions");
  menu.trigger.replaceChildren(icon("ellipsis"));
  menu.trigger.title = "More session actions";
  const action = (label: string, className: string, run: () => void) => {
    const button = h("button", { type: "button", class: `af-ghost af-term-action ${className}` }, label);
    button.addEventListener("click", () => { menu.close(); run(); });
    return button;
  };
  const newTabButton = (kind: "shell" | "vscode", label: string, glyph: "terminal" | "panels", name: string) => {
    const button = h("button", { type: "button", class: `af-ghost af-term-action af-term-new af-term-new-${kind}`, title: name },
      icon(glyph), h("span", { class: "af-term-new-label" }, label));
    button.setAttribute("aria-label", name);
    button.hidden = true;
    // The new tab owns input next; a focused button would swallow the rail keys.
    // detail counts clicks in one gesture; a fast local create can settle before the
    // second click of a double-click lands, so the in-flight guard alone misses it.
    // The first create may already have focused its terminal; a focusing mousedown
    // here would blur it back to rail mode.
    button.addEventListener("mousedown", (event) => {
      if (event.detail > 1) event.preventDefault();
    });
    button.addEventListener("click", (event) => {
      if (event.detail > 1) return;
      menu.close(); button.blur(); opts.newTab(kind);
    });
    return button;
  };
  const newTerminal = newTabButton("shell", "Terminal", "terminal", "New terminal tab");
  const newVSCode = newTabButton("vscode", "VS Code", "panels", "New VS Code tab");
  const docsShortcuts = h("div", { class: "af-term-docs-group" });
  const newTabs = h("div", { class: "af-term-new-group" }, docsShortcuts, newTerminal, newVSCode);
  const title = h("span", { class: "af-term-title" });
  applyTerminalTitle(title, opts.title);
  const placement = h("span", { class: "af-term-placement" });
  applyTerminalPlacement(placement, opts.placement?.detail ?? "", opts.placement?.remote ?? false);
  const titleBox = h("div", { class: "af-term-head-main" }, title, h("span", { class: "af-term-title-separator", ariaHidden: "true" }, " · "));
  const tabs = h("div", { class: "af-tabbar", role: "tablist" });
  tabs.setAttribute("aria-label", "Session tabs");
  const actions = h("div", { class: "af-term-actions" });
  actions.hidden = true;
  const retry = action("Retry limit", "", opts.retry);
  retry.title = "Retry after the usage limit";
  // The no-resend half of resolving an ambiguous handoff delivery (#4429):
  // separate button, separate verb — never a silent branch inside Retry.
  const deliver = action("Mark delivered", "", opts.markDelivered);
  deliver.title = "Retire the pending handoff mission without resending — the pane already shows it landed";
  deliver.hidden = true;
  const handoff = action("Handoff", "", opts.handoff);
  handoff.title = "Continue this session under another agent or account";
  const copy = action("Copy link", "af-copy-link", opts.copyLink);
  copy.title = "Copy link to this session";
  // The desktop font-size pair (#5500): terminal text only, beside the other
  // pane-level controls and hidden at the phone breakpoint (where the keybar
  // carries the same controls instead). Each press steps the shared device
  // class's size — every pane in the page refits, which is correct because the
  // value is per class, not per pane.
  const fontDecrease = action("A−", "af-term-zoom", () => opts.fontSizeStep?.(-1));
  fontDecrease.title = "Decrease terminal text size";
  fontDecrease.setAttribute("aria-label", "Decrease terminal text size");
  const fontIncrease = action("A+", "af-term-zoom", () => opts.fontSizeStep?.(1));
  fontIncrease.title = "Increase terminal text size";
  fontIncrease.setAttribute("aria-label", "Increase terminal text size");
  const tabUnavailable = h("span", { class: "af-tab-new-unavailable" });
  tabUnavailable.hidden = true;
  const closePane = action("Hide pane", "af-phone-pane-close", () => opts.closePane?.());
  closePane.hidden = true;
  menu.panel.append(placement, tabUnavailable, copy, handoff, deliver, actions, closePane);
  const attention = attentionChip(opts.clearAttention);
  const head = h("div", { class: "af-term-head" }, titleBox, tabs, newTabs, attention.el, fontDecrease, fontIncrease, retry, menu.el);
  return { head, title, placement, tabs, attention, retry, deliver, handoff, closePane, actions, newTabs, newTerminal, newVSCode, docsShortcuts, tabUnavailable, menu, dispose: menu.dispose };
}

/** The agent's open request for the user. Only the × clears it, so looking at the
 *  session or focusing its pane never dismisses it by accident. */
export function attentionChip(onDismiss: () => void) {
  const label = h("span", { class: "af-term-attention-label" }, "Attention");
  const dismiss = h("button", { type: "button", class: "af-term-attention-dismiss", title: "Dismiss attention request" }, icon("x"));
  dismiss.setAttribute("aria-label", "Dismiss attention request");
  dismiss.addEventListener("click", (event) => { event.stopPropagation(); dismiss.blur(); onDismiss(); });
  const el = h("div", { class: "af-term-attention", role: "status" }, icon("bell", "af-term-attention-bell"), label, dismiss);
  el.hidden = true;
  const patch = (on: boolean, reason: string) => {
    el.hidden = !on;
    const title = reason ? `Needs attention: ${reason}` : "Needs attention";
    if (el.title !== title) {
      el.title = title;
      // The phone app bar shows the bell and × only, so the words live here.
      el.setAttribute("aria-label", title);
    }
  };
  return { el, dismiss, patch };
}

/** Split leaves share the same title/close treatment as the main tab row. */
export function paneChrome(onClose: () => void) {
  const glyph = h("span", { class: "af-pane-glyph", ariaHidden: "true" });
  const label = h("span", { class: "af-pane-label" });
  const close = h("button", { type: "button", class: "af-pane-close", title: "Hide pane" }, icon("x"));
  close.setAttribute("aria-label", "Hide pane");
  close.addEventListener("click", (event) => { event.stopPropagation(); onClose(); });
  return { head: h("div", { class: "af-pane-head" }, glyph, label, close), glyph, label };
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
 *  around the async submit. close() removes it from the DOM. reactivate() reinstalls
 *  the document-level Tab trap after a retained handle is remounted by openModal
 *  without rebuilding it — an intervening modalChrome reaped this handle's trap, so
 *  the reopened error dialog would otherwise have no Tab containment. */
export interface ModalHandle {
  el: HTMLElement;
  setBusy(busy: boolean): void;
  setError(msg: string | null): void;
  close(): void;
  reactivate(): void;
}

// The single live document-level Tab trap. modalChrome reaps the previous trap
// when a new modal opens (mountOverlay closes any current modal first, so at
// most one is live). The trap is NOT removed in close() — a refused RPC remounts
// the same handle, and a keydown during that detached window must not strand it —
// so the previous modal's trap is reclaimed here, when the next modal takes over.
let activeModalTrap: ((event: KeyboardEvent) => void) | null = null;

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
  // Keep native Tab navigation within the currently enabled, visible controls.
  // Re-query on each keypress: errors, disclosures and busy state can change them.
  //
  // The trap is attached at the document level (capture phase), not the card, so it
  // still fires once focus has escaped to <body> — the state setBusy creates when it
  // disables a footer button that had focus: the browser moves
  // document.activeElement to <body>, and a Tab keydown there targets <body> and
  // bubbles body → html → document, never through the card (a descendant of body).
  // A card-level trap cannot observe that event, so its rescue branch
  // (!card.contains(document.activeElement)) was dead exactly when it was needed.
  // The document-level listener sees every keydown while the modal is open, gates
  // itself on the backdrop still being connected, and is NOT removed in close(): a
  // refused create/kill/archive RPC closes this modal (close() detaches the
  // backdrop) and then remounts the same handle (index.ts openModal) once the RPC
  // settles — the trap must survive that detached window so the reopened error
  // dialog keeps its Tab containment. The guard below therefore stays registered
  // and inert while the backdrop is detached; it does NOT self-remove on a
  // keydown during that window, because that would strand the remount. Final
  // disposal is the swap in modalChrome below: the next modal to open reaps the
  // previous active trap, so at most one trap is live at a time and the last
  // modal's trap lingers inert (its guard skips every event) after close if no
  // other modal opens. Capture runs before the browser's default Tab focus shift,
  // so the rescue can preventDefault and pull focus back inside before it leaves.
  const backdrop = h("div", { class: "af-modal-backdrop" }, card);
  backdrop.addEventListener("click", () => opts.onCancel());
  const trap = (event: KeyboardEvent) => {
    if (!backdrop.isConnected) return;
    if (event.key !== "Tab") return;
    const controls = [...card.querySelectorAll<HTMLElement>(
      'button, input, select, textarea, a[href], summary, [tabindex]',
    )].filter(control => control.tabIndex >= 0 && !control.matches(":disabled") && control.getClientRects().length > 0);
    const first = controls[0], last = controls.at(-1);
    if (!first) {
      event.preventDefault();
      card.focus();
    } else if (!card.contains(document.activeElement) || document.activeElement === card ||
      (event.shiftKey ? document.activeElement === first : document.activeElement === last)) {
      event.preventDefault();
      (event.shiftKey ? last! : first).focus();
    }
  };
  // Reap the previous modal's trap (if any) and install this one. A new modal
  // always opens through mountOverlay, which closes the current modal first, so
  // this swap is the disposal point: it bounds the live traps to one and reclaims
  // a retained handle's trap once a different modal has taken over.
  const installTrap = () => {
    if (activeModalTrap) document.removeEventListener("keydown", activeModalTrap, true);
    document.addEventListener("keydown", trap, true);
    activeModalTrap = trap;
  };
  installTrap();
  // Stop a click inside the card from bubbling to the backdrop's cancel handler.
  card.addEventListener("click", (e) => e.stopPropagation());

  cancelBtn.addEventListener("click", () => opts.onCancel());

  const handle: ModalHandle = {
    el: backdrop,
    setBusy(busy: boolean) {
      // Move focus off a footer button BEFORE disabling it: a focused button that
      // becomes disabled drops document.activeElement to <body>, escaping the
      // dialog's Tab trap (see the document-level trap above). Only relocate when a
      // footer button actually has focus, so a user typing into a field mid-RPC
      // keeps the cursor while the in-flight RPC disables the submit/cancel pair.
      if (busy && card.isConnected && (document.activeElement === confirmBtn || document.activeElement === cancelBtn)) {
        card.focus();
      }
      confirmBtn.disabled = busy;
      cancelBtn.disabled = busy;
      card.classList.toggle("af-modal-busy", busy);
    },
    setError(msg: string | null) {
      if (msg) {
        errorLine.replaceChildren(mutationNotice(`${opts.title} failed`, msg, `Check the error, then ${opts.confirmLabel.toLowerCase()} again.`));
        errorLine.hidden = false;
        // If focus escaped the dialog (a footer button was disabled while focused,
        // or some other path left activeElement outside the card), pull it back
        // inside so Tab stays contained while a refused attempt keeps the modal open
        // with an error. Leaves in-dialog focus — a field the user is editing —
        // alone, so a validation error does not yank the cursor out of its input.
        if (card.isConnected && !card.contains(document.activeElement)) {
          card.focus();
        }
      } else {
        errorLine.textContent = "";
        errorLine.hidden = true;
      }
    },
    close() {
      // The backdrop detaches here. The trap is NOT removed: a refused
      // create/kill/archive RPC closes this modal and then remounts the same
      // handle (index.ts openModal), and removing it here — or self-removing on
      // a keydown during the detached RPC window — would leave the reopened
      // error dialog with no Tab containment. The trap stays registered and is
      // inert while the backdrop is detached (the guard above skips the event
      // without removing itself); the next modalChrome reclaims it when a new
      // modal opens.
      backdrop.remove();
    },
    reactivate() {
      // Reinstall this handle's trap after a retained handle is remounted by
      // openModal WITHOUT rebuilding it. modalChrome reaped this trap when an
      // intervening modal opened (it swaps the single live trap to bound the
      // count), so a refused RPC that closes modal A, lets the user open and
      // close modal B in between, and then remounts A would otherwise leave A's
      // reopened error dialog with no Tab containment: A's backdrop is
      // reconnected but its trap was removed by modalChrome(B) and never
      // reinstalled. openModal calls this on every (re)mount; for a fresh modal
      // modalChrome already installed this trap, so this reaps and re-adds the
      // same listener (a no-op), and for a remount it reclaims the live slot.
      installTrap();
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
  const summary = h("summary", {}, h("span", { class: "af-defaults-fragment" }, "Defaults ·"), " ", summaryText);
  const body = h("div", { class: "af-defaults-body" });
  const el = h("details", { class: "af-defaults" }, summary, body);
  const setSummary = (fragments: string[]) => {
    summaryText.replaceChildren(...fragments.flatMap((fragment, index) =>
      [h("span", { class: "af-defaults-fragment" }, `${fragment}${index < fragments.length - 1 ? " ·" : ""}`), " "]));
  };
  return { el, body, setSummary };
}
