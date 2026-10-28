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

/** An inline outcome notice has one instruction; only definitive failures invite retry. */
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

export interface MutationOutcomeNotice {
  kind: "uncertain" | "confirmed" | "failed";
  detail: string;
}

export function renderMutationOutcome(notice: MutationOutcomeNotice): HTMLElement {
  if (notice.kind === "uncertain") {
    return mutationNotice("Outcome not confirmed", notice.detail, "Check the session before acting.", false);
  }
  if (notice.kind === "confirmed") {
    return mutationNotice("Operation completed", notice.detail, "Review the result before acting.", false);
  }
  return mutationNotice("Operation failed", notice.detail, "Check the error, then retry.");
}

/** Keep overlapping outcomes readable without turning an unknown outcome into a retry invitation. */
export function appendMutationOutcome(previous: MutationOutcomeNotice | undefined, next: MutationOutcomeNotice): MutationOutcomeNotice {
  return {
    kind: previous?.kind === "uncertain" || next.kind === "uncertain" ? "uncertain"
      : previous?.kind === "confirmed" || next.kind === "confirmed" ? "confirmed" : "failed",
    detail: [previous?.detail, next.detail].filter(Boolean).join("\n\n"),
  };
}
