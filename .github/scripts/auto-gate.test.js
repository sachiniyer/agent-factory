const assert = require("node:assert/strict");
const test = require("node:test");
const autoGate = require("./auto-gate.js");
const { __test } = autoGate;

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

test("maintainer discussion replies do not resolve Greptile P1/P2 findings without a marker", async () => {
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
          body: "I see the tradeoff and will think about it.",
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

test("maintainer replies resolve Greptile P1/P2 findings only with an explicit marker", async () => {
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
          body: "Documented tradeoff ACCEPTED.",
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

test("gate-ack token is an explicit Greptile finding resolution marker", () => {
  assert.equal(__test.hasResolutionMarker("Root accepts this [gate-ack]."), true);
  assert.equal(__test.hasResolutionMarker("accepted in discussion, not marked"), false);
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

test("merge pins the squash merge to the evaluated head SHA", async () => {
  const github = fakeMergeGithub({ headSha: "sha-that-passed" });

  await autoGate.merge({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
  });

  assert.equal(github.mergedWith.sha, "sha-that-passed");
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

function fakeMergeGithub({ headSha }) {
  const listFiles = function listFiles() {};
  const listForRef = function listForRef() {};
  const listComments = function listComments() {};
  const listReviewComments = function listReviewComments() {};
  const merge = async function merge(options) {
    github.mergedWith = options;
    return { data: { sha: "merge-sha" } };
  };
  const responses = new Map([
    [listFiles, []],
    [listForRef, [greptileRun()]],
    [listComments, [freshGreptileSummary()]],
    [listReviewComments, []],
  ]);

  const github = {
    mergedWith: null,
    rest: {
      actions: {
        createWorkflowDispatch: async () => {},
      },
      checks: { listForRef },
      issues: { listComments },
      pulls: { listFiles, listReviewComments, merge },
    },
    graphql: async () => {
      return {
        repository: {
          pullRequest: {
            number: 1465,
            title: "Gate test",
            url: "https://example.invalid/pr/1465",
            baseRefName: "master",
            headRefOid: headSha,
            isDraft: false,
            mergeable: "MERGEABLE",
            mergeStateStatus: "CLEAN",
            author: { login: "sachiniyer" },
            labels: { nodes: [] },
            commits: {
              nodes: [
                {
                  commit: {
                    committedDate: "2026-07-09T01:00:00Z",
                  },
                },
              ],
            },
          },
        },
      };
    },
    paginate: async (fn) => responses.get(fn) || [],
    request: async (route) => {
      if (route.includes("/rules/branches/")) {
        return { data: [] };
      }
      const error = new Error("not protected");
      error.status = 404;
      throw error;
    },
  };

  return github;
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
