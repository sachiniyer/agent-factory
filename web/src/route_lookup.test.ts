import test from "node:test";
import assert from "node:assert/strict";
import { settleLookup } from "./route_lookup.js";
import { CreateSelectionIntent } from "./optimistic.js";

// The shell's model: resolveRoute navigates, then a by-id lookup takes the clock's
// ticket; a rail click / keyboard selection / new hash calls navigate() again.
function shell() {
  const intent = new CreateSelectionIntent();
  let selected: string | null = "a";
  const log: string[] = [];
  const effects = {
    found: (r: { id?: string }) => { selected = r.id ?? null; log.push("found"); },
    missing: () => { selected = null; log.push("missing"); },
  };
  return {
    intent, log, effects,
    selected: () => selected,
    railClick: (id: string) => { intent.navigate(); selected = id; },
    hash: () => { intent.navigate(); return intent.snapshot(); },
  };
}

test("hash to an unknown id, then a rail click before the answer: the click wins", () => {
  const s = shell();
  const ticket = s.hash();
  s.railClick("b");
  assert.equal(settleLookup(s.intent, ticket, "x", [], s.effects), "stale");
  assert.equal(s.selected(), "b");
  assert.deepEqual(s.log, []);
});

test("a late found answer after a rail click does not steal the selection", () => {
  const s = shell();
  const ticket = s.hash();
  s.railClick("b");
  assert.equal(settleLookup(s.intent, ticket, "x", [{ id: "x" }], s.effects), "stale");
  assert.equal(s.selected(), "b");
});

test("a newer hash (back/forward) supersedes a pending lookup; the newest settles once", () => {
  for (const order of ["old-first", "new-first"] as const) {
    const s = shell();
    const t1 = s.hash();
    const t2 = s.hash();
    const answer = (t: number) => settleLookup(s.intent, t, "x", [], s.effects);
    const out = order === "old-first" ? [answer(t1), answer(t2)] : [answer(t2), answer(t1)];
    assert.deepEqual(s.log, ["missing"], order);
    assert.equal(out.filter((o) => o === "stale").length, 1, order);
  }
});

test("an undisturbed lookup settles: unknown → missing, known → found", () => {
  const s = shell();
  assert.equal(settleLookup(s.intent, s.hash(), "x", [], s.effects), "missing");
  assert.equal(settleLookup(s.intent, s.hash(), "x", [{ id: "x" }], s.effects), "found");
  assert.equal(s.selected(), "x");
});
