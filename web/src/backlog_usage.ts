// Plan usage for the Backlog view (backlog proposal, technical.md "Budget"):
// the GetBudget windows, backlog.spend payloads applied on top, the runner
// bar's window and hover text, run use, and the budget gates a start by hand
// asks about. No DOM and no I/O.

import type { Budget, RunnerView } from "./backlog_model.js";

export interface PlanWindow {
  plan: number;
  backlog: number;
  /** null when there is no valid reading for the window. */
  resets_at: string | null;
  /** The metered cost of every transcript in the window, in US dollars. */
  all_cost?: number;
}

export interface Usage {
  metering: boolean;
  /** The af account the top-level windows are for; "default" is the ambient
   *  Claude identity. */
  account: string;
  /** Each metered account's 5-hour window, from GetBudget. */
  accounts: Record<string, PlanWindow>;
  five_hour: PlanWindow;
  seven_day: PlanWindow;
  reading_at: string | null;
  /** Each item session's metered cost in US dollars at API prices. */
  runs: Record<string, number>;
  /** Each item session's metered cost in the current 5-hour window. */
  run_window: Record<string, number>;
  /** Why af cannot meter an item session, by session id. */
  unmeterable: Record<string, string>;
}

export type WindowKey = "five_hour" | "seven_day";

export const WINDOW_LABELS: Record<WindowKey, { bar: string; gate: string; hover: string }> = {
  five_hour: { bar: "5-hour window", gate: "the 5-hour window", hover: "5-hour" },
  seven_day: { bar: "week", gate: "the week", hover: "Weekly" },
};

const WINDOWS: readonly WindowKey[] = ["five_hour", "seven_day"];

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function numbers(v: unknown): Record<string, number> {
  const out: Record<string, number> = {};
  if (isRecord(v)) for (const [k, x] of Object.entries(v)) if (typeof x === "number") out[k] = x;
  return out;
}

function strings(v: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (isRecord(v)) for (const [k, x] of Object.entries(v)) if (typeof x === "string") out[k] = x;
  return out;
}

function parseWindow(v: unknown): PlanWindow {
  const w = isRecord(v) ? v : {};
  return {
    plan: Number(w.plan) || 0,
    backlog: Number(w.backlog) || 0,
    resets_at: typeof w.resets_at === "string" ? w.resets_at : null,
    all_cost: Number(w.all_cost) || 0,
  };
}

/** The usage half of a GetBudget or SetBudget reply. */
export function parseUsage(d: unknown): Usage {
  const v = isRecord(d) ? d : {};
  const w = isRecord(v.windows) ? v.windows : {};
  const accounts: Record<string, PlanWindow> = {};
  if (isRecord(v.accounts)) {
    for (const [name, a] of Object.entries(v.accounts)) {
      if (isRecord(a) && isRecord(a.windows)) accounts[name] = parseWindow(a.windows.five_hour);
    }
  }
  return {
    metering: v.metering === true,
    account: typeof v.account === "string" ? v.account : "",
    accounts,
    five_hour: parseWindow(w.five_hour),
    seven_day: parseWindow(w.seven_day),
    reading_at: typeof v.reading_at === "string" ? v.reading_at : null,
    runs: numbers(v.runs),
    run_window: numbers(v.run_window),
    unmeterable: strings(v.unmeterable),
  };
}

/** Whether a backlog.spend payload carries the unmeterable runs; a daemon
 *  from before they were added leaves them out. */
export function spendHasUnmeterable(ev: unknown): boolean {
  return isRecord(ev) && "unmeterable" in ev;
}

/** Applies a backlog.spend payload. Fields the payload leaves out keep what
 *  GetBudget last said. */
export function applySpend(prev: Usage | null, ev: unknown): Usage | null {
  if (!isRecord(ev)) return prev;
  return {
    metering: false,
    account: prev?.account ?? "",
    accounts: prev?.accounts ?? {},
    five_hour: parseWindow(ev.five_hour),
    seven_day: parseWindow(ev.seven_day),
    reading_at: typeof ev.reading_at === "string" ? ev.reading_at : null,
    runs: numbers(ev.runs),
    run_window: "run_window" in ev ? numbers(ev.run_window) : prev?.run_window ?? {},
    unmeterable: spendHasUnmeterable(ev) ? strings(ev.unmeterable) : prev?.unmeterable ?? {},
  };
}

export function valid(w: PlanWindow, now: number): boolean {
  return w.resets_at !== null && Date.parse(w.resets_at) > now;
}

/** A percent the way every surface shows it: 50%, 12.5%. */
export function pct(v: number): string {
  return `${Number(v.toFixed(1))}%`;
}

export function usd(v: number): string {
  return `$${v.toFixed(2)}`;
}

/** The window closest to stopping the runner: the larger of backlog / share
 *  and plan / stop at; a tie goes to the 5-hour window. null when neither
 *  window has a valid reading. */
