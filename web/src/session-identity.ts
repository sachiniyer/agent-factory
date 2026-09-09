import { h } from "./dom.js";
import { OPERATOR_KIND_LABELS, operatorKind, type OperatorKind } from "./status.js";
import type { SessionData } from "./types.js";

/** Only projected identity: an absent account means the ambient default, never
 * the viewer's login. Do not turn an unknown agent or missing repo into a guess. */
export function sessionIdentity(s: SessionData): string {
  return `${s.current_agent || "Agent not reported"} · ${s.account || "Default account"}`;
}

/** The header's label and color must share the same operator-level state. */
export function sessionOperatorState(s: SessionData): OperatorKind {
  return operatorKind(s);
}

export function patchSessionIdentity(node: HTMLElement, s: SessionData): void {
  const operator = sessionOperatorState(s);
  const state = h("span", { class: "af-session-state" }, OPERATOR_KIND_LABELS[operator]);
  state.dataset.state = operator;
  const owner = h("span", { class: "af-session-owner" }, sessionIdentity(s));
  const repo = s.worktree?.repo_path;
  const location = [repo?.replace(/\/+$/, "").split("/").pop(), s.branch].filter(Boolean).join(" · ");
  const work = h("span", { class: "af-session-location", title: [repo, s.branch].filter(Boolean).join(" · ") }, location);
  node.replaceChildren(state, owner, work);
  node.title = [state.textContent, owner.textContent, work.title].filter(Boolean).join(" · ");
}
