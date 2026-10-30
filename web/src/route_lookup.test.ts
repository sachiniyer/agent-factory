import test from "node:test";
import assert from "node:assert/strict";
import { RouteLookups, settleLookup } from "./route_lookup.js";

function run(order: "first-then-second" | "second-then-first") {
  const lookups = new RouteLookups();
  const log: string[] = [];
  const effects = { found: (r: { id: string }) => log.push("found:" + r.id), missing: () => log.push("missing") };
  const t1 = lookups.begin();
  const t2 = lookups.begin(); // the same hash resolved twice (hashchange + a resync)
  const answer = (t: number) => settleLookup(lookups, t, "x", [], effects);
  const out = order === "first-then-second" ? [answer(t1), answer(t2)] : [answer(t2), answer(t1)];
  return { out, log };
}

test("an unknown id ends in exactly one terminal effect in either answer order", () => {
  for (const order of ["first-then-second", "second-then-first"] as const) {
    const { out, log } = run(order);
    assert.deepEqual(log, ["missing"], order);
    assert.equal(out.filter((o) => o === "stale").length, 1, order);
  }
});

test("a late answer for a hash the user navigated away from is ignored", () => {
  const lookups = new RouteLookups();
  const log: string[] = [];
  const stale = lookups.begin();
  lookups.begin(); // navigation to another hash
  const out = settleLookup(lookups, stale, "x", [{ id: "x" }], { found: () => log.push("found"), missing: () => log.push("missing") });
  assert.equal(out, "stale");
  assert.deepEqual(log, []);
});

test("a found row settles once", () => {
  const lookups = new RouteLookups();
  const log: string[] = [];
  const t = lookups.begin();
  const out = settleLookup(lookups, t, "x", [{ id: "x" }], { found: (r) => log.push(r.id), missing: () => log.push("missing") });
  assert.equal(out, "found");
  assert.deepEqual(log, ["x"]);
});