export function tighterWindow(u: Usage, b: Budget, now: number): WindowKey | null {
  const ratio = (v: number, limit: number): number => (limit > 0 ? v / limit : Infinity);
  let best: WindowKey | null = null;
  let score = -Infinity;
  for (const k of WINDOWS) {
    const w = u[k];
    if (!valid(w, now)) continue;
    const s = Math.max(ratio(w.backlog, b.share), ratio(w.plan, b.stop_at));
    if (s > score) {
      best = k;
      score = s;
    }
  }
  return best;
}

/** A reset time: 14:00 within a day, else Mon 09:00, in local time. */
export function resetLabel(at: string, now: number): string {
  const d = new Date(at);
  const hm = `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  if (d.getTime() - now < 24 * 3_600_000) return hm;
  return `${d.toLocaleDateString("en-US", { weekday: "short" })} ${hm}`;
}

export function readingAge(at: string | null, now: number): string {
  if (!at) return "no reading yet";
  const min = Math.floor(Math.max(0, now - Date.parse(at)) / 60_000);
  if (min < 1) return "read just now";
  if (min < 60) return `read ${min} min ago`;
  const hours = Math.floor(min / 60);
  return hours < 24 ? `read ${hours} h ago` : `read ${Math.floor(hours / 24)} d ago`;
}

/** The runner bar's detail: both windows, the reading's age, and the scope. */
export function usageLines(u: Usage, now: number): string[] {
  const line = (k: WindowKey): string => {
    const w = u[k];
    const label = WINDOW_LABELS[k].hover;
    if (!valid(w, now)) return `${label} · no reading`;
    return `${label} · plan ${pct(w.plan)} · backlog ${pct(w.backlog)} · resets ${resetLabel(w.resets_at!, now)}`;
  };
  return [line("five_hour"), line("seven_day"), `${readingAge(u.reading_at, now)} · all projects`];
}

export function usageHover(u: Usage, now: number): string {
  return usageLines(u, now).join("\n");
}

/** A run's use as a percent of its account's 5-hour window: its cost in the
 *  window over all metered cost in it, times the plan percent. null without a
 *  reading, a window cost for the run, or any metered cost. account is the
 *  session's af account; empty is the ambient identity, "default". */
export function runUse(sessionId: string, u: Usage, now: number, account = ""): number | null {
  const name = account || "default";
  const w = name !== u.account && u.accounts[name] ? u.accounts[name] : u.five_hour;
  const cost = u.run_window[sessionId];
  if (cost === undefined || !valid(w, now) || !w.all_cost || w.all_cost <= 0) return null;
  return Math.min(100, (w.plan * cost) / w.all_cost);
}

export interface Gate {
  /** The gate and window, stable while the numbers move: "stop_at:five_hour". */
  key: string;
  text: string;
}

/** The first closed budget gate, or null when all are open. A start by hand
 *  passes the gates, so the web asks once per gate before it does. */
export function closedGate(b: Budget | null, u: Usage | null, runner: RunnerView | null, now: number): Gate | null {
  if (b && b.share <= 0) return { key: "share:zero", text: "The backlog share is 0%." };
  if (b && u) {
    for (const k of WINDOWS) {
      const w = u[k];
      if (valid(w, now) && w.backlog >= b.share) {
        return { key: `share:${k}`, text: `The backlog used its ${pct(b.share)} of ${WINDOW_LABELS[k].gate}.` };
      }
    }
    for (const k of WINDOWS) {
      const w = u[k];
      if (valid(w, now) && w.plan >= b.stop_at) {
        return { key: `stop_at:${k}`, text: `The plan is at ${pct(w.plan)} of ${WINDOW_LABELS[k].gate}.` };
      }
    }
  }
  // The budget's own totals, not the runner's gate: the daemon's runner view carries
  // no gate field today, so an exhausted dollar limit must be read from the budget.
  if (b && b.usd_limit > 0 && b.usd_spent >= b.usd_limit) {
    return { key: "usd_limit", text: `The backlog spent its ${usd(b.usd_limit)} dollar limit.` };
  }
  if (runner?.state === "stopped" && (runner.gate === "usage_limit" || runner.gate === "usd_limit") && runner.reason) {
    return { key: runner.gate, text: `${webReason(runner.reason).replace(/\.$/, "")}.` };
  }
  return null;
}

/** A daemon reason without its CLI hints: the web offers its own actions, so
 *  a clause naming an af command is dropped. */
export function webReason(reason: string): string {
  const out = reason
    .replace(/,?\s+or\s+[^;,·]*"af [^"]*"[^;,·]*/g, "")
    .replace(/(?:[;,]\s*|\s·\s)?[^;,·]*"af [^"]*"[^;,·]*/g, "")
    .trim();
  return out || reason;
}
