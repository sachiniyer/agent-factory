import assert from "node:assert/strict";
import { test } from "node:test";
import { missingFrom, RosterOrder } from "./docs_roster.js";

test("a list reply older than one already used is dropped", () => {
  const r = new RosterOrder();
  const refresh = r.begin(); // a refresh goes out
  const afterAdd = r.begin(); // Add doc's fetch goes out later
  assert.equal(r.accept(afterAdd), true);
  assert.equal(r.accept(refresh), false, "its list lacks the doc that was just added");
});

test("replies in order are all used, and a reply is not used twice out of order", () => {
  const r = new RosterOrder();
  const a = r.begin();
  const b = r.begin();
  assert.equal(r.accept(a), true);
  assert.equal(r.accept(b), true);
  assert.equal(r.accept(a), false);
});

test("missingFrom names the open docs the list lacks", () => {
  assert.deepEqual(missingFrom(["goals", "api", "notes"], ["goals", "api"]), ["notes"]);
  assert.deepEqual(missingFrom([], ["goals"]), []);
});
