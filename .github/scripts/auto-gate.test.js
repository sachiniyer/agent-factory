const assert = require("node:assert/strict");
const test = require("node:test");
const { __test } = require("./auto-gate.js");

test("required check matching respects the required source app", () => {
  const spec = { context: "Build", sourceAppId: 15368 };
  const checkRuns = [
    {
      name: "Build",
      app: { id: 999, slug: "spoof" },
      status: "completed",
      conclusion: "success",
      completed_at: "2026-07-09T01:00:00Z",
    },
    {
      name: "Build",
      app: { id: 15368, slug: "github-actions" },
      status: "in_progress",
      conclusion: null,
      started_at: "2026-07-09T00:59:00Z",
    },
  ];

  const state = __test.latestRequiredState(spec, checkRuns, []);

  assert.equal(state.ok, false);
  assert.match(state.description, /github-actions \(15368\)/);
});

test("stale Greptile confidence summary blocks even with a passing score", async () => {
  const result = await __test.evaluateGreptile({
    github: fakeGithub({
      checkRuns: [greptileRun()],
      issueComments: [
        {
          user: { login: "greptile-apps" },
          body: "Confidence Score: 5/5",
          created_at: "2026-07-09T00:00:00Z",
          updated_at: "2026-07-09T00:00:00Z",
        },
      ],
      reviewComments: [],
    }),
    context: fakeContext(),
    number: 1465,
    sha: "abc123",
    lastCommitDate: "2026-07-09T01:00:00Z",
    core: fakeCore(),
  });

  assert.equal(result.ok, false);
  assert.match(result.reasons.join("\n"), /summary comment is older than the head commit/);
});

test("external replies do not resolve Greptile P1/P2 findings", async () => {
  const result = await __test.evaluateGreptile({
    github: fakeGithub({
      checkRuns: [greptileRun()],
      issueComments: [freshGreptileSummary()],
      reviewComments: [
        greptileFinding({ id: 10 }),
        {
          id: 11,
          in_reply_to_id: 10,
          user: { login: "outside-user" },
          body: "Looks fine to me.",
          created_at: "2026-07-09T01:03:00Z",
        },
      ],
    }),
    context: fakeContext(),
    number: 1465,
    sha: "abc123",
    lastCommitDate: "2026-07-09T01:00:00Z",
    core: fakeCore(),
  });

  assert.equal(result.ok, false);
  assert.match(result.reasons.join("\n"), /1 unresolved Greptile P1\/P2 inline finding/);
});

test("maintainer replies resolve Greptile P1/P2 findings", async () => {
  const result = await __test.evaluateGreptile({
    github: fakeGithub({
      checkRuns: [greptileRun()],
      issueComments: [freshGreptileSummary()],
      reviewComments: [
        greptileFinding({ id: 10 }),
        {
          id: 11,
          in_reply_to_id: 10,
          user: { login: "sachiniyer" },
          body: "Documented tradeoff accepted.",
          created_at: "2026-07-09T01:03:00Z",
        },
      ],
    }),
    context: fakeContext(),
    number: 1465,
    sha: "abc123",
    lastCommitDate: "2026-07-09T01:00:00Z",
    core: fakeCore(),
  });

  assert.equal(result.ok, true);
});

test("outdated Greptile findings are not counted as unresolved", async () => {
  const result = await __test.evaluateGreptile({
    github: fakeGithub({
      checkRuns: [greptileRun()],
      issueComments: [freshGreptileSummary()],
      reviewComments: [greptileFinding({ id: 10, position: null })],
    }),
    context: fakeContext(),
    number: 1465,
    sha: "abc123",
    lastCommitDate: "2026-07-09T01:00:00Z",
    core: fakeCore(),
  });

  assert.equal(result.ok, true);
});

function fakeGithub({ checkRuns, issueComments, reviewComments }) {
  const listForRef = function listForRef() {};
  const listComments = function listComments() {};
  const listReviewComments = function listReviewComments() {};
  const responses = new Map([
    [listForRef, checkRuns],
    [listComments, issueComments],
    [listReviewComments, reviewComments],
  ]);

  return {
    rest: {
      checks: { listForRef },
      issues: { listComments },
      pulls: { listReviewComments },
    },
    paginate: async (fn) => responses.get(fn) || [],
  };
}

function fakeContext() {
  return { repo: { owner: "sachiniyer", repo: "agent-factory" } };
}

function fakeCore() {
  return {
    info: () => {},
    notice: () => {},
    setOutput: () => {},
    warning: () => {},
  };
}

function greptileRun() {
  return {
    name: "Greptile Review",
    app: { id: 123, slug: "greptile" },
    status: "completed",
    conclusion: "success",
    completed_at: "2026-07-09T01:05:00Z",
  };
}

function freshGreptileSummary() {
  return {
    user: { login: "greptile-apps" },
    body: "Confidence Score: 4/5",
    created_at: "2026-07-09T01:02:00Z",
    updated_at: "2026-07-09T01:02:00Z",
  };
}

function greptileFinding({ id, position = 7 }) {
  return {
    id,
    user: { login: "greptile-apps" },
    body: "P1: this needs attention",
    created_at: "2026-07-09T01:01:00Z",
    position,
  };
}
