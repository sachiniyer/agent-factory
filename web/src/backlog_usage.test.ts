import { test } from "node:test";
import assert from "node:assert/strict";

import type { Budget, RunnerView } from "./backlog_model.js";
import {
  type Usage, applySpend, closedGate, parseUsage, webReason, pct, readingAge, resetLabel, runUse, tighterWindow, usageHover,
} from "./backlog_usage.js";

const NOW = Date.parse("2000-01-03T10:00:00");
const at = (s: string): string => new Date(s).toISOString();
const SOON = at("2000-01-03T14:00:00");
const MONDAY = at("2000-01-10T09:00:00");

const budget = (over: Partial<Budget> = {}): Budget => ({ share: 50, stop_at: 90, max_per_run: 10, usd_limit: 0, usd_spent: 0, ...over });

function usage(over: Partial<Usage> = {}): Usage {
  return {
    metering: false,
    account: "default",
    accounts: {},
    five_hour: { plan: 41, backlog: 12, resets_at: SOON, all_cost: 20 },
    seven_day: { plan: 40, backlog: 18, resets_at: MONDAY, all_cost: 200 },
    reading_at: at("2000-01-03T09:58:00"),
    runs: {},
    run_window: {},
    unmeterable: {},
    ...over,
  };
}

test("parseUsage reads the GetBudget reply and fills what Go leaves out", () => {
  const u = parseUsage({
    budget: {}, metering: true, reading_at: null,
    windows: { five_hour: { plan: 3, backlog: 1, resets_at: SOON, all_cost: 2 }, seven_day: { plan: 0, backlog: 0, resets_at: null } },
    runs: { s1: 1.5, bad: "x" }, unmeterable: { s2: "the runner can't meter aider" },
  });
  assert.equal(u.metering, true);
  assert.deepEqual(u.five_hour, { plan: 3, backlog: 1, resets_at: SOON, all_cost: 2 });
  assert.equal(u.seven_day.resets_at, null);
  assert.deepEqual(u.runs, { s1: 1.5 });
  assert.deepEqual(u.unmeterable, { s2: "the runner can't meter aider" });
  assert.deepEqual(parseUsage(null).runs, {});
});

// The exact JSON of #39's backlog/budget_json_test.go TestBudgetResponseJSONShape
// and TestSpendEventJSONShape.
const GO_BUDGET_RESPONSE = '{"budget":{"schema_version":1,"share":50,"stop_at":90,"max_per_run":10,"usd_limit":0,"usd_spent":0},' +
  '"account":"default",' +
  '"windows":{"five_hour":{"plan":40,"backlog":10,"all_cost":8,"resets_at":"2026-10-20T14:00:00Z"},"seven_day":{"plan":0,"backlog":0,"all_cost":0,"resets_at":null}},' +
  '"reading_at":"2026-10-20T12:00:00Z",' +
  '"accounts":{"default":{"windows":{"five_hour":{"plan":40,"backlog":10,"all_cost":8,"resets_at":"2026-10-20T14:00:00Z"},"seven_day":{"plan":0,"backlog":0,"all_cost":0,"resets_at":null}},"reading_at":"2026-10-20T12:00:00Z","unpriced":["claude-x-9"]}},' +
  '"runs":{"s1":3.5},"run_window":{"s1":2},"unmeterable":{"s2":"the runner can\'t meter aider"}}';
const GO_SPEND_EVENT = '{"five_hour":{"plan":40,"backlog":10,"all_cost":8,"resets_at":"2026-10-20T14:00:00Z"},' +
  '"seven_day":{"plan":0,"backlog":0,"all_cost":0,"resets_at":null},' +
  '"reading_at":"2026-10-20T12:00:00Z",' +
  '"runs":{"s1":3.5},"run_window":{"s1":2},"unmeterable":{"s2":"the runner can\'t meter aider"}}';

