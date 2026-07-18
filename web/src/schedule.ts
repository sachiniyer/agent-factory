// The web half of the canonical schedule model behind the friendly task-schedule
// picker (#2057, phase 2). This is a DELIBERATE MIRROR of the Go package
// `schedule` (schedule/schedule.go) — the same preset types, the same cron
// generation, the same best-effort ParseCron, and the same plain-English
// describe() text, so the TUI form and this browser modal cannot disagree about
// what a picker state means.
//
// Go is the canonical source: this file follows it, never the other way round.
// What keeps the two honest is not review discipline but a test — schedule.test.ts
// loads the SAME shared vector file the Go test loads
// (schedule/testdata/vectors.json, read across the repo root, no copy) and asserts
// every cron/human/parse triple matches. Diverge from Go and that test fails.
// This is the #1970 shared-source-of-truth pattern, and the same arrangement
// frame.ts/frame.test.ts already use for the wire codec.
//
// Cron stays the stored/wire format: this model only shapes the INPUT. The daemon's
// own validation and scheduling are untouched, and Custom carries a raw expression
// verbatim as the advanced escape hatch.

/** The preset schedule shape a Schedule takes — the string values are the Go
 *  `schedule.Type` constants verbatim (they appear in the shared vectors). */
export type ScheduleType =
  | "everyNMinutes"
  | "everyNHours"
  | "hourly"
  | "daily"
  | "weekly"
  | "monthly"
  | "custom";

/** The schedule types in picker order, with their selector labels. Custom sits
 *  last as the advanced escape hatch — the same order as the TUI's
 *  `scheduleTypes` (ui/schedule_picker.go). */
export const SCHEDULE_TYPE_OPTIONS: ReadonlyArray<{ type: ScheduleType; label: string }> = [
  { type: "everyNMinutes", label: "Every N minutes" },
  { type: "everyNHours", label: "Every N hours" },
  { type: "hourly", label: "Hourly" },
  { type: "daily", label: "Daily" },
  { type: "weekly", label: "Weekly" },
  { type: "monthly", label: "Monthly" },
  { type: "custom", label: "Custom (cron)" },
];

/**
 * A preset schedule plus its contextual fields — the TS shape of Go's
 * `schedule.Schedule`. Only the fields relevant to `type` are meaningful; the
 * rest are absent, which stands in for Go's zero values (the shared vectors omit
 * them the same way, via `omitempty`).
 *
 * `hour` is 24-hour (0-23) — the 12-hour AM/PM presentation lives in describe()
 * and the picker UI, not in the model. `weekdays` holds Go `time.Weekday`
 * numbering: Sunday=0 … Saturday=6.
 */
export interface Schedule {
  type: ScheduleType;
  interval?: number;
  hour?: number;
  minute?: number;
  weekdays?: number[];
  dayOfMonth?: number;
  raw?: string;
}

/** parseCron's result: the structured schedule plus whether the expression was
 *  recognized as one of the shapes cron() emits (Go's `(Schedule, bool)`). */
export interface ParseResult {
  schedule: Schedule;
  ok: boolean;
}

/** Three-letter weekday abbreviations in cron/`time.Weekday` order, matching Go's
 *  `time.Weekday(n).String()[:3]`. */
const WEEKDAY_ABBREV = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"] as const;

/**
 * Renders the schedule as a 5-field cron expression (minute hour day-of-month
 * month day-of-week). Custom returns `raw` verbatim. The generated expressions are
 * exactly the shapes parseCron recognizes, so parseCron(cron(s)) round-trips every
 * preset back to s. Mirrors Go's `(Schedule).Cron`.
 */
export function cron(s: Schedule): string {
  switch (s.type) {
    case "everyNMinutes":
      return `*/${s.interval ?? 0} * * * *`;
    case "everyNHours":
      return `0 */${s.interval ?? 0} * * *`;
    case "hourly":
      return `${s.minute ?? 0} * * * *`;
    case "daily":
      return `${s.minute ?? 0} ${s.hour ?? 0} * * *`;
    case "weekly":
      return `${s.minute ?? 0} ${s.hour ?? 0} * * ${weekdayField(s.weekdays)}`;
    case "monthly":
      return `${s.minute ?? 0} ${s.hour ?? 0} ${s.dayOfMonth ?? 0} * *`;
    default: // custom and any unknown type
      return s.raw ?? "";
  }
}

/**
 * Renders the schedule as the plain-English preview line, e.g. "Every 15 minutes",
 * "Every day at 3:41 PM", "Every week on Mon, Wed at 9:00 AM". Custom echoes its
 * raw cron ("Custom: 41 3 * * *"). Times use a 12-hour clock with AM/PM. Mirrors
 * Go's `(Schedule).Describe` — byte-for-byte, per the shared vectors.
 */
