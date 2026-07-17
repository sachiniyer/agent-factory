"use strict";

// Unit tests for the pure staleness decision. Run with the runner's built-in
// node:test (no package.json, no install) — wired into CI in pr.yml alongside
// the auto-gate helper tests:
//   node --test .github/scripts/stale-external-pr.test.js

const assert = require("node:assert/strict");
const test = require("node:test");
const { __test } = require("./stale-external-pr.js");
const { decide, preScreen, isExternalHead } = __test;

const NOW = new Date("2026-07-17T12:00:00Z");
const BASE = "sachiniyer/agent-factory";
const hoursAgo = (h) => new Date(NOW.getTime() - h * 3600 * 1000).toISOString();

function forkPR(overrides = {}) {
  return {
    number: 1,
    state: "open",
    author: "ext-user",
    authorAssociation: "CONTRIBUTOR",
    headRepoFullName: "ext-user/agent-factory",
    baseRepoFullName: BASE,
    headRef: "patch-1",
    labels: [],
    reviews: [],
    commits: [],
    comments: [],
    ...overrides,
  };
}

function maintReview(state, at, author = "sachiniyer") {
  return { author, authorAssociation: "OWNER", state, submittedAt: at };
}

// ---- The five required cases -------------------------------------------------

test("fork PR, changes requested 13h ago, no response → CLOSE", () => {
  const pr = forkPR({ reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(13))] });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, true, d.reason);
});

test("fork PR, contributor pushed a commit 1h ago → KEEP", () => {
  const pr = forkPR({
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(13))],
    commits: [{ author: "ext-user", committedAt: hoursAgo(1) }],
  });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /ball in maintainer/);
});

test("internal-branch PR (same repo) → NEVER close", () => {
  const pr = forkPR({
    headRepoFullName: BASE,
    headRef: "sachiniyer/some-session",
    author: "sachiniyer",
    authorAssociation: "OWNER",
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(48))],
  });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /internal-branch/);
});

test("maintainer owes the response (contributor commented after CR) → KEEP", () => {
  const pr = forkPR({
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(20))],
    comments: [
      { author: "ext-user", authorAssociation: "CONTRIBUTOR", createdAt: hoursAgo(2) },
    ],
  });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /ball in maintainer/);
});

test("pending maintainer approval → NEVER close", () => {
  const pr = forkPR({
    reviews: [
      maintReview("CHANGES_REQUESTED", hoursAgo(30)),
      maintReview("APPROVED", hoursAgo(20)),
    ],
  });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /APPROVED/);
});

// ---- Threshold / grace -------------------------------------------------------

test("change request younger than 12h → grace, KEEP", () => {
  const pr = forkPR({ reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(6))] });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /grace/);
});

test("change request exactly 12h old → CLOSE (boundary is inclusive)", () => {
  const pr = forkPR({ reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(12))] });
  assert.equal(decide(pr, NOW).shouldClose, true);
});

// ---- Review ordering / decisional state --------------------------------------

test("changes requested AFTER an earlier approval → CLOSE when stale", () => {
  const pr = forkPR({
    reviews: [
      maintReview("APPROVED", hoursAgo(30)),
      maintReview("CHANGES_REQUESTED", hoursAgo(20)),
    ],
  });
  assert.equal(decide(pr, NOW).shouldClose, true);
});

test("no changes-requested review at all → KEEP", () => {
  const pr = forkPR({ reviews: [maintReview("COMMENTED", hoursAgo(40))] });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /no maintainer changes-requested/);
});

test("changes requested by a NON-maintainer is ignored → KEEP", () => {
  const pr = forkPR({
    reviews: [
      { author: "rando", authorAssociation: "NONE", state: "CHANGES_REQUESTED", submittedAt: hoursAgo(40) },
    ],
  });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /no maintainer changes-requested/);
});

test("MEMBER and COLLABORATOR reviews also count as maintainer change requests", () => {
  for (const assoc of ["MEMBER", "COLLABORATOR"]) {
    const pr = forkPR({
      reviews: [
        { author: "other-maint", authorAssociation: assoc, state: "CHANGES_REQUESTED", submittedAt: hoursAgo(20) },
      ],
    });
    assert.equal(decide(pr, NOW).shouldClose, true, `assoc=${assoc}`);
  }
});