test("the Go BudgetResponse and spend event JSON parse to the run use the daemon means", () => {
  const before = Date.parse("2026-10-20T12:30:00Z");
  const u = parseUsage(JSON.parse(GO_BUDGET_RESPONSE));
  assert.deepEqual(u.five_hour, { plan: 40, backlog: 10, resets_at: "2026-10-20T14:00:00Z", all_cost: 8 });
  assert.equal(u.seven_day.resets_at, null);
  assert.equal(u.account, "default");
  assert.deepEqual(u.accounts.default, u.five_hour);
  assert.deepEqual(u.runs, { s1: 3.5 });
  assert.deepEqual(u.run_window, { s1: 2 });
  assert.deepEqual(u.unmeterable, { s2: "the runner can't meter aider" });
  assert.equal(runUse("s1", u, before), 10, "2 / 8 of a window at 40%");
  const spent = applySpend({ ...u, run_window: {}, unmeterable: {} }, JSON.parse(GO_SPEND_EVENT))!;
  assert.deepEqual(spent.run_window, { s1: 2 });
  assert.deepEqual(spent.unmeterable, { s2: "the runner can't meter aider" });
  assert.equal(runUse("s1", spent, before), 10);
});

test("a run on another account uses that account's 5-hour window", () => {
  const u = usage({ run_window: { s1: 2 }, account: "default",
    accounts: { work: { plan: 80, backlog: 0, resets_at: SOON, all_cost: 4 } } });
  assert.equal(runUse("s1", u, NOW, "work"), 40);
  assert.equal(runUse("s1", u, NOW), 4.1);
  assert.equal(runUse("s1", u, NOW, "unknown"), 4.1);
});

test("applySpend replaces windows and runs but keeps what the event does not carry", () => {
  const prev = usage({ metering: true, unmeterable: { s9: "x" } });
  const next = applySpend(prev, { five_hour: { plan: 50, backlog: 20, resets_at: SOON }, seven_day: {}, reading_at: null, runs: { s1: 2 } });
  assert.equal(next?.five_hour.backlog, 20);
  assert.equal(next?.seven_day.resets_at, null);
  assert.deepEqual(next?.runs, { s1: 2 });
  assert.deepEqual(next?.unmeterable, { s9: "x" });
  assert.equal(next?.metering, false);
  assert.equal(applySpend(prev, "junk"), prev);
});

test("the bar shows the window closest to stopping the runner", () => {
  assert.equal(tighterWindow(usage(), budget(), NOW), "five_hour", "plan 41/90 beats backlog 18/50");
  assert.equal(tighterWindow(usage({ seven_day: { plan: 40, backlog: 30, resets_at: MONDAY } }), budget(), NOW), "seven_day");
  assert.equal(tighterWindow(usage({ five_hour: { plan: 45, backlog: 0, resets_at: SOON },
    seven_day: { plan: 0, backlog: 25, resets_at: MONDAY } }), budget(), NOW), "five_hour", "a tie goes to the 5-hour window");
  assert.equal(tighterWindow(usage(), budget({ share: 0 }), NOW), "five_hour");
  assert.equal(tighterWindow(usage({ five_hour: { plan: 0, backlog: 0, resets_at: null } }), budget(), NOW), "seven_day");
  assert.equal(tighterWindow(usage({ five_hour: { plan: 9, backlog: 9, resets_at: at("2000-01-03T09:00:00") },
    seven_day: { plan: 0, backlog: 0, resets_at: null } }), budget(), NOW), null);
});

test("hover text shows both windows, the reading age and the scope", () => {
  assert.equal(usageHover(usage(), NOW), [
    "5-hour · plan 41% · backlog 12% · resets 14:00",
    "Weekly · plan 40% · backlog 18% · resets Mon 09:00",
    "read 2 min ago · all projects",
  ].join("\n"));
  assert.match(usageHover(usage({ seven_day: { plan: 0, backlog: 0, resets_at: null }, reading_at: null }), NOW),
    /Weekly · no reading\nno reading yet · all projects$/);
  assert.equal(resetLabel(SOON, NOW), "14:00");
  assert.equal(readingAge(at("2000-01-03T09:59:30"), NOW), "read just now");
  assert.equal(readingAge(at("2000-01-03T07:00:00"), NOW), "read 3 h ago");
  assert.equal(pct(12.5), "12.5%");
  assert.equal(pct(17.83), "17.8%");
});