export function describe(s: Schedule): string {
  switch (s.type) {
    case "everyNMinutes":
      return (s.interval ?? 0) === 1 ? "Every minute" : `Every ${s.interval ?? 0} minutes`;
    case "everyNHours":
      return (s.interval ?? 0) === 1 ? "Every hour" : `Every ${s.interval ?? 0} hours`;
    case "hourly":
      return `Every hour at :${pad2(s.minute ?? 0)}`;
    case "daily":
      return `Every day at ${clockTime(s.hour ?? 0, s.minute ?? 0)}`;
    case "weekly":
      return `Every week on ${weekdayNames(s.weekdays)} at ${clockTime(s.hour ?? 0, s.minute ?? 0)}`;
    case "monthly":
      return `Every month on the ${ordinal(s.dayOfMonth ?? 0)} at ${clockTime(s.hour ?? 0, s.minute ?? 0)}`;
    default: // custom and any unknown type
      return `Custom: ${s.raw ?? ""}`;
  }
}

/**
 * Best-effort maps a 5-field cron expression back to a structured Schedule. It
 * recognizes exactly the shapes cron() emits — so a task saved by the picker
 * re-opens as its matching preset — plus the Sunday day-of-week alias (7).
 * Anything else (ranges, multi-value minute/hour fields, month restrictions, step
 * forms we don't emit, or a malformed expression) returns
 * `{type: "custom", raw: expr}` with ok=false, signalling the UI to fall back to
 * the raw-cron editor. It deliberately does NOT parse arbitrary cron.
 *
 * Mirrors Go's `ParseCron`, including the out-of-range step rule (see stepOfStar):
 * a minute step of 60 or an hour step of 24 is REJECTED rather than clamped, so an
 * untouched re-save never silently rewrites the user's expression.
 */
export function parseCron(expr: string): ParseResult {
  const f = fields(expr);
  if (f.length !== 5) {
    return { schedule: custom(expr), ok: false };
  }
  const [minute, hour, dom, month, dow] = f;

  // No preset restricts the month field.
  if (month !== "*") {
    return { schedule: custom(expr), ok: false };
  }

  // every N minutes: */N * * * * (N in 1-59; see stepOfStar)
  const everyMin = stepOfStar(minute, 59);
  if (everyMin !== null && hour === "*" && dom === "*" && dow === "*") {
    return { schedule: { type: "everyNMinutes", interval: everyMin }, ok: true };
  }
  // every N hours: 0 */N * * * (N in 1-23)
  const everyHour = stepOfStar(hour, 23);
  if (everyHour !== null && minute === "0" && dom === "*" && dow === "*") {
    return { schedule: { type: "everyNHours", interval: everyHour }, ok: true };
  }
  // hourly: M * * * *
  const hourlyMinute = singleInt(minute, 0, 59);
  if (hourlyMinute !== null && hour === "*" && dom === "*" && dow === "*") {
    return { schedule: { type: "hourly", minute: hourlyMinute }, ok: true };
  }

  // The remaining presets all pin a single minute and hour.
  const m = singleInt(minute, 0, 59);
  const h = singleInt(hour, 0, 23);
  if (m !== null && h !== null) {
    if (dom === "*" && dow === "*") {
      // daily: M H * * *
      return { schedule: { type: "daily", hour: h, minute: m }, ok: true };
    }
    if (dom === "*") {
      // weekly: M H * * <days>
      const days = weekdayList(dow);
      if (days !== null) {
        return { schedule: { type: "weekly", hour: h, minute: m, weekdays: days }, ok: true };
      }
    } else if (dow === "*") {
      // monthly: M H DOM * *
      const d = singleInt(dom, 1, 31);
      if (d !== null) {
        return { schedule: { type: "monthly", hour: h, minute: m, dayOfMonth: d }, ok: true };
      }
    }
  }
  return { schedule: custom(expr), ok: false };
}

function custom(expr: string): Schedule {
  return { type: "custom", raw: expr };
}

/** Splits on runs of whitespace, dropping empties — Go's `strings.Fields`. */
function fields(expr: string): string[] {
  return expr.split(/\s+/).filter((part) => part !== "");
}

/**
 * Renders the day-of-week cron field: a sorted, de-duplicated, comma-separated
 * list of weekday numbers (0-6). Empty weekdays render as "*" (every day) so the
 * expression stays valid even mid-edit; the UI requires at least one day before it
 * will save a weekly task.
 */
function weekdayField(days: number[] | undefined): string {
  const nums = normalizeWeekdays(days);
  return nums.length === 0 ? "*" : nums.join(",");
}

