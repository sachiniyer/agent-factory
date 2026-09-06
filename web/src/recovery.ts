// P4 recovery screens. Callers own transport and retain their existing form nodes.
import { h } from "./dom.js";
import { currentMode } from "./theme.js";

export interface Recovery {
  condition: string;
  detail?: string;
  action: string;
  run: () => void;
  failed?: boolean;
}

export function recoveryScreen(state: Recovery): HTMLElement {
  const action = h("button", { type: "button", class: "af-recovery-action" }, state.action);
  action.addEventListener("click", state.run);
  const screen = h("section", { class: "af-recovery" },
    h("h1", { class: state.failed ? "af-recovery-title af-recovery-failed" : "af-recovery-title" }, state.condition),
    ...(state.detail ? [h("p", { class: "af-recovery-detail" }, state.detail)] : []),
    action,
  );
  scopeRecovery(screen);
  if (state.failed) screen.setAttribute("role", "alert");
  return screen;
}

/** An inline failure has one recovery instruction; the existing submit owns retry. */
export function mutationNotice(condition: string, detail: string, action: string, failed = true): HTMLElement {
  return scopeRecovery(h("div", { class: "af-recovery af-recovery-notice", role: "alert" },
    h("strong", { class: failed ? "af-recovery-failed" : "" }, condition),
    h("p", { class: "af-recovery-detail" }, detail),
    h("p", { class: "af-recovery-next" }, action),
  ));
}

export function scopeRecovery<T extends HTMLElement>(element: T): T {
  element.setAttribute("data-af-theme", currentMode());
  return element;
}
