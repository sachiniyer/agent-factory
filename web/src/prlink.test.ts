// Unit coverage for the pane header's PR link decision (#3285). Pinned as a
// pure function, the tablabel.ts way: each of these can be wrong without
// anything throwing — a dead link for a record with no URL, a badge that never
// updates after a merge, a DOM write per snapshot for an unchanged badge.

import { test } from "node:test";
import assert from "node:assert/strict";

import { prLinkView } from "./prlink.js";

test("prLinkView: no pr_info renders nothing", () => {
  const view = prLinkView(undefined);
  assert.equal(view.visible, false);
  assert.equal(view.sig, "");
});

test("prLinkView: a numberless record is the daemon's 'no PR' projection", () => {
  // SetPRInfo with Number 0 clears the badge; the projection then carries an
  // empty pr_info object rather than a real PR.
  assert.equal(prLinkView({}).visible, false);
});

test("prLinkView: a record without a URL has nothing to open — no dead control", () => {
  assert.equal(prLinkView({ number: 41, state: "OPEN" }).visible, false);
});

test("prLinkView: number and state join with the house separator, state lowercased", () => {
  const view = prLinkView({ number: 41, state: "OPEN", url: "https://example.com/pr/41", title: "fix: sweep" });
  assert.equal(view.visible, true);
  // Copy conventions: sentence case, ` · ` joining fragments, no caps-shouting —
  // gh reports states as OPEN/MERGED/CLOSED, which must not shout in the header.
  assert.equal(view.text, "PR #41 · open");
  assert.equal(view.href, "https://example.com/pr/41");
  assert.equal(view.title, "fix: sweep");
});

test("prLinkView: a stateless record still names the PR", () => {
  assert.equal(prLinkView({ number: 41, url: "https://example.com/pr/41" }).text, "PR #41");
});

test("prLinkView: the signature moves exactly when the badge content moves", () => {
  const a = prLinkView({ number: 41, state: "OPEN", url: "https://example.com/pr/41" });
  const same = prLinkView({ number: 41, state: "OPEN", url: "https://example.com/pr/41", branch: "af/x" });
  // branch is provenance, not content: it does not change what the header draws.
  assert.equal(a.sig, same.sig);
  const merged = prLinkView({ number: 41, state: "MERGED", url: "https://example.com/pr/41" });
  // The state flip after a merge is the update the daemon sweep exists to
  // deliver (#3232); an unmoved signature here would pin the badge at "open".
  assert.notEqual(a.sig, merged.sig);
});
