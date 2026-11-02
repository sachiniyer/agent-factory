// Loads every project's backlog for the Backlog view, the Sessions view's item
// chips and the tab's needs-you badge. A backlog.changed frame names only the ids a
// write directly touched and never a parent whose derived state moved with them, so
// the ids are not read: any frame re-reads GetBacklog, and an empty ids list (a
// project-level change such as the runner settings) is the same refresh. A frame
// also names its project by repo id, which the web cannot map to every root it
// knows, so every project is re-read; there are few. The stream's reconnect resync
// and every focus of the view re-read too, and a refresh reads the budget, which
// publishes no event of its own. backlog.spend carries the plan use itself, so it applies without a refetch.

import { errorText } from "./api.js";
import { type BudgetReply, getBacklog, getBudget } from "./backlog_api.js";
import type { BacklogView, Budget } from "./backlog_model.js";
import { type Usage, applySpend, spendHasUnmeterable } from "./backlog_usage.js";

export interface BacklogData {
  views: ReadonlyMap<string, BacklogView>;
  errors: ReadonlyMap<string, string>;
  budget: Budget | null;
  usage: Usage | null;
  /** The loader refresh these views came from; a later refresh started later. */
  generation: number;
}

export const EMPTY_BACKLOG_DATA: BacklogData = { views: new Map(), errors: new Map(), budget: null, usage: null, generation: 0 };

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
 *  re-read. backlog.spend carries the plan use and is applied in place. A session event for a session an
 *  item run points at is a refresh too: killing, archiving or limiting it changes the
 *  item's state and the slot count with no backlog frame. Other sessions' events are
 *  not the backlog's business. */
export function backlogEventAction(type: string, sessionId?: string, runSessions?: ReadonlySet<string>): "refresh" | "spend" | null {
  if (type === "backlog.changed" || type === "backlog.runner") return "refresh";
  if (type === "backlog.spend") return "spend";
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
  private budgetGeneration = 0;
  private spendGeneration = 0;
  private accountsTimer: ReturnType<typeof setTimeout> | null = null;
  // Whether a backlog.spend since the last GetBudget reply named the
  // unmeterable runs; an older daemon's payload does not.
  private spendUnmeterable = false;

  constructor(private readonly opts: BacklogLoaderOptions) {}

  /** The newest refresh started so far. */
  started(): number {
    return this.generation;
  }

  /** Refetches every project. A later call wins over one still in flight. Settles true
   *  when a snapshot that read `root` (every project, when none is named) was committed
   *  after this call: a read that failed leaves the old snapshot in place, and a write
   *  that was only followed by such a read has not been confirmed. A refresh that a newer
   *  one superseded settles with the newer one's outcome, since its own result was
   *  discarded. */
  refresh(root?: string): Promise<boolean> {
    const run = this.run();
    this.tail = run;
    return run.then((ok) => ok.size === 0 ? false : root === undefined || !ok.has(root) ? [...ok.values()].every(Boolean) : ok.get(root) === true);
  }

  /** The newest refresh started so far, settled with whether each project's read landed in a committed snapshot. */
  private tail: Promise<Map<string, boolean>> = Promise.resolve(new Map());

  /** How many refreshes have started reading: a refresh a reset made stale, with none
   *  started after it, is the tail itself and has nothing newer to wait for. */
  private refreshesStarted = 0;

  private async run(): Promise<Map<string, boolean>> {
    const token = this.opts.token();
    if (token === null) return new Map();
    const mine = ++this.refreshesStarted;
    const gen = ++this.generation;
    const budgetGen = ++this.budgetGeneration;
    const spendGen = this.spendGeneration;
    const roots = [...new Set(this.opts.roots().filter(Boolean))];
    const [results, budget] = await Promise.all([
      Promise.all(roots.map((root) => getBacklog(root, token)
        .then((view) => ({ root, view, error: "" }))
        .catch((e: unknown) => ({ root, view: null, error: errorText(e) })))),
      getBudget(token).catch(() => null),
    ]);
    if (this.opts.token() !== token) return new Map();
    // A newer refresh commits instead; this one is acknowledged only by what that one reads.
    // A reset with no newer refresh leaves nothing to wait for: waiting on the tail would
    // be waiting on this refresh itself (a promise chaining cycle).
    if (gen !== this.generation) return this.refreshesStarted === mine ? new Map() : this.tail;
    const ok = new Map(results.map((r) => [r.root, r.view !== null] as [string, boolean]));
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
    const fresh = budgetGen === this.budgetGeneration ? budget : null;
    let usage = cur.usage;
    if (fresh && spendGen === this.spendGeneration) {
      usage = fresh.usage;
      this.spendUnmeterable = false;
    } else if (fresh && cur.usage && !this.spendUnmeterable) {
      usage = { ...cur.usage, unmeterable: fresh.usage.unmeterable };
    }
    this.opts.commit({ views, errors, budget: fresh?.budget ?? cur.budget, usage, generation: gen });
    return ok;
  }

  /** Takes the budget and plan use a SetBudget reply carries. A GetBudget still in flight was read
   *  before the save, so its reply is dropped. */
  applyBudget(reply: BudgetReply): void {
    this.budgetGeneration++;
    this.opts.commit({ ...this.opts.current(), budget: reply.budget, usage: reply.usage });
  }

  /** Applies a backlog.spend payload. A GetBudget still in flight was read
   *  before it, so that reply's plan use is dropped. */
  applySpend(ev: unknown): void {
    const cur = this.opts.current();
    const usage = applySpend(cur.usage, ev);
    if (usage === cur.usage) return;
    this.spendGeneration++;
    if (spendHasUnmeterable(ev)) this.spendUnmeterable = true;
    this.opts.commit({ ...cur, usage });
    this.requestAccounts();
  }

  /** A spend event carries the top-level windows but not which account they are for, nor the
   *  other accounts' windows, and the account the daemon meters can change between events. So
   *  each burst of events is followed by one budget-only read, whose account identity and
   *  per-account windows replace the ones kept. */
  private requestAccounts(): void {
    if (this.accountsTimer !== null) return;
    this.accountsTimer = setTimeout(() => {
      this.accountsTimer = null;
      void this.refreshAccounts();
    }, DEBOUNCE_MS);
  }

  private async refreshAccounts(): Promise<void> {
    const token = this.opts.token();
    if (token === null) return;
    const budgetGen = this.budgetGeneration;
    const spendGen = this.spendGeneration;
    const fresh = await getBudget(token).catch(() => null);
    // A full refresh that started meanwhile commits its own budget read.
    if (!fresh || this.opts.token() !== token || budgetGen !== this.budgetGeneration) return;
    const cur = this.opts.current();
    let usage = fresh.usage;
    if (spendGen !== this.spendGeneration && cur.usage) {
      usage = { ...cur.usage, account: fresh.usage.account, accounts: fresh.usage.accounts };
    } else {
      this.spendUnmeterable = false;
    }
    this.opts.commit({ ...cur, budget: fresh.budget, usage });
  }

  /** Coalesces a burst of events into one refetch. */
  request(): void {
    if (this.timer !== null) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.refresh();
    }, DEBOUNCE_MS);
  }

  reset(): void {
    this.generation++;
    this.budgetGeneration++;
    this.spendGeneration++;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    if (this.accountsTimer !== null) clearTimeout(this.accountsTimer);
    this.accountsTimer = null;
  }
}
