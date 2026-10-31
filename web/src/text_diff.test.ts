import assert from "node:assert/strict";
import { test } from "node:test";
import { applyChanges, diffText, hasConflictMarkers, landedIn, mergeWithMarkers } from "./text_diff.js";

test("diffText round-trips and leaves unchanged lines alone", () => {
  const cases: [string, string][] = [
    ["a\nb\nc", "a\nB\nc"],
    ["a\nb\nc\n", "a\nc\n"],
    ["a", "a\nb"],
    ["", "x"],
    ["x", ""],
    ["one\ntwo\nthree", "zero\none\ntwo\nthree\nfour"],
    ["a\nb", "a\nb\n"],
  ];
  for (const [before, after] of cases) {
    const changes = diffText(before, after);
    assert.equal(applyChanges(before, changes), after, JSON.stringify([before, after]));
  }
  const changes = diffText("keep\nold\nkeep too", "keep\nnew\nkeep too");
  assert.deepEqual(changes, [{ from: 5, to: 9, insert: "new\n" }]);
});

test("diffText of equal texts is empty", () => {
  assert.deepEqual(diffText("same\n", "same\n"), []);
});

test("mergeWithMarkers takes each side's own changes", () => {
  const base = "a\nb\nc\nd";
  assert.equal(mergeWithMarkers(base, "A\nb\nc\nd", "a\nb\nc\nD"), "A\nb\nc\nD");
  assert.equal(mergeWithMarkers(base, "a\nb\nX\nc\nd", "a\nb\nc\nd"), "a\nb\nX\nc\nd");
  assert.equal(mergeWithMarkers(base, "a\nB\nc\nd", "a\nB\nc\nd"), "a\nB\nc\nd");
});

test("mergeWithMarkers marks only the region both sides changed", () => {
  const merged = mergeWithMarkers("a\nb\nc", "a\nmine\nc", "a\ntheirs\nc");
  assert.equal(merged, "a\n<<<<<<< mine\nmine\n=======\ntheirs\n>>>>>>> agent\nc");
  assert.ok(hasConflictMarkers(merged));
  assert.ok(!hasConflictMarkers("a\nb"));
});

test("landedIn recognises a save whose changes the head holds beside edits to the lines next to them", () => {
  const base = "a8\na11\na9\nu1\na3";
  const saved = "u17\na11\na9\nu1\na3";
  // The save landed, and an agent has since edited the two lines after it: the merge of the save onto
  // this head is a conflict (the edits touch), but every change the save made is in the head.
  assert.equal(landedIn(base, saved, "u17\na15\na14\nu1\na3"), true);
  assert.notEqual(mergeWithMarkers(base, saved, "u17\na15\na14\nu1\na3"), "u17\na15\na14\nu1\na3");
  // The save did not land: the head has only the agent's edits.
  assert.equal(landedIn(base, saved, "a8\na15\na14\nu1\na3"), false);
  // A save that changes nothing, or only deletes, cannot be shown to have landed.
  assert.equal(landedIn(base, base, base), false);
  assert.equal(landedIn("a\nb\nc", "a\nc", "a\nc"), false);
  // An insertion that landed between the lines around it.
  assert.equal(landedIn("a\nb", "a\nnew\nb", "a\nnew\nx\nb"), true);
  assert.equal(landedIn("a\nb", "a\nnew\nb", "a\nx\nb"), false);
});
