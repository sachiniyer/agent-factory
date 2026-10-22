import { hasAttention } from "./status.js";
import type { SessionData } from "./types.js";

export interface AttentionView {
  phase: "login" | "app";
  view: string;
  sessions: SessionData[];
  selectedId: string | null;
  shownTabs: number[];
}

/** The id of the session whose attention request the user is now looking at:
 *  selected, its agent tab (index 0) shown in a pane, and the pane exposed. */
export function attentionSeen(state: AttentionView, paneExposed: boolean): string | null {
  if (!paneExposed || state.phase !== "app" || state.view !== "sessions" || state.selectedId === null) {
    return null;
  }
  if (!state.shownTabs.includes(0)) {
    return null;
  }
  const s = state.sessions.find((x) => x.id === state.selectedId);
  return s && hasAttention(s) ? state.selectedId : null;
}

/** The parts of `document` that say whether the agent pane is in front of the user. */
export interface PageView {
  visibilityState: DocumentVisibilityState;
  hasFocus(): boolean;
  querySelector(selector: string): unknown;
}

/** True when the page is visible and focused, and neither a modal dialog nor the
 *  open session drawer covers the pane. The drawer covers it only on a narrow
 *  viewport; on a wide one the rail sits beside the pane. */
export function agentPaneExposed(doc: PageView, narrow: boolean): boolean {
  return (
    doc.visibilityState === "visible" &&
    doc.hasFocus() &&
    doc.querySelector('[aria-modal="true"]') === null &&
    !(narrow && doc.querySelector(".af-app.af-nav-open") !== null)
  );
}

/** Sends one clear per attention request, keyed by its set_at, so repaints while
 *  the request is in flight do not repeat it. The clear names that set_at, so it
 *  cannot remove a newer request. A failed clear may be retried. */
export class AttentionClearer {
  private sent = new Set<string>();

  constructor(private readonly clear: (id: string, setAt: string) => Promise<void>) {}

  note(state: AttentionView, paneExposed: boolean): void {
    const id = attentionSeen(state, paneExposed);
    if (id === null) {
      return;
    }
    const setAt = state.sessions.find((x) => x.id === id)?.attention?.set_at ?? "";
    const key = `${id}@${setAt}`;
    if (this.sent.has(key)) {
      return;
    }
    this.sent.add(key);
    this.clear(id, setAt).catch(() => this.sent.delete(key));
  }
}
