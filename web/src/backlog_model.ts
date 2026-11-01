// The Backlog view's data model: the wire shapes of the backlog routes
// (backlog/service_types.go) and the pure tree, filter, move and waits-on rules
// the view needs. No DOM and no I/O, so backlog_model.test.ts covers it directly.

export type Status = "draft" | "queued" | "running" | "review" | "done" | "dropped";

export type State =
  | "done" | "dropped" | "needs_you" | "in_review" | "working"
  | "starting" | "stopped" | "waiting" | "queued" | "draft";

export interface RunRef {
  session_id: string;
  title: string;
  state: string;
  prev_status: Status;
  started_by: string;
  started_at: string;
  /** The run's session branch, recorded when it starts. */
  branch?: string;
  /** The kept answers (delivery "new_run") this run's first prompt carries. */
  answers?: number[];
}

export interface Message {
  id: number;
  kind: "question" | "answer" | "note" | string;
  author: string;
  text: string;
  answers?: number;
  closed?: string;
  at: string;
  /** An answer's way to the agent; "new_run" and "superseded" do not close its question. */
  delivery?: string;
}

export interface Item {
  id: number;
  title: string;
  body?: string;
  status: Status;
  parent?: number;
  sequential?: boolean;
  waits_on?: number[];
  program?: string;
  source?: { proposal: string; key: string } | null;
  pr?: string;
  runs?: RunRef[];
  thread?: Message[];
  created_at: string;
  updated_at: string;
}

export interface Settings {
  runner: boolean;
  max_running: number;
  on_done: string;
}

export interface Backlog {
  schema_version: number;
  next_id: number;
  settings: Settings;
  items: Item[];
}

export interface Derived {
  state: State;
  reason?: string;
}

export interface RunnerView {
  state: "off" | "idle" | "running" | "waiting" | "stopped" | string;
  reason?: string;
  /** The closed gate when the runner is stopped or waiting: limit,
   *  usage_limit, no_reading, unmeterable, usd_limit, no_account, metering. */
  gate?: string;
  used: number;
  max: number;
}

export interface BacklogView {
  backlog: Backlog;
  states: Record<string, Derived>;
  runner: RunnerView;
}

/** The global budget: the plan % under which the runner starts items, the
 *  cap per run, and an optional dollar limit for accounts with no plan
 *  readings. */
export interface Budget {
  limit: number;
  max_per_run: number;
  usd_limit: number;
  usd_spent: number;
}

/** One of before, after, into, top, bottom: the MoveBacklogItem target. */
export type MoveTarget =
  | { before: number } | { after: number } | { into: number } | { top: true } | { bottom: true };

export const STATE_LABELS: Record<State, string> = {
  done: "Done",
  dropped: "Dropped",
  needs_you: "Needs you",
  in_review: "In review",
  working: "Working",
  starting: "Starting",
  stopped: "Stopped",
  waiting: "Waiting",
  queued: "Queued",
  draft: "Draft",
};

export const STATE_GLYPHS: Record<State, string> = {
  done: "✓",
  dropped: "×",
  needs_you: "?",
  in_review: "✎",
  working: "●",
  starting: "◌",
  stopped: "■",
  waiting: "⏸",
  queued: "▸",
  draft: "○",
};

export type Filter = "all" | "needs_you" | "in_review" | "working" | "queued" | "waiting" | "draft";

export const FILTERS: ReadonlyArray<{ filter: Filter; label: string }> = [
  { filter: "all", label: "All" },
  { filter: "needs_you", label: "Needs you" },
  { filter: "in_review", label: "In review" },
  { filter: "working", label: "Working" },
  { filter: "queued", label: "Queued" },
  { filter: "waiting", label: "Waiting" },
  { filter: "draft", label: "Draft" },
];

const FILTER_STATES: Record<Exclude<Filter, "all">, readonly State[]> = {
  needs_you: ["needs_you"],
  in_review: ["in_review"],
  working: ["working", "starting"],
  queued: ["queued"],
  waiting: ["waiting"],
  draft: ["draft"],
};