test("run use is the run's cost in the 5-hour window over all cost in it, times plan %", () => {
  const u = usage({ runs: { s1: 50 }, run_window: { s1: 2 } });
  assert.equal(runUse("s1", u, NOW), 4.1, "lifetime cost does not count");
  assert.equal(runUse("s2", u, NOW), null);
  assert.equal(runUse("s1", { ...u, five_hour: { plan: 41, backlog: 12, resets_at: null, all_cost: 20 } }, NOW), null);
  assert.equal(runUse("s1", { ...u, five_hour: { plan: 41, backlog: 12, resets_at: SOON, all_cost: 0 } }, NOW), null);
});

test("closedGate names the first closed budget gate, keyed by gate and window", () => {
  const runner: RunnerView = { state: "off", used: 0, max: 1 };
  const g = (b: Budget | null, u: Usage | null, r: RunnerView = runner) => closedGate(b, u, r, NOW);
  assert.equal(g(budget(), usage()), null);
  assert.deepEqual(g(budget({ share: 15 }), usage()), { key: `share:seven_day:${MONDAY}`, text: "The backlog used its 15% of the week." });
  assert.deepEqual(g(budget({ share: 10 }), usage()), { key: `share:five_hour:${SOON}`, text: "The backlog used its 10% of the 5-hour window." });
  assert.deepEqual(g(budget({ stop_at: 40 }), usage()), { key: `stop_at:five_hour:${SOON}`, text: "The plan is at 41% of the 5-hour window." });
  assert.equal(g(budget({ stop_at: 40 }), usage({ five_hour: { plan: 44, backlog: 12, resets_at: SOON } }))?.key, `stop_at:five_hour:${SOON}`,
    "the key holds while the numbers move");
  const NEXT = at("2000-01-03T19:00:00");
  assert.notEqual(g(budget({ stop_at: 40 }), usage({ five_hour: { plan: 44, backlog: 12, resets_at: NEXT } }))?.key, g(budget({ stop_at: 40 }), usage())?.key,
    "a new window is a new gate, so a start asks again");
  assert.deepEqual(g(budget({ share: 0 }), null), { key: "share:zero", text: "The backlog share is 0%." });
  assert.equal(g(budget({ share: 10 }), usage({ five_hour: { plan: 41, backlog: 12, resets_at: at("2000-01-03T09:00:00") },
    seven_day: { plan: 0, backlog: 0, resets_at: null } })), null);
  const limited: RunnerView = { state: "stopped", gate: "usage_limit", reason: "Usage limit until 14:00", used: 0, max: 1 };
  assert.deepEqual(g(budget(), usage(), limited), { key: "usage_limit", text: "Usage limit until 14:00." });
  assert.equal(g(null, null, { ...limited, gate: "no_reading" }), null);
  // The daemon's runner view has no gate field: an exhausted dollar limit is read from the budget.
  const stoppedNoGate: RunnerView = { state: "stopped", reason: "Dollar limit reached", used: 0, max: 1 };
  assert.deepEqual(g(budget({ usd_limit: 5, usd_spent: 5 }), null, stoppedNoGate), { key: "usd_limit", text: "The backlog spent its $5.00 dollar limit." });
  assert.deepEqual(g(budget({ usd_limit: 5, usd_spent: 7.5 }), null), { key: "usd_limit", text: "The backlog spent its $5.00 dollar limit." });
  assert.equal(g(budget({ usd_limit: 5, usd_spent: 4.99 }), null), null);
  assert.equal(g(budget({ usd_limit: 0, usd_spent: 9 }), null), null, "0 is off");
});

test("webReason drops the clauses that name af commands", () => {
  assert.equal(webReason('Backlog share is 0% · raise it with "af backlog budget --share 50"'), "Backlog share is 0%");
  assert.equal(webReason('No plan usage seen yet · start an item by hand once to take a reading, or set "af backlog budget --usd-limit 10"'),
    "No plan usage seen yet · start an item by hand once to take a reading");
  assert.equal(webReason('The runner can\'t meter aider; start B-12 by hand with "af backlog start 12"'), "The runner can't meter aider");
  assert.equal(webReason('all 2 slots are in use; wait for a run to end or raise them with "af backlog runner --max-running 3"'),
    "all 2 slots are in use; wait for a run to end");
  assert.equal(webReason('No price for model x; add [backlog.prices."x"] to config.toml'), 'No price for model x; add [backlog.prices."x"] to config.toml');
  assert.equal(webReason("Usage limit until 14:00"), "Usage limit until 14:00");
});
