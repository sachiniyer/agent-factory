// The TypeScript half of the cross-language schedule contract (#2057 phase 2).
//
// It validates web/src/schedule.ts against the SAME fixture the Go test uses —
// schedule/testdata/vectors.json, read straight across the repo root, NOT a copy —
// so the browser picker and the TUI picker are pinned to identical cron generation,
// identical plain-English previews, and identical parse rules, and cannot silently
// diverge. The Go half is schedule/schedule_test.go: TestVectors — same file, same
// assertions, same per-vector rules (documented in the fixture's own `_readme`).
// This is the arrangement frame.test.ts already uses for the wire codec, and the
// #1970 shared-source-of-truth pattern the issue calls for.
//
// If you change one implementation's behavior, this test is what fails until the
// vectors and the other implementation agree. Go is canonical: fix the TS, or
// change the vectors and BOTH implementations together.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

import { SCHEDULE_TYPE_OPTIONS, type Schedule, type ScheduleType, cron, describe, parseCron } from "./schedule.js";

const here = dirname(fileURLToPath(import.meta.url));
// web/src → repo-root/schedule/testdata/vectors.json (single source of truth,
// shared verbatim with schedule/schedule_test.go).
const FIXTURE_PATH = join(here, "..", "..", "schedule", "testdata", "vectors.json");

/** Overrides the default parseCron expectation for a vector; any absent field falls
 *  back to the vector's own cron/schedule (see the fixture's schema note). */
interface ParseExpect {
  cron?: string;
  ok?: boolean;
  schedule?: Schedule;
}

interface Vector {
  name: string;
  schedule?: Schedule;
  cron?: string;
  human?: string;
  parse?: ParseExpect;
}

function loadVectors(): Vector[] {
  // A missing/unreadable fixture must FAIL, never skip: the whole point of this
  // test is that it breaks when the TS and Go models drift, so it may not quietly
  // pass with nothing loaded.
  const raw = readFileSync(FIXTURE_PATH, "utf8");
  const parsed = JSON.parse(raw) as { vectors?: Vector[] };
  const vectors = parsed.vectors ?? [];
  assert.ok(vectors.length > 0, `no vectors loaded from ${FIXTURE_PATH}`);
  return vectors;
}

/** Fills in the zero values Go's struct has but the JSON omits (`omitempty`), so a
 *  parsed schedule and an expected one compare exactly the way Go's
 *  reflect.DeepEqual compares them. parseCron never returns an empty-but-present
 *  weekday list, so collapsing absent → [] loses nothing the Go test enforces. */
function canonical(s: Schedule): Required<Schedule> {
  return {
    type: s.type,
    interval: s.interval ?? 0,
    hour: s.hour ?? 0,
    minute: s.minute ?? 0,
    weekdays: s.weekdays ?? [],
    dayOfMonth: s.dayOfMonth ?? 0,
    raw: s.raw ?? "",
  };
}

const VECTORS = loadVectors();

for (const v of VECTORS) {
  test(`vectors: ${v.name}`, () => {
    if (v.schedule !== undefined && v.cron !== undefined) {
      assert.equal(cron(v.schedule), v.cron, `cron() for ${v.name}`);
    }
    if (v.schedule !== undefined && v.human !== undefined) {
      assert.equal(describe(v.schedule), v.human, `describe() for ${v.name}`);
    }

    // parseCron: input and expectations default to the vector, with the optional
    // parse block overriding — the same resolution the Go test does.
    const input = v.parse?.cron ?? v.cron;
    if (input === undefined) {
      return;
    }
    const expectOk = v.parse?.ok ?? true;
    const expected = v.parse?.schedule ?? v.schedule;

    const got = parseCron(input);
    assert.equal(got.ok, expectOk, `parseCron(${JSON.stringify(input)}) ok`);
    if (expected !== undefined) {
      assert.deepStrictEqual(
        canonical(got.schedule),
        canonical(expected),
        `parseCron(${JSON.stringify(input)}) schedule`,
      );
    }
  });
}

