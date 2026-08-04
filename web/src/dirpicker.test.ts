// Unit coverage for the Add-project directory picker's state rules (#2788).
//
// The picker's DOM is asserted in the Playwright selftest against a real daemon;
// what lives here is the decision it makes on every navigation, which is where
// the bug this feature could ship would be: rendering a REFUSED directory as an
// empty one. Those are opposite facts — "there is nothing here" vs "I was not
// allowed to look" — and once a refusal is flattened into `entries: []` the UI
// tells the user their repos are not on the disk they are looking at.
//
// Pure like project.ts / filter.ts: no DOM, no daemon.

import { test } from "node:test";
import assert from "node:assert/strict";

import type { DirectoryEntry, DirectoryListing } from "./api.js";
import {
  INITIAL_PICKER_STATE,
  entryNote,
  pickerFailed,
  pickerLoaded,
  pickerLoading,
  truncationNote,
} from "./dirpicker.js";

function entry(name: string, over: Partial<DirectoryEntry> = {}): DirectoryEntry {
  return { name, path: `/work/${name}`, is_repo: false, is_symlink: false, ...over };
}

function listing(over: Partial<DirectoryListing> = {}): DirectoryListing {
  return {
    path: "/work",
    parent: "/",
    home: "/root",
    is_repo: false,
    entries: [entry("mock-repo", { is_repo: true }), entry("plain")],
    truncated: false,
    ...over,
  };
}

test("a failed navigation keeps the listing the user is standing in", () => {
  const here = pickerLoaded(listing());
  const failed = pickerFailed(pickerLoading(here), "cannot read /work/locked: permission denied");

  assert.equal(failed.error, "cannot read /work/locked: permission denied");
  assert.equal(failed.loading, false);
  assert.notEqual(failed.listing, null, "a refusal must not null the listing");
  assert.deepEqual(
    failed.listing?.entries.map((e) => e.name),
    ["mock-repo", "plain"],
    "the failed descent did not happen, so the picker did not move",
  );
  assert.equal(failed.listing?.path, "/work", "the header still names where the user actually is");
});

test("a refusal on the FIRST load shows the error and no fabricated empty listing", () => {
  const failed = pickerFailed(pickerLoading(INITIAL_PICKER_STATE), "cannot read /nope: permission denied");

  assert.equal(failed.listing, null, "there is no directory to show, and none is invented");
  assert.equal(failed.error, "cannot read /nope: permission denied");
  // The distinction this pins: `listing === null` renders as "nothing loaded yet,
  // here is why", NEVER as a listing with zero entries. An empty-entries listing
  // is a positive claim ("this directory has no subdirectories") that a refusal
  // never licenses.
  assert.notDeepEqual(failed.listing, { ...listing(), entries: [] });
});

test("a successful navigation moves the picker and clears the previous error", () => {
  const refused = pickerFailed(pickerLoaded(listing()), "cannot read /work/locked: permission denied");
  const moved = pickerLoaded(listing({ path: "/work/mock-repo", parent: "/work", is_repo: true, entries: [] }));

  assert.equal(moved.error, null, "the error described a directory the user is no longer entering");
  assert.equal(moved.listing?.path, "/work/mock-repo");
  assert.equal(moved.listing?.is_repo, true);
  assert.equal(refused.error !== null, true, "the refusal was real before the move");
});

test("an in-flight navigation keeps the current view and its error", () => {
  const refused = pickerFailed(pickerLoaded(listing()), "cannot read /work/locked: permission denied");
  const loading = pickerLoading(refused);

  assert.equal(loading.loading, true);
  assert.equal(loading.error, refused.error, "the message must not blink away before the outcome is known");
  assert.equal(loading.listing?.path, "/work");
});

test("entryNote marks only what can become a project", () => {
  assert.equal(entryNote(entry("plain")), "", "a plain directory carries no mark — that is what makes it not a target");
  assert.equal(entryNote(entry("repo", { is_repo: true })), "git repo");
  assert.equal(entryNote(entry("link", { is_symlink: true })), "link");
  assert.equal(entryNote(entry("both", { is_repo: true, is_symlink: true })), "git repo · link");
});

test("truncationNote says a capped listing is capped", () => {
  assert.equal(truncationNote(listing()), "", "nothing dropped, nothing to say");
  const capped = truncationNote(listing({ truncated: true }));
  assert.match(capped, /first 2 directories/, "the cap names how many are shown");
  assert.match(capped, /type the path below/, "and points at the escape hatch");
});
