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
 *  selected, its agent tab (index 0) shown in a pane, and the page in front. */
export function attentionSeen(state: AttentionView, pageInFront: boolean): string | null {
  if (!pageInFront || state.phase !== "app" || state.view !== "sessions" || state.selectedId === null) {
    return null;
  }
  if (!state.shownTabs.includes(0)) {
    return null;
  }
  const s = state.sessions.find((x) => x.id === state.selectedId);
  return s && hasAttention(s) ? state.selectedId : null;
}

/** Sends one clear per attention request, keyed by its set_at, so repaints while
 *  the request is in flight do not repeat it. A failed clear may be retried. */
export class AttentionClearer {
  private sent = new Set<string>();

  constructor(private readonly clear: (id: string) => Promise<void>) {}

  note(state: AttentionView, pageInFront: boolean): void {
    const id = attentionSeen(state, pageInFront);
    if (id === null) {
      return;
    }
    const s = state.sessions.find((x) => x.id === id);
    const key = `${id}@${s?.attention?.set_at ?? ""}`;
    if (this.sent.has(key)) {
      return;
    }
    this.sent.add(key);
    this.clear(id).catch(() => this.sent.delete(key));
  }
}