// A vector file that lost its coverage would let the parity test pass vacuously for
// a whole schedule type. Pin that every type the picker offers is actually exercised
// by at least one cron-generating vector.
test("the shared vectors cover every schedule type", () => {
  const covered = new Set<ScheduleType>();
  for (const v of VECTORS) {
    if (v.schedule !== undefined && v.cron !== undefined) {
      covered.add(v.schedule.type);
    }
  }
  for (const opt of SCHEDULE_TYPE_OPTIONS) {
    assert.ok(covered.has(opt.type), `no cron vector covers the "${opt.type}" schedule type`);
  }
});

// The structural round-trip the picker depends on: an existing task re-opens as its
// matching preset. Mirrors Go's TestParseCronRoundTrip.
test("parseCron(cron(s)) round-trips every preset", () => {
  const presets: Schedule[] = [
    { type: "everyNMinutes", interval: 1 },
    { type: "everyNMinutes", interval: 45 },
    { type: "everyNMinutes", interval: 59 }, // upper bound still a preset
    { type: "everyNHours", interval: 1 },
    { type: "everyNHours", interval: 8 },
    { type: "everyNHours", interval: 23 }, // upper bound still a preset
    { type: "hourly", minute: 0 },
    { type: "hourly", minute: 17 },
    { type: "daily", hour: 0, minute: 0 },
    { type: "daily", hour: 23, minute: 59 },
    { type: "weekly", hour: 9, minute: 30, weekdays: [1] },
    { type: "weekly", hour: 7, minute: 0, weekdays: [0, 6] },
    { type: "monthly", hour: 12, minute: 0, dayOfMonth: 1 },
    { type: "monthly", hour: 6, minute: 15, dayOfMonth: 28 },
  ];
  for (const s of presets) {
    const expr = cron(s);
    const got = parseCron(expr);
    assert.ok(got.ok, `parseCron(${expr}) must recognize a preset`);
    assert.deepStrictEqual(canonical(got.schedule), canonical(s), `round-trip of ${expr}`);
  }
});

// The best-effort contract: anything that is not one of the emitted shapes returns
// {custom, raw} with ok=false so the UI drops into the raw-cron editor. Mirrors Go's
// TestParseCronUnrecognizedFallsBackToCustom.
test("an unrecognized expression falls back to custom, preserving the raw text", () => {
  for (const expr of [
    "0 9 * * 1-5", // weekday range (we emit comma lists)
    "*/7 9-17 * * *", // hour range
    "15 */3 * * *", // minute + hour-step combination we never emit
    "0 0 1,15 * *", // day-of-month list
    "0 0 * JAN *", // named month
    "@daily", // descriptor
    "0 0", // too few fields
    "*/60 * * * *", // minute step at/over field size — clamps if parsed, so custom
    "0 */24 * * *", // hour step at/over field size — custom, not clamped to */23
  ]) {
    const got = parseCron(expr);
    assert.equal(got.ok, false, `parseCron(${expr}) ok must be false`);
    assert.deepStrictEqual(canonical(got.schedule), canonical({ type: "custom", raw: expr }));
  }
});

// TS's Number()/parseInt() accept shapes Go's strconv.Atoi rejects ("5x", " 5",
// "0x10", ""). Left unguarded, those would parse into presets the Go model calls
// custom — a drift the vectors alone don't cover, since they can't enumerate every
// malformed field.
test("loose numeric fields Go rejects stay custom, not a silently-accepted preset", () => {
  for (const expr of [
    "5x * * * *", // parseInt would take the "5"
    "0x10 * * * *", // Number() would take 16
    "5. * * * *", // Number() would take 5
    "1e2 * * * *", // Number() would take 100
    "*/5x * * * *", // the step form, same trap
    "0 9 * * 1,2x", // a bad member of a weekday list
  ]) {
    const got = parseCron(expr);
    assert.equal(got.ok, false, `parseCron(${expr}) ok must be false`);
    assert.equal(got.schedule.type, "custom");
    assert.equal(got.schedule.raw, expr);
  }
});

// Normalization the cron field and the description both depend on. Mirrors Go's
// TestWeekdayOrderIsSundayFirstAndDeduped.
test("weekdays are deduped and ordered Sunday-first regardless of input order", () => {
  const s: Schedule = { type: "weekly", hour: 9, minute: 0, weekdays: [3, 0, 1, 3] };
  assert.equal(cron(s), "0 9 * * 0,1,3");
  assert.equal(describe(s), "Every week on Sun, Mon, Wed at 9:00 AM");
});
