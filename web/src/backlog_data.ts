// Loads every project's backlog for the Backlog view, the Sessions view's item
// chips and the tab's needs-you badge. A backlog.changed frame names only the ids a
// write directly touched and never a parent whose derived state moved with them, so
// the ids are not read: any frame re-reads GetBacklog, and an empty ids list (a
// project-level change such as the runner settings) is the same refresh. A frame
// also names its project by repo id, which the web cannot map to every root it
// knows, so every project is re-read; there are few. The stream's reconnect resync
// and every focus of the view re-read too, and a refresh reads the budget, which
// publishes no event of its own. backlog.spend (not yet emitted) re-reads only it.

import { errorText } from "./api.js";
import { getBacklog, getBudget } from "./backlog_api.js";
import type { BacklogView, Budget } from "./backlog_model.js";

export interface BacklogData {
  views: ReadonlyMap<string, BacklogView>;
  errors: ReadonlyMap<string, string>;
  budget: Budget | null;
  /** The loader refresh these views came from; a later refresh started later. */
  generation: number;
}

export const EMPTY_BACKLOG_DATA: BacklogData = { views: new Map(), errors: new Map(), budget: null, generation: 0 };

const DEBOUNCE_MS = 150;

/** The sessions that item runs point at: the ones whose lifecycle moves an item's
 *  derived state and the runner's slot count without any backlog write. */
export function runSessionIds(data: BacklogData): Set<string> {
  const ids = new Set<string>();
  for (const view of data.views.values()) {
    for (const it of view.backlog.items) {
      for (const run of it.runs ?? []) if (run.session_id) ids.add(run.session_id);
    }
  }
  return ids;
}

const SESSION_EVENTS: ReadonlySet<string> = new Set(["session.updated", "session.killed", "session.archived", "session.restored"]);

/** What a wire event asks of the loader. The frame's ids are a hint and are never
 *  read: a parent's derived state moves without being named, and an empty list is a
 *  project-level change, so every backlog.changed and backlog.runner frame is a full
 *  re-read. backlog.spend moves only the budget. A session event for a session an
 *  item run points at is a refresh too: killing, archiving or limiting it changes the
 *  item's state and the slot count with no backlog frame. Other sessions' events are
 *  not the backlog's business. */
export function backlogEventAction(type: string, sessionId?: string, runSessions?: ReadonlySet<string>): "refresh" | "budget" | null {
  if (type === "backlog.changed" || type === "backlog.runner") return "refresh";
  if (type === "backlog.spend") return "budget";
  if (SESSION_EVENTS.has(type) && sessionId && runSessions?.has(sessionId)) return "refresh";
  return null;
}

export interface BacklogLoaderOptions {
  token(): string | null;
  roots(): string[];
  commit(data: BacklogData): void;
  current(): BacklogData;
}

export class BacklogLoader {
  private generation = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private budgetTimer: ReturnType<typeof setTimeout> | null = null;
  private budgetGeneration = 0;

  constructor(private readonly opts: BacklogLoaderOptions) {}

  /** The newest refresh started so far. */
  started(): number {
    return this.generation;
  }

  /** Refetches every project. A later call wins over one still in flight. Settles true
   *  when the read of `root` (every project's, when none is named) succeeded: a read
   *  that failed leaves the old snapshot in place, and a write that was only
   *  followed by such a read has not been confirmed. */
  async refresh(root?: string): Promise<boolean> {
    const token = this.opts.token();
    if (token === null) return false;
    const gen = ++this.generation;
    const budgetGen = ++this.budgetGeneration;
    const roots = [...new Set(this.opts.roots().filter(Boolean))];
    const [results, budget] = await Promise.all([
      Promise.all(roots.map((root) => getBacklog(root, token)
        .then((view) => ({ root, view, error: "" }))
        .catch((e: unknown) => ({ root, view: null, error: errorText(e) })))),
      getBudget(token).catch(() => null),
    ]);
    const read = root === undefined || !results.some((r) => r.root === root)
      ? results.every((r) => r.view !== null)
      : results.some((r) => r.root === root && r.view !== null);
    if (this.opts.token() !== token) return false;
    if (gen !== this.generation) return read;   // a newer refresh commits; this read was still after the write
    const cur = this.opts.current();
    const prev = cur.views;
    const views = new Map<string, BacklogView>();
    const errors = new Map<string, string>();
    for (const r of results) {
      if (r.view) views.set(r.root, r.view);
      else {
        errors.set(r.root, r.error);
        const old = prev.get(r.root);
        if (old) views.set(r.root, old);
      }
    }
    this.opts.commit({ views, errors, budget: budgetGen === this.budgetGeneration ? budget ?? cur.budget : cur.budget, generation: gen });
    return read;
  }

  /** Refetches only the budget, for backlog.spend. */
  async refreshBudget(): Promise<void> {
    const token = this.opts.token();
    if (token === null) return;
    const gen = ++this.budgetGeneration;
    const budget = await getBudget(token).catch(() => null);
    if (gen !== this.budgetGeneration || this.opts.token() !== token || !budget) return;
    this.opts.commit({ ...this.opts.current(), budget });
  }

  /** Coalesces a burst of events into one refetch. */
  request(): void {
    if (this.timer !== null) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.refresh();
    }, DEBOUNCE_MS);
  }

  requestBudget(): void {
    if (this.budgetTimer !== null) return;
    this.budgetTimer = setTimeout(() => {
      this.budgetTimer = null;
      void this.refreshBudget();
    }, DEBOUNCE_MS);
  }

  reset(): void {
    this.generation++;
    this.budgetGeneration++;
    if (this.timer !== null) clearTimeout(this.timer);
    if (this.budgetTimer !== null) clearTimeout(this.budgetTimer);
    this.timer = null;
    this.budgetTimer = null;
  }
}