/** Renders weekdays as sorted three-letter abbreviations joined by ", ", e.g.
 *  [1, 3] → "Mon, Wed". Ordered Sunday-first to match the cron day-of-week
 *  numbering. */
function weekdayNames(days: number[] | undefined): string {
  return normalizeWeekdays(days)
    .map((n) => WEEKDAY_ABBREV[n])
    .join(", ");
}

/** Maps each weekday into 0-6 (7 is the Sunday alias), de-duplicates, and sorts
 *  ascending, so both the cron field and the description present days in a stable
 *  Sunday-first order. Mirrors Go's normalizeWeekdays (which cannot receive a
 *  fractional weekday; truncating here keeps a stray one out of the cron field). */
function normalizeWeekdays(days: number[] | undefined): number[] {
  const seen = new Set<number>();
  for (const d of days ?? []) {
    const n = ((Math.trunc(d) % 7) + 7) % 7;
    seen.add(n);
  }
  return [...seen].sort((a, b) => a - b);
}

/** Formats a 24-hour hour/minute as a 12-hour clock time with AM/PM, e.g.
 *  (0,0)→"12:00 AM", (12,0)→"12:00 PM", (15,41)→"3:41 PM". */
function clockTime(hour: number, minute: number): string {
  let suffix = "AM";
  let h = hour;
  if (h === 0) {
    h = 12;
  } else if (h === 12) {
    suffix = "PM";
  } else if (h > 12) {
    h -= 12;
    suffix = "PM";
  }
  return `${h}:${pad2(minute)} ${suffix}`;
}

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

/** Renders n as an English ordinal: 1→"1st", 2→"2nd", 3→"3rd", 11→"11th",
 *  21→"21st", 31→"31st". */
function ordinal(n: number): string {
  let suffix = "th";
  if (n % 100 < 11 || n % 100 > 13) {
    switch (n % 10) {
      case 1:
        suffix = "st";
        break;
      case 2:
        suffix = "nd";
        break;
      case 3:
        suffix = "rd";
        break;
    }
  }
  return `${n}${suffix}`;
}

// stepOfStar parses a "*/N" field and returns N when it is a friendly interval,
// 1 <= N <= max, else null. A step at or beyond the field size (e.g. "*/60" for
// minutes or "*/24" for hours) is rejected so parseCron falls back to custom and
// preserves the raw expression, rather than the picker clamping it (to */59 /
// */23) and silently rewriting the cron on an otherwise-untouched re-save
// (#2057). Any other shape (a bare "*", a single int, a range step, or a list)
// also returns null.
function stepOfStar(field: string, max: number): number | null {
  if (!field.startsWith("*/")) {
    return null;
  }
  const n = atoi(field.slice(2));
  if (n === null || n < 1 || n > max) {
    return null;
  }
  return n;
}

/** Parses a field that is a single integer within [min,max], else null. A field
 *  containing any cron metacharacter (* / , -) fails, so only a bare number
 *  matches. */
function singleInt(field: string, min: number, max: number): number | null {
  const n = atoi(field);
  if (n === null || n < min || n > max) {
    return null;
  }
  return n;
}

/**
 * Parses a day-of-week field that is a comma-separated list of single weekday
 * numbers (0-6, or 7 as a Sunday alias) into sorted, de-duplicated weekday values
 * — the shape cron() emits for a weekly schedule. A wildcard, range, or step form
 * returns null so the caller falls back to custom.
 */
function weekdayList(field: string): number[] | null {
  if (field === "*") {
    return null;
  }
  const seen = new Set<number>();
  for (const part of field.split(",")) {
    const n = atoi(part);
    if (n === null || n < 0 || n > 7) {
      return null;
    }
    seen.add(n === 7 ? 0 : n); // Sunday alias
  }
  if (seen.size === 0) {
    return null;
  }
  return [...seen].sort((a, b) => a - b);
}

/**
 * Go's `strconv.Atoi` semantics, which the parse rules above depend on: an
 * optional sign followed by ASCII digits and nothing else — no surrounding
 * whitespace, no partial prefix ("5x"), no empty string, no float. TS's Number()
 * and parseInt() are both looser in ways that would silently accept expressions Go
 * rejects (and so drift from the shared vectors), hence the explicit shape check.
 */
function atoi(s: string): number | null {
  if (!/^[+-]?\d+$/.test(s)) {
    return null;
  }
  const n = Number(s);
  if (!Number.isSafeInteger(n)) {
    return null; // Go returns ErrRange for an out-of-int64 value
  }
  return n === 0 ? 0 : n; // normalize "-0" so equality with 0 holds
}
