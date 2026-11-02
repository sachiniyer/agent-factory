// The effort session controls (effort orchestrator proposal, ui.md): the group
// card's "Effort session" button, the group panel's Orchestrator row actions,
// and the "With effort session" tag on an item whose question waits for it.

import { errorText } from "./api.js";
import { openOrchestrator, unbindOrchestrator } from "./backlog_api.js";
import { type BacklogView, type Item, type OrchestratorView, formatID, itemByID, orchestratorQuestion } from "./backlog_model.js";
import { h } from "./dom.js";
import { icon } from "./icon.js";
import type { SessionData } from "./types.js";

export const WITH_EFFORT_SESSION = "With effort session";

export interface EffortHost {
  token(): string | null;
  /** Re-reads the backlogs; settles true when the read of `root` succeeded. */
  refresh(root?: string): Promise<boolean>;
  notify(message: string): void;
  openSession(id: string): void;
  confirm(title: string, body: string, confirmLabel: string): Promise<boolean>;
}

/** The group's orchestrator with its session state, or undefined when the
 *  group has none. A binding GetBacklog has no view for yet reads as gone. */
export function orchestratorOf(view: BacklogView, group: number): OrchestratorView | undefined {
  const o = itemByID(view.backlog, group)?.orchestrator;
  if (!o) return undefined;
  return view.orchestrators?.[String(group)] ?? { ...o, state: "gone", pending: 0 };
}

export function effortLabel(view: BacklogView, group: number): string {
  return orchestratorOf(view, group) ? "Effort session" : "Start effort session";
}

/** What the button reads while af opens the session. */
export function effortProgress(view: BacklogView, group: number): string {
  const o = orchestratorOf(view, group);
  if (!o) return "Starting effort session…";
  if (o.state === "gone") return "Replacing effort session…";
  if (o.state === "archived" || o.state === "lost" || o.state === "dead") return "Restoring effort session…";
  return "Opening effort session…";
}

const STATE_TEXT: Record<string, string> = {
  live: "Live", archived: "Archived", lost: "Lost", dead: "Dead", limited: "Usage limit", gone: "Gone",
};

export function orchestratorStateText(state: string): string {
  return STATE_TEXT[state] ?? state;
}

/** "With effort session" on an item whose open question waits for its
 *  group's orchestrator, unless the state chip already says so. */
export function routedTag(view: BacklogView, it: Item): HTMLElement[] {
  if (!orchestratorQuestion(view.backlog, it)) return [];
  if (view.states[String(it.id)]?.reason === WITH_EFFORT_SESSION) return [];
  return [h("span", { class: "af-backlog-tag af-backlog-effort-tag" }, WITH_EFFORT_SESSION)];
}

/** How long a session af just started may take to reach the session list
 *  before af stops waiting to open it, so a late one never steals focus. */
export const WAIT_LISTED_MS = 30_000;

/** Opens and unbinds groups' effort sessions. A session af just started may
 *  reach the session list after the reply; it opens once it is listed, within
 *  WAIT_LISTED_MS and while the user stays on the project. A reply that comes
 *  after the user moved to another project opens nothing. */
export class EffortSessions {
  private readonly busy = new Map<string, string>();
  private waiting: string | null = null;
  private waitTimer: ReturnType<typeof setTimeout> | null = null;
  private sessions: readonly SessionData[] = [];
  private project: string | null = null;

  constructor(private readonly host: EffortHost, private readonly rerender: () => void) {}

  /** The project the pane shows. Moving to another stops waiting. */
  showProject(root: string | null): void {
    if (root === this.project) return;
    this.project = root;
    this.stopWaiting();
  }

  sessionsChanged(sessions: readonly SessionData[]): void {
    this.sessions = sessions;
    const id = this.waiting;
    if (id && sessions.some((s) => s.id === id)) {
      this.stopWaiting();
      this.host.openSession(id);
    }
  }

  private stopWaiting(): void {
    this.waiting = null;
    if (this.waitTimer !== null) clearTimeout(this.waitTimer);
    this.waitTimer = null;
  }

  private waitFor(id: string): void {
    this.stopWaiting();
    this.waiting = id;
    this.waitTimer = setTimeout(() => this.stopWaiting(), WAIT_LISTED_MS);
  }

  button(root: string, view: BacklogView, group: Item): HTMLButtonElement {
    const progress = this.busy.get(`${root}\n${group.id}`);
    const bound = !!group.orchestrator;
    const label = progress ?? effortLabel(view, group.id);
    const b = h("button", { type: "button", class: "af-ghost af-backlog-effort" },
      label, ...(progress || !bound ? [] : [icon("external-link")]));
    b.dataset.action = "effort";
    b.disabled = progress !== undefined;
    b.setAttribute("aria-label", `${label.replace(/…$/, "")} for ${formatID(group.id)}`);
    if (progress) b.setAttribute("aria-busy", "true");
    b.addEventListener("click", () => void this.open(root, view, group.id));
    return b;
  }

  async open(root: string, view: BacklogView, group: number): Promise<void> {
    const key = `${root}\n${group}`;
    const token = this.host.token();
    if (token === null || this.busy.has(key)) return;
    this.stopWaiting();
    this.busy.set(key, effortProgress(view, group));
    this.rerender();
    try {
      const r = await openOrchestrator(root, group, token);
      if (this.project !== root) return;
      if (this.sessions.some((s) => s.id === r.session_id)) this.host.openSession(r.session_id);
      else this.waitFor(r.session_id);
    } catch (e) {
      this.host.notify(errorText(e));
    } finally {
      this.busy.delete(key);
      this.rerender();
      await this.host.refresh(root);
    }
  }

  async unbind(root: string, group: number): Promise<void> {
    const ok = await this.host.confirm("Unbind effort session",
      `The session keeps running, but af stops sending it the events of ${formatID(group)}. Questions that wait for it go to the Inbox.`,
      "Unbind");
    const token = this.host.token();
    if (!ok || token === null) return;
    try {
      await unbindOrchestrator(root, group, token);
    } catch (e) {
      this.host.notify(errorText(e));
    }
    await this.host.refresh(root);
  }
}
