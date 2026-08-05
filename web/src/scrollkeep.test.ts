// Pins the "same list or a different one" decision behind the scroll keep (#2933).
//
// The positive case is the point of the change, but the NEGATIVE one is what keeps the
// fix from being its own bug: restoring an offset after a project switch or a filter
// change would drop the reader halfway down a list they have never seen. A helper that
// restored unconditionally would look like it worked in every manual test and be wrong
// exactly when the list changed identity.

import { test } from "node:test";
import assert from "node:assert/strict";

import { keptScrollTop, listToken, rebuildKeepingScroll } from "./scrollkeep.js";

test("the same list keeps the reader's place", () => {
  assert.equal(keptScrollTop("repo-a none", "repo-a none", 420), 420);
});

test("a DIFFERENT list starts at the top", () => {
  assert.equal(keptScrollTop("repo-a none", "repo-b none", 420), 0, "switching project must not land mid-list");
  assert.equal(keptScrollTop("repo-a none", "repo-a archived", 420), 0, "changing the filter changes the membership");
});

test("the first render has nothing to keep", () => {
  assert.equal(keptScrollTop(null, "repo-a none", 420), 0);
});

test("tokens cannot collide across differently-split scopes", () => {
  // "a" + "b|c" must not read as "a|b" + "c" — the separator has to be a character
  // that cannot appear in a repo path or a filter name.
  assert.notEqual(listToken(["a", "b c"]), listToken(["a b", "c"]));
  assert.equal(listToken(["a", null]), listToken(["a", null]), "the same scope is the same token");
  assert.notEqual(listToken([null]), listToken(["none"]), "an absent scope is not the literal string");
});

test("a rebuild restores the offset it captured BEFORE the rebuild", () => {
  // The clamp happens during the rebuild, which is why the value has to be read first.
  const el = { scrollTop: 900 };
  rebuildKeepingScroll(el, "same", "same", () => {
    el.scrollTop = 0; // what replaceChildren does to a scroll container
  });
  assert.equal(el.scrollTop, 900);
});

test("a rebuild of a different list leaves it at the top", () => {
  const el = { scrollTop: 900 };
  rebuildKeepingScroll(el, "before", "after", () => {
    el.scrollTop = 0;
  });
  assert.equal(el.scrollTop, 0);
});

test("no write when the offset is already what it should be", () => {
  // Assigning scrollTop can fire a scroll event; 0 → 0 on every churn rebuild would be
  // pure noise for anything listening.
  let writes = 0;
  const el = {
    _top: 0,
    get scrollTop(): number {
      return this._top;
    },
    set scrollTop(v: number) {
      writes += 1;
      this._top = v;
    },
  };
  rebuildKeepingScroll(el, "same", "same", () => {
    /* a rebuild that did not move it */
  });
  assert.equal(writes, 0, "an unchanged offset must not be rewritten");
});