export function formatID(id: number): string {
  return `B-${id}`;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function arr<T>(v: unknown): T[] {
  return Array.isArray(v) ? (v as T[]) : [];
}

function parseItem(v: unknown): Item {
  const it = (isRecord(v) ? v : {}) as unknown as Item;
  return { ...it, waits_on: arr<number>(it.waits_on), runs: arr<RunRef>(it.runs), thread: arr<Message>(it.thread) };
}

/** Normalises a GetBacklog reply: Go's omitempty and nil slices become empty arrays. */
export function parseBacklogView(d: unknown): BacklogView {
  const v = isRecord(d) ? d : {};
  const b = isRecord(v.backlog) ? v.backlog : {};
  const settings = isRecord(b.settings) ? (b.settings as unknown as Settings) : { runner: false, max_running: 1, on_done: "keep" };
  const runner = isRecord(v.runner) ? (v.runner as unknown as RunnerView) : { state: "off", used: 0, max: 1 };
  return {
    backlog: {
      schema_version: Number(b.schema_version) || 0,
      next_id: Number(b.next_id) || 1,
      settings,
      items: arr<unknown>(b.items).map(parseItem),
    },
    states: isRecord(v.states) ? (v.states as Record<string, Derived>) : {},
    runner,
  };
}

export function itemByID(b: Backlog, id: number): Item | undefined {
  return b.items.find((it) => it.id === id);
}

export function childrenOf(b: Backlog, id: number): Item[] {
  return b.items.filter((it) => (it.parent ?? 0) === id);
}

export function isParent(b: Backlog, id: number): boolean {
  return b.items.some((it) => it.parent === id);
}

/** Whether anc is a strict ancestor of id. */
export function isAncestor(b: Backlog, anc: number, id: number): boolean {
  const seen = new Set<number>();
  for (let it = itemByID(b, id); it && it.parent && !seen.has(it.id); it = itemByID(b, it.parent)) {
    seen.add(it.id);
    if (it.parent === anc) return true;
  }
  return false;
}

export function depthOf(b: Backlog, id: number): number {
  let d = 0;
  const seen = new Set<number>();
  for (let it = itemByID(b, id); it && it.parent && !seen.has(it.id); it = itemByID(b, it.parent)) {
    seen.add(it.id);
    d++;
  }
  return d;
}

export function derivedOf(view: BacklogView, id: number): Derived {
  return view.states[String(id)] ?? { state: "draft" };
}

/** The item's oldest question that no answer names and that is not closed. */
export function openQuestion(it: Item): Message | undefined {
  // An answer to a gone session starts a new run that carries it: while that
  // run starts, the item does not need the user (backlog.Item.AnswerStarting).
  const last = it.runs?.[it.runs.length - 1];
  if (last?.state === "starting" && last.answers?.length) return undefined;
  const thread = it.thread ?? [];
  // An answer kept for a new run, or one a later answer replaced, leaves its
  // question open until a run starts with it (backlog.Message.closesQuestion).
  const closes = (m: Message) => m.kind === "answer" && !!m.answers && m.delivery !== "new_run" && m.delivery !== "superseded";
  const answered = new Set(thread.filter(closes).map((m) => m.answers));
  return thread.find((m) => m.kind === "question" && !m.closed && !answered.has(m.id));
}

/** Items with an open question, across every given backlog. Parents roll a
 *  child's question up, so counting questions (not states) counts each once. */
export function needsYouCount(views: Iterable<BacklogView>): number {
  let n = 0;
  for (const v of views) n += v.backlog.items.filter((it) => openQuestion(it)).length;
  return n;
}

/** Each project root with a non-zero needs-you count. */
export function needsYouByRoot(views: ReadonlyMap<string, BacklogView>): Map<string, number> {
  const out = new Map<string, number>();
  for (const [root, v] of views) {
    const n = needsYouCount([v]);
    if (n > 0) out.set(root, n);
  }
  return out;
}

export interface SessionItem {
  root: string;
  id: number;
  title: string;
  state: State;
}

/** The proposals the backlog's items came from, in first-seen order. */
export function sources(b: Backlog): string[] {
  return [...new Set(b.items.map((it) => it.source?.proposal ?? "").filter(Boolean))];
}

/** What a rail chip draws and navigates by: a change in any part redraws it. The project
 *  root is part of it, since a rebound project keeps its items' ids and titles. */
export function sessionItemsSig(items: ReadonlyMap<string, SessionItem>): string {
  return JSON.stringify([...items].map(([sid, it]) => [sid, it.id, it.title, it.root, it.state]));
}

/** Maps each item session id to its item, from the items' run refs. A session
 *  named by two items keeps the later run. */
export function sessionItems(views: ReadonlyMap<string, BacklogView>): Map<string, SessionItem> {
  const out = new Map<string, SessionItem>();
  for (const [root, v] of views) {
    for (const it of v.backlog.items) {
      for (const run of it.runs ?? []) {
        if (run.session_id) out.set(run.session_id, { root, id: it.id, title: it.title, state: derivedOf(v, it.id).state });
      }
    }
  }
  return out;
}

export interface ListRow {
  item: Item;
  depth: number;
  derived: Derived;
  parent: boolean;
  folded: boolean;
  /** The row's reason line: the derived reason, plus a needs-you count on parents. */
  reason: string;
}

export interface ListOptions {
  filter: Filter;
  showDone: boolean;
  folded: ReadonlySet<number>;
  /** Only items made from this proposal. */
  source?: string | null;
}

function closedState(s: State): boolean {
  return s === "done" || s === "dropped";
}

/** The rows the list shows, in tree order. A filter keeps a row that matches or
 *  has a descendant that matches, so a match keeps its context. Hidden done rows
 *  hide their subtree; a folded parent hides its descendants. */
export function listRows(view: BacklogView, opts: ListOptions): ListRow[] {
  const b = view.backlog;
  const kids = new Map<number, number[]>();
  for (const it of b.items) {
    const p = it.parent ?? 0;
    kids.set(p, [...(kids.get(p) ?? []), it.id]);
  }
  const wanted = opts.filter === "all" ? null : FILTER_STATES[opts.filter];
  const matchMemo = new Map<number, boolean>();
  const matches = (id: number): boolean => {
    const memo = matchMemo.get(id);
    if (memo !== undefined) return memo;
    matchMemo.set(id, false);
    const it = itemByID(b, id);
    const own = (wanted === null || wanted.includes(derivedOf(view, id).state)) &&
      (!opts.source || it?.source?.proposal === opts.source);
    const r = own || (kids.get(id) ?? []).some(matches);
    matchMemo.set(id, r);
    return r;
  };
  const needsBelow = (id: number): number =>
    (kids.get(id) ?? []).reduce((n, c) => {
      const it = itemByID(b, c);
      return n + (it && openQuestion(it) ? 1 : 0) + needsBelow(c);
    }, 0);
  const rows: ListRow[] = [];
  const walk = (parent: number, depth: number): void => {
    for (const id of kids.get(parent) ?? []) {
      const item = itemByID(b, id);
      if (!item) continue;
      const derived = derivedOf(view, id);
      if (!opts.showDone && closedState(derived.state)) continue;
      if (!matches(id)) continue;
      const hasKids = (kids.get(id) ?? []).length > 0;
      const folded = hasKids && opts.folded.has(id);
      let reason = derived.reason ?? "";
      if (hasKids) {
        const n = needsBelow(id);
        if (n > 0) reason = reason ? `${reason} · ${n} needs you` : `${n} needs you`;
      }
      rows.push({ item, depth, derived, parent: hasKids, folded, reason });
      if (hasKids && !folded) walk(id, depth + 1);
    }
  };
  walk(0, 0);
  return rows;
}

function siblings(b: Backlog, id: number): Item[] {
  const it = itemByID(b, id);
  return it ? childrenOf(b, it.parent ?? 0) : [];
}

function canBeParent(it: Item): boolean {
  return it.status !== "running" && (it.runs ?? []).length === 0;
}

/** The move Alt+arrow asks for, or null when there is nowhere to go. Up and down
 *  pass the neighbouring sibling; right nests under the previous sibling; left
 *  puts the item after its parent. */
export function keyboardMove(b: Backlog, id: number, dir: "up" | "down" | "in" | "out"): MoveTarget | null {
  const it = itemByID(b, id);
  if (!it) return null;
  const sibs = siblings(b, id);
  const i = sibs.findIndex((s) => s.id === id);
  switch (dir) {
    case "up":
      return i > 0 ? { before: sibs[i - 1].id } : null;
    case "down":
      return i >= 0 && i < sibs.length - 1 ? { after: sibs[i + 1].id } : null;
    case "in": {
      const prev = i > 0 ? sibs[i - 1] : undefined;
      return prev && canBeParent(prev) ? { into: prev.id } : null;
    }
    case "out":
      return it.parent ? { after: it.parent } : null;
  }
}

export type DropZone = "before" | "after" | "into";

/** The move a drop of `dragId` on `targetId` asks for, or null when the drop is
 *  onto the item itself or into its own subtree. */
export function dropMove(b: Backlog, dragId: number, targetId: number, zone: DropZone): MoveTarget | null {
  const target = itemByID(b, targetId);
  if (!target || !itemByID(b, dragId) || dragId === targetId || isAncestor(b, dragId, targetId)) return null;
  if (zone === "into") return canBeParent(target) ? { into: targetId } : null;
  return zone === "before" ? { before: targetId } : { after: targetId };
}

function subtreeEnd(items: Item[], i: number, b: Backlog): number {
  const root = items[i].id;
  let j = i + 1;
  while (j < items.length && isAncestor(b, root, items[j].id)) j++;
  return j;
}

/** Applies a move to a copy of the backlog, the same way backlog.Move does, so a
 *  reorder shows at once. Returns null when the move is not valid. */
export function applyMove(b: Backlog, id: number, to: MoveTarget): Backlog | null {
  const it = itemByID(b, id);
  if (!it) return null;
  let parent = it.parent ?? 0;
  let anchor = 0;
  if ("before" in to || "after" in to) {
    anchor = "before" in to ? to.before : to.after;
    const a = itemByID(b, anchor);
    if (!a) return null;
    parent = a.parent ?? 0;
  } else if ("into" in to) {
    anchor = to.into;
    parent = to.into;
  }
  if (anchor === id || (anchor !== 0 && isAncestor(b, id, anchor))) return null;
  if (parent !== 0) {
    const p = itemByID(b, parent);
    if (!p || !canBeParent(p)) return null;
  }
  const items = b.items.map((x) => ({ ...x }));
  const start = items.findIndex((x) => x.id === id);
  const end = subtreeEnd(items, start, b);
  const block = items.splice(start, end - start);
  block[0].parent = parent || undefined;
  const moved: Backlog = { ...b, items };
  const indexOf = (x: number): number => items.findIndex((y) => y.id === x);
  const bottomOf = (p: number): number => (p === 0 ? items.length : subtreeEnd(items, indexOf(p), moved));
  let at: number;
  if ("before" in to) at = indexOf(to.before);
  else if ("after" in to) at = subtreeEnd(items, indexOf(to.after), moved);
  else if ("top" in to) at = parent === 0 ? 0 : indexOf(parent) + 1;
  else at = bottomOf(parent);
  items.splice(at, 0, ...block);
  if (parent !== 0) {
    const p = items.find((x) => x.id === parent);
    if (p) p.status = "draft";
  }
  return moved;
}

/** Shows an optimistic move over a fresher list: the order and parents `moved`
 *  settled on (and the statuses the move itself changed from `base`), with every
 *  other field, and any item the move never saw, taken from `fresh`. */
export function overlayMove(fresh: Backlog, moved: Backlog, base: Backlog): Backlog {
  const now = new Map(fresh.items.map((x) => [x.id, x]));
  const was = new Map(base.items.map((x) => [x.id, x]));
  const seen = new Set<number>();
  const items: Item[] = [];
  for (const m of moved.items) {
    const f = now.get(m.id);
    if (!f) continue;
    seen.add(m.id);
    const b = was.get(m.id);
    items.push({ ...f, parent: m.parent, ...(b && b.status !== m.status ? { status: m.status } : {}) });
  }
  for (const f of fresh.items) if (!seen.has(f.id)) items.push(f);
  return { ...fresh, items };
}

/** Items that wait on id. */
export function neededBy(b: Backlog, id: number): Item[] {
  return b.items.filter((it) => (it.waits_on ?? []).includes(id));
}

/** Open items that wait on id: the row's "blocks n". Open by derived state, the
 *  one listRows hides closed rows by: a parent whose children all finished
 *  derives done while its stored status can still read running. */
export function blocking(view: BacklogView, id: number): Item[] {
  return neededBy(view.backlog, id).filter((it) => !closedState(derivedOf(view, it.id).state));
}

/** The items id waits on and the items that wait on it. */
export function relatedIDs(b: Backlog, id: number): Set<number> {
  const out = new Set<number>((itemByID(b, id)?.waits_on ?? []).filter((w) => itemByID(b, w)));
  for (const it of neededBy(b, id)) out.add(it.id);
  out.delete(id);
  return out;
}

/** The newest run: the one the state chip links to and the session card shows. */
export function latestRun(it: Item): RunRef | undefined {
  return it.runs?.at(-1);
}

export type ItemAction = "queue" | "draft" | "start" | "review" | "done" | "drop" | "reopen" | "delete";

export const ACTION_LABELS: Record<ItemAction, string> = {
  queue: "Queue",
  draft: "Back to draft",
  start: "Start now",
  review: "Send to review",
  done: "Mark done",
  drop: "Drop",
  reopen: "Reopen",
  delete: "Delete",
};

/** Whether "Start now" is offered. It follows the daemon's start rule
 *  (backlog/runs.go BeginStart) so the button is never offered where the daemon
 *  refuses: not a parent (parents run through their children), not a closed item
 *  (done or dropped: reopen first), and not one already Starting or Working (an
 *  active run is "already working"). The daemon would accept a "continue" to an item
 *  that is waiting on an answer, but the answer is the next step there, so the
 *  button is not offered while the item Needs you. What only the daemon can see (a
 *  usage-limited session) it still refuses, with its own message. */
export function canStartNow(b: Backlog, it: Item, derived: Derived): boolean {
  if (isParent(b, it.id)) return false;
  if (it.status === "done" || it.status === "dropped") return false;
  return derived.state !== "working" && derived.state !== "starting" && derived.state !== "needs_you";
}

/** The actions the panel offers for an item, in the order it shows them. A
 *  parent only queues its draft children and drops its open ones. Send to
 *  review answers af's own question, such as the cap per run. */
export function itemActions(b: Backlog, it: Item, derived: Derived): ItemAction[] {
  if (isParent(b, it.id)) {
    const out: ItemAction[] = [];
    if (b.items.some((c) => isAncestor(b, it.id, c.id) && c.status === "draft" && !isParent(b, c.id))) out.push("queue");
    if (!closedState(derived.state)) out.push("drop");
    return out;
  }
  const out: ItemAction[] = [];
  const s = it.status;
  if (s === "draft" || s === "review") out.push("queue");
  if (s === "queued" || s === "review") out.push("draft");
  if (canStartNow(b, it, derived)) out.push("start");
  if (s === "running" && openQuestion(it)?.author === "af") out.push("review");
  if (s !== "done" && s !== "dropped") out.push("done", "drop");
  else out.push("reopen");
  if (s === "draft" && (it.runs ?? []).length === 0 && (it.thread ?? []).length === 0) out.push("delete");
  return out;
}

/** href for an agent-supplied link: only http and https URLs, else null. */
export function safeHttpUrl(raw: string | undefined): string | null {
  if (!raw) return null;
  try {
    const u = new URL(raw);
    return (u.protocol === "http:" || u.protocol === "https:") && u.host ? u.href : null;
  } catch {
    return null;
  }
}

/** The item a Run next pass starts first: the top Queued leaf in tree order. */
export function topQueued(view: BacklogView): Item | undefined {
  return view.backlog.items.find((it) => derivedOf(view, it.id).state === "queued" && !isParent(view.backlog, it.id));
}

export function needsYouLabel(n: number): string {
  return n === 1 ? "1 needs you" : `${n} need you`;
}
