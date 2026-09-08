import { h } from "./dom.js";
import { OPERATOR_KIND_LABELS, operatorKind, rowStatus } from "./status.js";
import type { SessionData } from "./types.js";

/** Only projected identity: an absent account means the ambient default, never
 * the viewer's login. Do not turn an unknown agent or missing repo into a guess. */
export function sessionIdentity(s: SessionData): string {
  return `${s.current_agent || "Agent not reported"} · ${s.account || "Default account"}`;
}

export function patchSessionIdentity(node: HTMLElement, s: SessionData): void {
  const state = h("span", { class: "af-session-state" }, OPERATOR_KIND_LABELS[operatorKind(s)]);
  state.dataset.state = rowStatus(s).kind ?? "working";
  const owner = h("span", { class: "af-session-owner" }, sessionIdentity(s));
  const repo = s.worktree?.repo_path;
  const location = [repo?.replace(/\/+$/, "").split("/").pop(), s.branch].filter(Boolean).join(" · ");
  const work = h("span", { class: "af-session-location", title: [repo, s.branch].filter(Boolean).join(" · ") }, location);
  node.replaceChildren(state, owner, work);
  node.title = [state.textContent, owner.textContent, work.title].filter(Boolean).join(" · ");
}