// ---- Ball-in-court nuances ---------------------------------------------------

test("contributor's follow-up COMMENTED review after CR → KEEP", () => {
  const pr = forkPR({
    reviews: [
      maintReview("CHANGES_REQUESTED", hoursAgo(20)),
      { author: "ext-user", authorAssociation: "CONTRIBUTOR", state: "COMMENTED", submittedAt: hoursAgo(3) },
    ],
  });
  assert.equal(decide(pr, NOW).shouldClose, false);
});

test("contributor comment BEFORE the change request does not reset the clock → CLOSE", () => {
  const pr = forkPR({
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(20))],
    comments: [
      { author: "ext-user", authorAssociation: "CONTRIBUTOR", createdAt: hoursAgo(30) },
    ],
  });
  assert.equal(decide(pr, NOW).shouldClose, true);
});

test("a third party's comment after the CR does not keep it open → CLOSE", () => {
  const pr = forkPR({
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(20))],
    comments: [
      { author: "passerby", authorAssociation: "NONE", createdAt: hoursAgo(1) },
    ],
  });
  assert.equal(decide(pr, NOW).shouldClose, true);
});

test("any commit after the CR keeps it open, even if attributed to no one → KEEP", () => {
  const pr = forkPR({
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(20))],
    commits: [{ author: "", committedAt: hoursAgo(2) }],
  });
  assert.equal(decide(pr, NOW).shouldClose, false);
});

// ---- Author / fork identity --------------------------------------------------

test("maintainer-authored fork PR is NEVER closed", () => {
  const pr = forkPR({
    author: "sachiniyer",
    authorAssociation: "OWNER",
    headRepoFullName: "sachiniyer/agent-factory-fork",
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(40), "someone-else")],
  });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /maintainer/);
});

test("bot-authored PR (allowlisted login) is NEVER closed", () => {
  const pr = forkPR({
    author: "app-detail-app[bot]",
    authorAssociation: "NONE",
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(40))],
  });
  assert.equal(decide(pr, NOW).shouldClose, false);
});

test("deleted fork head (no head repo) still counts as external → CLOSE when stale", () => {
  const pr = forkPR({
    headRepoFullName: "",
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(20))],
  });
  assert.equal(decide(pr, NOW).shouldClose, true);
});

// ---- State gate --------------------------------------------------------------

test("already-closed PR → SKIP", () => {
  const pr = forkPR({
    state: "closed",
    reviews: [maintReview("CHANGES_REQUESTED", hoursAgo(40))],
  });
  const d = decide(pr, NOW);
  assert.equal(d.shouldClose, false);
  assert.match(d.reason, /not open/);
});

// ---- Helpers -----------------------------------------------------------------

test("isExternalHead: same repo is internal; fork and deleted-head are external", () => {
  assert.equal(isExternalHead(BASE, BASE), false);
  assert.equal(isExternalHead("ext/agent-factory", BASE), true);
  assert.equal(isExternalHead("", BASE), true);
  assert.equal(isExternalHead(null, BASE), true);
  assert.equal(isExternalHead(undefined, BASE), true);
});

test("preScreen mirrors decide's cheap gates (fork / maintainer / open)", () => {
  const internal = preScreen(
    { state: "open", head: { repo: { full_name: BASE }, ref: "sachiniyer/x" }, user: { login: "sachiniyer" }, author_association: "OWNER" },
    BASE,
  );
  assert.equal(internal.eligible, false);
  assert.match(internal.reason, /internal-branch/);

  const eligible = preScreen(
    { state: "open", head: { repo: { full_name: "ext/agent-factory" }, ref: "patch-1" }, user: { login: "ext-user" }, author_association: "CONTRIBUTOR" },
    BASE,
  );
  assert.equal(eligible.eligible, true);

  const closed = preScreen(
    { state: "closed", head: { repo: { full_name: "ext/agent-factory" }, ref: "patch-1" }, user: { login: "ext-user" }, author_association: "CONTRIBUTOR" },
    BASE,
  );
  assert.equal(closed.eligible, false);
  assert.match(closed.reason, /not open/);
});
