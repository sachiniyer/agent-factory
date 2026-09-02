const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const autoGate = require("./auto-gate.js");
const { __test } = autoGate;

const HEAD_SHA = "0a5393dd71ddbbf66486d31939728f9947c843bb";
const OTHER_SHA = "da0a05ea3b9036a12f67a3b3877d16dd0dac893d";
const ACTIONS_APP_ID = 15368;
const AUTO_GATE_WORKFLOW = path.join(__dirname, "..", "workflows", "auto-gate.yml");

test("Auto Gate can be recovered manually by PR number", () => {
  const workflow = fs.readFileSync(AUTO_GATE_WORKFLOW, "utf8");
  const helper = fs.readFileSync(path.join(__dirname, "auto-gate.js"), "utf8");

  assert.match(workflow, /workflow_dispatch:\s+inputs:\s+pr_number:/);
  assert.match(
    workflow,
    /pull_request_target:\s+types: \[opened, reopened, closed, synchronize, edited, converted_to_draft, ready_for_review, labeled, unlabeled, auto_merge_enabled, auto_merge_disabled\]/,
  );
  assert.match(workflow, /workflow_run:\s+workflows: \[PR Validation\]\s+types: \[completed\]/);
  assert.match(workflow, /pr_number:\s+[\s\S]*?required: true[\s\S]*?type: number/);
  assert.match(
    workflow,
    /targets: \$\{\{ steps\.evaluate\.outputs\.targets \}\}/,
  );
  assert.match(
    workflow,
    /concurrency:\s+group: auto-gate-aggregate-\$\{\{ matrix\.aggregate\.head_sha \}\}\s+cancel-in-progress: false/,
  );
  assert.match(workflow, /strategy:\s+fail-fast: false\s+matrix:\s+aggregate:/);
  assert.match(
    workflow,
    /invalidate-gate:[\s\S]*?await autoGate\.invalidateAggregateDecision\([\s\S]*?apply-gate:[\s\S]*?needs: \[auto-gate, invalidate-gate\]/,
  );
  assert.match(
    workflow,
    /invalidate-gate:[\s\S]*?outputs:\s+invalidated_heads: \$\{\{ steps\.invalidate\.outputs\.invalidated_heads \}\}/,
  );
  assert.match(
    workflow,
    /core\.setOutput\("invalidated_heads", JSON\.stringify\(invalidatedHeads\)\)[\s\S]*?if \(failures\.length > 0\)[\s\S]*?throw new AggregateError/,
  );
  assert.match(
    workflow,
    /apply-gate:[\s\S]*?needs\.invalidate-gate\.outputs\.invalidated_heads != ''[\s\S]*?matrix:\s+aggregate: \$\{\{ fromJSON\(needs\.invalidate-gate\.outputs\.invalidated_heads\) \}\}/,
  );
  assert.doesNotMatch(workflow, /needs\.invalidate-gate\.result == 'success'/);
  assert.match(workflow, /HEAD_SHA: \$\{\{ matrix\.aggregate\.head_sha \}\}/);
  assert.match(workflow, /TARGETS_JSON: \$\{\{ needs\.auto-gate\.outputs\.targets \}\}/);
  assert.match(workflow, /PR_NUMBER: \$\{\{ inputs\.pr_number \|\| '' \}\}/);
  assert.match(workflow, /prNumber: explicitPrNumber/);
  assert.match(
    workflow,
    /- name: Evaluate, report, and merge the serialized head[\s\S]*?github-token: \$\{\{ github\.token \}\}/,
  );
  assert.match(workflow, /await autoGate\.processAggregateHead\(/);
  assert.match(workflow, /mergeEnabled: process\.env\.MERGE_ENABLED === "true"/);
  assert.match(workflow, /typeof autoGate\.resolveTargets === "function"/);
  assert.match(workflow, /context\.eventName === "workflow_dispatch" && targets\.length === 0/);
  assert.match(workflow, /evaluationFailed = result\.reasons\.some/);
  assert.match(workflow, /core\.setOutput\("targets", "\[\]"\)/);
  assert.match(
    workflow,
    /helper is not present[\s\S]*?core\.setOutput\("targets", "\[\]"\);\s+core\.setOutput\("aggregate_heads", "\[\]"\)/,
  );
  assert.match(
    workflow,
    /const knownEventHeads =[\s\S]*?payload\.pull_request\?\.head\?\.sha[\s\S]*?catch \(error\)[\s\S]*?const readFailure = autoGate\?\.isReadFailure\?\.\(error\) === true;[\s\S]*?core\.setOutput\("targets", "\[\]"\);[\s\S]*?JSON\.stringify\([\s\S]*?knownEventHeads\.map[\s\S]*?read_failure: readFailure \? summary : ""[\s\S]*?if \(readFailure\)[\s\S]*?core\.setFailed\(summary\);[\s\S]*?return;[\s\S]*?throw error;/,
  );
  assert.match(
    workflow,
    /const knownEventHeads =[\s\S]*?payload\.pull_request\?\.head\?\.sha,\s+payload\.after,[\s\S]*?payload\.before,[\s\S]*?filter\(\(value\) => \/\^\[0-9a-f\]\{40\}\$\/\.test\(value\)\)\)\];/,
  );
  assert.doesNotMatch(workflow, /knownEventHeads =[\s\S]{0,500}\.sort\(\)/);
  assert.match(workflow, /READ_FAILURE: \$\{\{ matrix\.aggregate\.read_failure \|\| '' \}\}/);
  assert.match(workflow, /readFailureReason: process\.env\.READ_FAILURE/);
  assert.match(
    workflow,
    /if: >-\s+always\(\) &&\s+needs\.auto-gate\.outputs\.aggregate_heads != '' &&\s+needs\.auto-gate\.outputs\.aggregate_heads != '\[\]'/,
  );
  assert.match(workflow, /aggregate_heads: \$\{\{ steps\.evaluate\.outputs\.aggregate_heads \}\}/);
  assert.doesNotMatch(workflow, /^  (?:begin-aggregate|aggregate-gate|merge-gate):/m);
  assert.doesNotMatch(workflow, /^  pull_request:/m);
  assert.doesNotMatch(workflow, /AUTO_GATE_TOKEN/);
  assert.doesNotMatch(helper, /payload\.action|context\.payload\.action/);
});

test("failed aggregate invalidations retry before entering the serialized lane", async () => {
  const exhausted = { head_sha: HEAD_SHA };
  const fenced = { head_sha: OTHER_SHA };
  const run = await runInvalidateGateStep({
    aggregateHeads: [exhausted, fenced],
    invalidateResults: {
      [HEAD_SHA]: [{ writeState: "read-only" }, { writeState: "read-only" }],
      [OTHER_SHA]: [{ writeState: "created" }],
    },
  });

  assert.deepEqual(
    run.attempts,
    [HEAD_SHA, HEAD_SHA, OTHER_SHA],
    "a failed invalidation must retry exactly once before the serialized lane",
  );
  assert.deepEqual(
    JSON.parse(run.outputs.invalidated_heads),
    [fenced],
    "a head whose invalidation exhausted its retry must not enter the serialized lane",
  );
  assert.ok(run.error, "an exhausted invalidation must still fail the invalidate step");
  assert.match(run.error.message, /1 aggregate invalidation\(s\) failed after the pre-lane retry/);
});

test("a retried invalidation that succeeds admits its head to the serialized lane", async () => {
  const run = await runInvalidateGateStep({
    aggregateHeads: [{ head_sha: HEAD_SHA }],
    invalidateResults: {
      [HEAD_SHA]: [{ writeState: "read-only" }, { writeState: "created" }],
    },
  });

  assert.deepEqual(run.attempts, [HEAD_SHA, HEAD_SHA]);
  assert.deepEqual(JSON.parse(run.outputs.invalidated_heads), [{ head_sha: HEAD_SHA }]);
  assert.equal(run.error, null, "a rescued head must not fail the invalidate step");
});

test("manual recovery reports a previously absent gate distinctly", async () => {
  const github = fakeGateGithub({ checkRuns: happyCheckRuns() });
  const result = {
    prNumber: "1465",
    headSha: HEAD_SHA,
    shouldMerge: false,
    summary: "BLOCKED: waiting on review",
  };

  const report = await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result,
    manual: true,
  });

  assert.equal(report.state, "never-ran");
  assert.equal(github.createdChecks[0].conclusion, "failure");
  assert.match(github.createdChecks[0].output.title, /^NEVER_RAN:/);
});

test("an evaluated but blocked gate reports WAITING rather than NEVER_RAN", async () => {
  const github = fakeGateGithub({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        id: 321,
        name: decisionName(1465, HEAD_SHA),
        externalId: decisionExternalId(1465, HEAD_SHA),
        conclusion: "failure",
      }),
    ],
  });
  const result = {
    prNumber: "1465",
    headSha: HEAD_SHA,
    shouldMerge: false,
    summary: "BLOCKED: waiting on review",
  };

  const report = await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result,
    manual: false,
  });

  assert.equal(report.state, "waiting");
  assert.equal(github.createdChecks.length, 0);
  assert.equal(github.updatedChecks[0].check_run_id, 321);
  assert.match(github.updatedChecks[0].output.title, /^WAITING:/);
});

test("a read-only fork token leaves the decision unreported without failing the gate", async () => {
  const error = new Error("Resource not accessible by integration");
  error.status = 403;
  const github = fakeGateGithub({ checkRuns: happyCheckRuns(), checkWriteError: error });
  const warnings = [];
  const core = { ...fakeCore(), warning: (message) => warnings.push(message) };

  const report = await autoGate.reportDecision({
    github,
    context: fakeContext({ pull_request: { head: { repo: { fork: true } } } }),
    core,
    result: {
      prNumber: 1465,
      headSha: HEAD_SHA,
      baseRefName: "master",
      isOpen: true,
      shouldMerge: false,
      summary: "BLOCKED: author is not an allowed maintainer/app",
    },
  });

  assert.equal(report.state, "read-only");
  assert.equal(github.createdChecks.length, 0);
  assert.match(warnings.join("\n"), /could not publish.*read-only token/i);

  const baseGithub = fakeGateGithub({ checkRuns: happyCheckRuns(), checkWriteError: error });
  await assert.rejects(
    autoGate.reportDecision({
      github: baseGithub,
      context: fakeContext(),
      core: fakeCore(),
      result: {
        prNumber: 1465,
        headSha: HEAD_SHA,
        baseRefName: "master",
        isOpen: true,
        shouldMerge: false,
        summary: "BLOCKED: waiting",
      },
    }),
    /Resource not accessible by integration/,
  );
});

test("a non-allowed author gets a passing manual decision without an automatic merge", async () => {
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: true,
    files: ["app/termpane.go"],
    issueComments: [],
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "manual");
  assert.equal(transaction.aggregate.ok, true);
  assert.equal(github.mergedWith, null);
  const exactDecision = github.createdChecks.find(
    (check) => check.name === decisionName(1465, HEAD_SHA),
  );
  assert.equal(exactDecision.conclusion, "success");
  assert.match(
    exactDecision.output.summary,
    /Auto Gate does not auto-merge PRs from this author; a maintainer must review and merge manually\./,
  );
  assert.match(exactDecision.output.summary, /missing the play-tested label/);
  assert.match(exactDecision.output.summary, /Codex has not reviewed head/);
  assert.doesNotMatch(exactDecision.output.summary, /not an allowed maintainer/);
  assert.equal(github.updatedChecks.at(-1).conclusion, "success");
  assert.deepEqual(github.disabledAutoMergePullRequestIds, ["PR_node_1465"]);
  // The disable is the last thing before the aggregate PASS — the only green
  // branch protection can consume.
  assert.deepEqual(github.operations.slice(0, 4), [
    "check:create",
    "check:create",
    "auto-merge:disable",
    "check:update",
  ]);
});

test("a native auto-merge cancellation failure leaves the manual-only aggregate red", async () => {
  const error = new Error("cannot disable native auto-merge");
  error.status = 500;
  const github = fakeGateGithub({
    author: "outside-contributor",
    nativeAutoMergeEnabled: true,
    nativeAutoMergeDisableError: error,
  });

  await assert.rejects(
    autoGate.processAggregateHead({
      github,
      context: fakeContext(),
      core: fakeCore(),
      headSha: HEAD_SHA,
      targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
      mergeEnabled: true,
    }),
    /cannot disable native auto-merge/,
  );

  assert.equal(github.nativeAutoMergeDisableAttempts, 1);
  // The aggregate — the only consumable green — was never published.
  assert.equal(github.createdChecks[0].name, "Auto Gate decision");
  assert.equal(github.createdChecks[0].conclusion, "failure");
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "no passing aggregate may be published when the disable failed",
  );
});

test("auto-merge armed after the PR read is still disabled before the green", async () => {
  // #3381: nativeAutoMergeEnabled was captured once in getPullRequest, and the
  // files, required-checks, comments, reviews and review-comment reads all ran
  // before the guard consulted it. Auto-merge armed anywhere in that window was
  // invisible, the disable was skipped, and a PASSING decision published on a PR
  // the gate had just declared maintainer-review-only — which GitHub could then
  // merge on that very green.
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: false,
    nativeAutoMergeArmedAfterRead: true,
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "manual");
  assert.deepEqual(github.disabledAutoMergePullRequestIds, ["PR_node_1465"]);
  assert.equal(github.nativeAutoMergeArmed, false);
  // Disabled immediately BEFORE the aggregate PASS, which is the only green
  // branch protection consumes. Ordering is the whole mechanism.
  assert.deepEqual(github.operations.slice(0, 4), [
    "check:create",
    "check:create",
    "auto-merge:disable",
    "check:update",
  ]);
  assert.equal(
    github.operations.indexOf("auto-merge:disable") + 1,
    github.operations.lastIndexOf("check:update"),
    "no other write may separate the disarm from the aggregate green",
  );
});

test("the auto-merge state is read fresh rather than trusted from the snapshot", async () => {
  const github = fakeGateGithub({ author: "detail-app", nativeAutoMergeEnabled: false });

  await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  // One read for a PR that is not armed: nothing to disable, nothing to confirm.
  assert.equal(github.autoMergeStateReads, 1);
  assert.equal(github.nativeAutoMergeDisableAttempts, 0);
});

test("a disable that leaves auto-merge armed refuses to publish the green", async () => {
  // The mutation succeeding is a claim; the read is the evidence. Without the
  // confirming read a disable that silently does nothing publishes a green on a
  // PR GitHub is still free to merge.
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: true,
    nativeAutoMergeStaysArmed: true,
  });

  await assert.rejects(
    autoGate.processAggregateHead({
      github,
      context: fakeContext(),
      core: fakeCore(),
      headSha: HEAD_SHA,
      targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
      mergeEnabled: true,
    }),
    /GitHub-native auto-merge is still armed after disabling it/,
  );

  assert.equal(github.nativeAutoMergeDisableAttempts, 1);
  // The aggregate stays on its WAITING failure; no consumable green exists.
  assert.equal(github.createdChecks[0].conclusion, "failure");
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "no passing aggregate may be published while auto-merge is still armed",
  );
});

test("an unreadable auto-merge state leaves the manual-only aggregate red", async () => {
  // Fail closed. An unreadable state is not a disarmed one, and the decision
  // this transaction is about to publish is a PASS.
  const error = new Error("auto-merge state unavailable");
  error.status = 500;
  const github = fakeGateGithub({ author: "detail-app", autoMergeStateError: error });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  // Retried, then reported as a read failure: a clean BLOCKED aggregate rather
  // than an unhandled error, and no passing decision either way.
  assert.equal(transaction.state, "evaluation-error");
  assert.equal(github.autoMergeStateReads, 3);
  assert.equal(github.nativeAutoMergeDisableAttempts, 0);
  assert.equal(github.updatedChecks.at(-1).conclusion, "failure");
  assert.match(
    github.updatedChecks.at(-1).output.summary,
    /could not read auto-merge state for PR #1465 after 3 attempts/,
  );
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "no passing aggregate may be published when the auto-merge state is unknown",
  );
});

test("an auto-mergeable PR never reaches the auto-merge disable path", async () => {
  // The precondition belongs to the manual-merge path only. A PR the gate will
  // merge itself is not one GitHub must be stopped from merging.
  const github = fakeGateGithub({ nativeAutoMergeEnabled: true });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "merged");
  assert.equal(github.autoMergeStateReads, 0);
  assert.equal(github.nativeAutoMergeDisableAttempts, 0);
});

test("a PR cannot read another PR's decision when both share one head", async () => {
  const github = fakeGateGithub({
    associatedPullRequests: [
      { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
    ],
    pullRequestsByNumber: {
      1465: { headRefOid: HEAD_SHA },
      2048: { headRefOid: HEAD_SHA },
    },
    checkRuns: [
      ...happyCheckRuns(),
      {
        ...checkRun({
          id: 321,
          name: decisionName(1465, HEAD_SHA),
          externalId: decisionExternalId(1465, HEAD_SHA),
          conclusion: "success",
        }),
        output: { title: "PASS: PR #1465 requirements are satisfied" },
      },
    ],
  });

  const targets = await autoGate.resolveTargets({
    github,
    context: fakeContext({ sha: HEAD_SHA }),
    core: fakeCore(),
  });
  assert.deepEqual(targets, [
    { prNumber: 1465, headSha: HEAD_SHA, decisionKey: decisionKey(1465, HEAD_SHA) },
    { prNumber: 2048, headSha: HEAD_SHA, decisionKey: decisionKey(2048, HEAD_SHA) },
  ]);

  await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result: {
      prNumber: "2048",
      headSha: HEAD_SHA,
      baseRefName: "master",
      isOpen: true,
      shouldMerge: false,
      summary: "BLOCKED: 1 unresolved live Codex inline finding",
    },
  });

  assert.equal(github.updatedChecks.length, 0);
  assert.equal(github.createdChecks.length, 1);
  assert.equal(
    github.createdChecks[0].name,
    decisionName(2048, HEAD_SHA),
  );
  assert.equal(github.createdChecks[0].external_id, decisionExternalId(2048, HEAD_SHA));
});

test("a single-PR aggregate reports only that PR's unmet requirements", async () => {
  const github = fakeGateGithub({
    checkRuns: [
      ...happyCheckRuns(),
      {
        ...checkRun({
          id: 321,
          name: decisionName(1465, HEAD_SHA),
          externalId: decisionExternalId(1465, HEAD_SHA),
          conclusion: "failure",
        }),
        output: { summary: "WAITING: required check Build is missing" },
      },
    ],
  });

  const aggregate = await autoGate.reportAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });

  assert.equal(aggregate.ok, false);
  assert.equal(
    github.createdChecks[0].output.summary,
    "Waiting on:\n- PR #1465 at this commit is waiting: required check Build is missing",
  );
  assert.doesNotMatch(github.createdChecks[0].output.summary, /currently belongs|shared by/);
  assert.doesNotMatch(github.createdChecks[0].output.summary, /To decouple/);
});

test("the fixed aggregate names a blocked PR and recovery when the head is shared", async () => {
  const github = fakeGateGithub({
    associatedPullRequests: [
      { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
    ],
    checkRuns: [
      ...happyCheckRuns(),
      {
        ...checkRun({
          id: 321,
          name: decisionName(1465, HEAD_SHA),
          externalId: decisionExternalId(1465, HEAD_SHA),
          conclusion: "success",
        }),
        output: { summary: "PASS: PR #1465 requirements are satisfied" },
      },
      {
        ...checkRun({
          id: 654,
          name: decisionName(2048, HEAD_SHA),
          externalId: decisionExternalId(2048, HEAD_SHA),
          conclusion: "failure",
        }),
        output: { summary: "BLOCKED: 1 unresolved live Codex inline finding" },
      },
    ],
  });

  assert.equal(
    typeof autoGate.reportAggregateDecision,
    "function",
    "master has no fixed-name aggregate enforcement for shared heads",
  );
  const aggregate = await autoGate.reportAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });

  assert.equal(aggregate.ok, false);
  assert.equal(github.createdChecks.length, 1);
  assert.equal(github.createdChecks[0].name, "Auto Gate decision");
  assert.equal(github.createdChecks[0].external_id, aggregateExternalId(HEAD_SHA));
  assert.equal(github.createdChecks[0].conclusion, "failure");
  assert.match(github.createdChecks[0].output.summary, /shared by open master PRs #1465 and #2048/);
  assert.match(
    github.createdChecks[0].output.summary,
    /PR #2048.*1 unresolved live Codex inline finding/,
  );
  assert.match(github.createdChecks[0].output.summary, /To decouple without merging another PR/);
});

test("the fixed aggregate passes only after every shared-head decision passes", async () => {
  const github = fakeGateGithub({
    associatedPullRequests: [
      { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
    ],
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        id: 321,
        name: decisionName(1465, HEAD_SHA),
        externalId: decisionExternalId(1465, HEAD_SHA),
        conclusion: "success",
      }),
      checkRun({
        id: 654,
        name: decisionName(2048, HEAD_SHA),
        externalId: decisionExternalId(2048, HEAD_SHA),
        conclusion: "success",
      }),
    ],
  });

  const aggregate = await autoGate.reportAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });

  assert.equal(aggregate.ok, true);
  assert.equal(github.createdChecks[0].conclusion, "success");
  assert.match(github.createdChecks[0].output.summary, /Every associated decision passes/);
});

test("the fixed aggregate cannot pre-authorize a commit before its PR exists", async () => {
  const github = fakeGateGithub({ associatedPullRequests: [] });

  const aggregate = await autoGate.reportAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });

  assert.equal(aggregate.ok, false);
  assert.equal(github.createdChecks[0].conclusion, "failure");
  assert.match(github.createdChecks[0].output.summary, /No open master PR currently points/);
  assert.match(github.createdChecks[0].output.summary, /No open pull request to master.*owns this commit/);
});

test("a missing shared-head decision names the PR and manual recovery", async () => {
  const github = fakeGateGithub({
    associatedPullRequests: [
      { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
    ],
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        name: decisionName(1465, HEAD_SHA),
        externalId: decisionExternalId(1465, HEAD_SHA),
        conclusion: "success",
      }),
    ],
  });

  const aggregate = await autoGate.reportAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });

  assert.equal(aggregate.ok, false);
  assert.match(github.createdChecks[0].output.summary, /PR #2048.*has no exact PR\/head/);
  assert.match(github.createdChecks[0].output.summary, /run Auto Gate manually for PR #2048/);
});

test("one serialized head transaction refreshes every shared PR before publishing", async () => {
  const github = fakeGateGithub({
    associatedPullRequests: [
      { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
    ],
    pullRequestsByNumber: {
      2048: { reviewComments: [codexFinding({ id: 20, line: 18 })] },
    },
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
  });

  assert.equal(transaction.state, "waiting");
  assert.deepEqual(
    github.createdChecks.map((check) => check.name),
    ["Auto Gate decision", decisionName(1465, HEAD_SHA), decisionName(2048, HEAD_SHA)],
  );
  assert.equal(github.updatedChecks.length, 1);
  assert.match(github.updatedChecks[0].output.summary, /PR #2048.*1 unresolved live Codex/);
  assert.ok(github.reviewCommentReadsByNumber[2048] > 0);
  assert.equal(github.mergedWith, null);
});

test("one shared-head transaction merges at most one PR before master changes", async () => {
  const github = fakeGateGithub({
    associatedPullRequests: [
      { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
    ],
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [
      { prNumber: 1465, headSha: HEAD_SHA },
      { prNumber: 2048, headSha: HEAD_SHA },
    ],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "merged");
  assert.equal(transaction.mergedPrNumber, 1465);
  assert.equal(github.mergedWith.pull_number, 1465);
  assert.equal(transaction.invalidated.state, "pending");
  assert.equal(github.createdChecks.at(-1).conclusion, "failure");
  assert.match(github.createdChecks.at(-1).output.title, /WAITING: refreshing/);
});

test("a merge API error makes the published aggregate non-green before propagating", async () => {
  const error = new Error("merge API unavailable");
  error.status = 500;
  const github = fakeGateGithub({ mergeError: error });

  await assert.rejects(
    autoGate.processAggregateHead({
      github,
      context: fakeContext(),
      core: fakeCore(),
      headSha: HEAD_SHA,
      targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
      mergeEnabled: true,
    }),
    /merge API unavailable/,
  );

  assert.equal(github.mergedWith, null);
  assert.equal(github.mergeAttempts, 1);
  assert.equal(github.createdChecks.at(-1).conclusion, "failure");
  assert.match(github.createdChecks.at(-1).output.title, /WAITING: refreshing/);
});

test("a stale aggregate PASS is made non-green before decisions refresh", async () => {
  const github = fakeGateGithub({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        id: 321,
        name: decisionName(1465, HEAD_SHA),
        externalId: decisionExternalId(1465, HEAD_SHA),
        conclusion: "success",
      }),
      checkRun({
        id: 777,
        name: "Auto Gate decision",
        externalId: aggregateExternalId(HEAD_SHA),
        conclusion: "success",
      }),
    ],
  });

  await autoGate.beginAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });

  assert.equal(github.createdChecks.length, 1);
  assert.equal(github.updatedChecks.length, 0);
  assert.equal(github.createdChecks[0].status, "completed");
  assert.equal(github.createdChecks[0].conclusion, "failure");
  assert.match(github.createdChecks[0].output.summary, /refreshing every open master PR/);
});

test("an ambiguous aggregate create is reconciled without replaying the write", async () => {
  const error = new Error("fetch failed");
  error.status = 500;
  const github = fakeGateGithub({ checkCreateAcceptedErrors: [error] });

  const invalidated = await autoGate.invalidateAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });

  assert.equal(invalidated.writeState, "created");
  assert.equal(github.checkCreateAttempts, 1);
  assert.equal(github.checkListReads, 1);
  assert.equal(github.createdChecks.length, 1);
  assert.equal(github.createdChecks[0].conclusion, "failure");
});

test("a definitively rate-limited aggregate create retries the rejected write", async () => {
  const error = new Error("API rate limit exceeded");
  error.status = 403;
  error.response = {
    headers: { "retry-after": "0", "x-ratelimit-remaining": "0" },
  };
  const github = fakeGateGithub({ checkCreateErrors: [error, error] });

  const invalidated = await autoGate.invalidateAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });

  assert.equal(invalidated.writeState, "created");
  assert.equal(github.checkCreateAttempts, 3);
  assert.equal(github.checkListReads, 0);
  assert.equal(github.createdChecks.length, 1);
});

test("idempotent check-run updates retry transient failures", async () => {
  const error = new Error("fetch failed");
  error.status = 500;
  const github = fakeGateGithub({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        id: 321,
        name: decisionName(1465, HEAD_SHA),
        externalId: decisionExternalId(1465, HEAD_SHA),
        conclusion: "failure",
      }),
    ],
    checkUpdateErrors: [error, error],
  });

  await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result: {
      prNumber: 1465,
      headSha: HEAD_SHA,
      baseRefName: "master",
      isOpen: true,
      shouldMerge: false,
      summary: "BLOCKED: waiting",
    },
  });

  assert.equal(github.checkUpdateAttempts, 3);
  assert.equal(github.updatedChecks.length, 1);
  assert.equal(github.updatedChecks[0].check_run_id, 321);
});

test("rate-limited check-run updates retry but ordinary forbidden writes do not", async () => {
  const rateLimitError = new Error("API rate limit exceeded");
  rateLimitError.status = 403;
  rateLimitError.response = {
    headers: { "retry-after": "0", "x-ratelimit-remaining": "0" },
  };
  const existingDecision = checkRun({
    id: 321,
    name: decisionName(1465, HEAD_SHA),
    externalId: decisionExternalId(1465, HEAD_SHA),
    conclusion: "failure",
  });
  const rateLimitedGithub = fakeGateGithub({
    checkRuns: [...happyCheckRuns(), existingDecision],
    checkUpdateErrors: [rateLimitError],
  });

  await autoGate.reportDecision({
    github: rateLimitedGithub,
    context: fakeContext(),
    core: fakeCore(),
    result: {
      prNumber: 1465,
      headSha: HEAD_SHA,
      baseRefName: "master",
      isOpen: true,
      shouldMerge: false,
      summary: "BLOCKED: waiting",
    },
  });

  assert.equal(rateLimitedGithub.checkUpdateAttempts, 2);

  const forbiddenError = new Error("Resource not accessible by integration");
  forbiddenError.status = 403;
  const forbiddenGithub = fakeGateGithub({
    checkRuns: [...happyCheckRuns(), existingDecision],
    checkUpdateErrors: [forbiddenError],
  });

  await assert.rejects(
    autoGate.reportDecision({
      github: forbiddenGithub,
      context: fakeContext(),
      core: fakeCore(),
      result: {
        prNumber: 1465,
        headSha: HEAD_SHA,
        baseRefName: "master",
        isOpen: true,
        shouldMerge: false,
        summary: "BLOCKED: waiting",
      },
    }),
    /Resource not accessible by integration/,
  );
  assert.equal(forbiddenGithub.checkUpdateAttempts, 1);
});

test("exhausted aggregate invalidation prevents the transaction from merging", async () => {
  const error = new Error("fetch failed");
  error.status = 500;
  const github = fakeGateGithub({ checkWriteError: error });

  await assert.rejects(
    autoGate.processAggregateHead({
      github,
      context: fakeContext(),
      core: fakeCore(),
      headSha: HEAD_SHA,
      targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
      mergeEnabled: true,
    }),
    /could not invalidate aggregate .* after ambiguous create failure \(fetch failed\) after 3 attempts/,
  );

  assert.equal(github.checkCreateAttempts, 1);
  assert.equal(github.checkListReads, 3);
  assert.equal(github.mergedWith, null);
  assert.equal(github.operations.includes("merge"), false);
});

test("a transient association read is retried without becoming an empty PR set", async () => {
  const error = new Error("fetch failed");
  error.status = 500;
  const github = fakeGateGithub({ associationError: error });

  const aggregate = await autoGate.evaluateAggregateDecision({
    github,
    context: fakeContext(),
    headSha: HEAD_SHA,
  });

  assert.equal(github.associationReads, 2);
  assert.deepEqual(aggregate.pullNumbers, [1465]);
  assert.doesNotMatch(aggregate.blockers.join("\n"), /No open pull request/);
});

test("a persistent association read failure ends with an explicit blocked aggregate", async () => {
  const error = new Error("fetch failed");
  error.status = 500;
  const github = fakeGateGithub({
    associationError: error,
    associationErrorEveryRead: true,
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        id: 777,
        name: "Auto Gate decision",
        externalId: aggregateExternalId(HEAD_SHA),
        conclusion: "success",
      }),
    ],
  });
  const notices = [];
  const core = { ...fakeCore(), notice: (message) => notices.push(message) };

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core,
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "evaluation-error");
  assert.equal(github.associationReads, 3);
  assert.equal(github.createdChecks.length, 1);
  assert.equal(github.createdChecks[0].name, "Auto Gate decision");
  assert.equal(github.createdChecks[0].conclusion, "failure");
  assert.equal(github.updatedChecks.length, 1);
  assert.equal(github.updatedChecks[0].conclusion, "failure");
  assert.match(
    github.updatedChecks[0].output.summary,
    /could not enumerate PRs at commit .* after 3 attempts: fetch failed/,
  );
  assert.match(notices.join("\n"), /BLOCKED:.*could not enumerate PRs/i);
  assert.equal(github.mergedWith, null);
  assert.equal(
    github.createdChecks.some((check) => check.name === decisionName(1465, HEAD_SHA)),
    false,
  );
});

test("a persistent PR query failure keeps the aggregate blocked without rethrowing", async () => {
  const error = new Error("fetch failed");
  error.status = 500;
  const github = fakeGateGithub({ graphqlErrorsByNumber: { 1465: error } });
  const warnings = [];
  const core = { ...fakeCore(), warning: (message) => warnings.push(message) };

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core,
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "evaluation-error");
  assert.equal(github.graphqlReadsByNumber[1465], 3);
  assert.equal(github.createdChecks.length, 1);
  assert.equal(github.updatedChecks.length, 1);
  assert.equal(github.updatedChecks[0].conclusion, "failure");
  assert.match(
    github.updatedChecks[0].output.summary,
    /could not read PR #1465 after 3 attempts: fetch failed/,
  );
  assert.match(warnings.join("\n"), /could not read PR #1465/);
  assert.doesNotMatch(warnings.join("\n"), /\n\s+at /);
  assert.equal(github.mergedWith, null);
});

test("a resolver read failure is terminal instead of being reevaluated downstream", async () => {
  const github = fakeGateGithub();

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
    readFailureReason: "could not read PR #1465 after 3 attempts: fetch failed",
  });

  assert.equal(transaction.state, "evaluation-error");
  assert.equal(github.associationReads, 0);
  assert.equal(github.createdChecks.length, 1);
  assert.equal(github.updatedChecks.length, 1);
  assert.equal(github.updatedChecks[0].conclusion, "failure");
  assert.match(github.updatedChecks[0].output.summary, /could not read PR #1465/);
  assert.equal(github.mergedWith, null);
});

test("an older transaction cannot overwrite a newer invalidation generation", async () => {
  const github = fakeGateGithub({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        name: decisionName(1465, HEAD_SHA),
        externalId: decisionExternalId(1465, HEAD_SHA),
        conclusion: "success",
      }),
    ],
  });

  const older = await autoGate.beginAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });
  const newer = await autoGate.invalidateAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
  });
  const report = await autoGate.reportAggregateDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    checkRunId: older.checkRunId,
  });

  assert.equal(report.state, "superseded");
  assert.equal(github.updatedChecks.length, 0);
  assert.equal(newer.checkRunId, 10001);
  assert.equal(github.createdChecks.at(-1).conclusion, "failure");
});

test("aggregate resolution invalidates the current head before the previous synchronization head", () => {
  const heads = autoGate.resolveAggregateHeads({
    context: fakeContext({
      before: HEAD_SHA,
      after: OTHER_SHA,
      pull_request: { head: { sha: OTHER_SHA } },
    }),
    targets: [{ prNumber: 1465, headSha: OTHER_SHA }],
  });

  assert.deepEqual(heads, [OTHER_SHA, HEAD_SHA]);
});

test("the queued legacy evaluator keeps resolved PR numbers numeric", async () => {
  const github = fakeGateGithub();
  const graphql = github.graphql;
  github.graphql = async (query, variables) => {
    assert.equal(typeof variables.number, "number");
    return graphql(query, variables);
  };

  const result = await autoGate.evaluate({
    github,
    context: fakeContext({ sha: HEAD_SHA }),
    core: fakeCore(),
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, true, result.reasons.join("\n"));
});

test("a queued evaluation cannot overwrite PASS after the PR merges", async () => {
  const github = fakeGateGithub({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({ id: 321, name: "Auto Gate decision", conclusion: "success" }),
    ],
  });

  const report = await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result: {
      prNumber: "1465",
      headSha: HEAD_SHA,
      isOpen: false,
      shouldMerge: false,
      summary: "BLOCKED: PR is already merged, not open",
    },
  });

  assert.equal(report.state, "closed");
  assert.equal(github.createdChecks.length, 0);
  assert.equal(github.updatedChecks.length, 0);
});

test("a non-master PR event cannot overwrite a master decision on the same commit", async () => {
  const github = fakeGateGithub({ checkRuns: happyCheckRuns() });

  const report = await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result: {
      prNumber: "1465",
      headSha: HEAD_SHA,
      baseRefName: "release",
      isOpen: true,
      shouldMerge: false,
      summary: "BLOCKED: base branch is release, not master",
    },
  });

  assert.equal(report.state, "ineligible");
  assert.equal(github.createdChecks.length, 0);
  assert.equal(github.updatedChecks.length, 0);
});

test("required check matching respects the required source app", () => {
  const spec = { context: "Build", sourceAppId: ACTIONS_APP_ID };
  const checkRuns = [
    checkRun({ name: "Build", conclusion: "success", appId: 999, appSlug: "spoof" }),
    checkRun({ name: "Build", status: "in_progress", conclusion: null }),
  ];

  const state = __test.latestRequiredState(spec, checkRuns, []);

  assert.equal(state.ok, false);
  assert.equal(state.waiting, true);
  assert.match(state.description, /github-actions \(15368\)/);
});

test("a cancelled required check blocks the gate", async () => {
  const result = await evaluateGate({
    checkRuns: [
      checkRun({ name: "Lint", conclusion: "cancelled" }),
      checkRun({ name: "Build", conclusion: "success" }),
    ],
  });

  assert.equal(result.shouldMerge, false);
  assert.match(result.reasons.join("\n"), /required check Lint.*cancelled/);
});

test("an absent required check blocks the gate", async () => {
  const result = await evaluateGate({
    checkRuns: [checkRun({ name: "Build", conclusion: "success" })],
  });

  assert.equal(result.shouldMerge, false);
  assert.match(result.reasons.join("\n"), /required check Lint.*missing/);
});

test("the synthetic decision check is not its own prerequisite", async () => {
  const result = await evaluateGate({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({ name: decisionName(1465, HEAD_SHA), conclusion: "failure" }),
    ],
    requiredChecks: [
      { context: "Lint", integration_id: ACTIONS_APP_ID },
      { context: "Build", integration_id: ACTIONS_APP_ID },
      { context: decisionName(1465, HEAD_SHA), integration_id: ACTIONS_APP_ID },
    ],
  });

  assert.equal(result.shouldMerge, true, result.reasons.join("\n"));
});

test("a same-named decision check from another app remains required", async () => {
  const result = await evaluateGate({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({ name: "Auto Gate decision", conclusion: "failure" }),
    ],
    requiredChecks: [
      { context: "Lint", integration_id: ACTIONS_APP_ID },
      { context: "Build", integration_id: ACTIONS_APP_ID },
      { context: "Auto Gate decision", integration_id: 999 },
    ],
  });

  assert.equal(result.shouldMerge, false);
  assert.match(
    result.reasons.join("\n"),
    /required check Auto Gate decision \(app 999\).*missing/,
  );
});

test("a source-less synthetic decision is not its own prerequisite", async () => {
  const result = await evaluateGate({
    requiredChecks: [
      { context: "Lint", integration_id: ACTIONS_APP_ID },
      { context: "Build", integration_id: ACTIONS_APP_ID },
      { context: "Auto Gate decision" },
    ],
  });

  assert.equal(result.shouldMerge, true, result.reasons.join("\n"));
});

test("a source-less legacy duplicate does not deadlock an app-bound decision requirement", async () => {
  const result = await evaluateGate({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({ name: "Auto Gate decision", conclusion: "failure" }),
    ],
    requiredChecks: [
      { context: "Lint", integration_id: ACTIONS_APP_ID },
      { context: "Build", integration_id: ACTIONS_APP_ID },
      { context: "Auto Gate decision" },
      { context: "Auto Gate decision", integration_id: ACTIONS_APP_ID },
    ],
  });

  assert.equal(result.shouldMerge, true, result.reasons.join("\n"));
});

test("terminal non-success conclusions never verify a required check", () => {
  const spec = { context: "Lint", sourceAppId: ACTIONS_APP_ID };

  for (const conclusion of ["cancelled", "timed_out", "neutral", "action_required"]) {
    const state = __test.latestRequiredState(
      spec,
      [checkRun({ name: "Lint", conclusion })],
      [],
    );
    assert.equal(state.ok, false, `${conclusion} must not verify the check`);
  }
});

test("only the known conditional Deploy check may be skipped", async () => {
  const allowed = await evaluateGate({
    requiredChecks: [
      { context: "Lint", integration_id: ACTIONS_APP_ID },
      { context: "Build", integration_id: ACTIONS_APP_ID },
      { context: "Deploy", integration_id: ACTIONS_APP_ID },
    ],
  });
  assert.equal(allowed.shouldMerge, true);

  const blocked = await evaluateGate({
    checkRuns: [
      ...requiredSuccessRuns(),
      checkRun({ name: "Deploy", conclusion: "skipped" }),
      checkRun({ name: "Security scan", conclusion: "skipped" }),
    ],
    requiredChecks: [
      { context: "Lint", integration_id: ACTIONS_APP_ID },
      { context: "Build", integration_id: ACTIONS_APP_ID },
      { context: "Security scan", integration_id: ACTIONS_APP_ID },
    ],
  });
  assert.equal(blocked.shouldMerge, false);
  assert.match(blocked.reasons.join("\n"), /required check Security scan.*skipped/);
});

test("a failing optional Web selftest does not become a merge requirement", async () => {
  const result = await evaluateGate({
    checkRuns: [...happyCheckRuns(), checkRun({ name: "Web selftest", conclusion: "failure" })],
  });

  assert.equal(result.shouldMerge, true);
});

test("transient CodeQL neutral waits for Analyze jobs and later passes", async () => {
  const settling = await evaluateGate({
    checkRuns: [
      ...requiredSuccessRuns(),
      checkRun({
        name: "CodeQL",
        conclusion: "neutral",
        appId: 57789,
        appSlug: "github-advanced-security",
      }),
      checkRun({ name: "Analyze (go)", status: "in_progress", conclusion: null }),
      checkRun({ name: "Deploy", conclusion: "skipped" }),
    ],
    requiredChecks: [
      { context: "Lint", integration_id: ACTIONS_APP_ID },
      { context: "Build", integration_id: ACTIONS_APP_ID },
      { context: "CodeQL", integration_id: 57789 },
    ],
  });

  assert.equal(settling.shouldMerge, false);
  assert.match(settling.reasons.join("\n"), /required check CodeQL.*still settling.*neutral/);

  const settled = await evaluateGate({
    checkRuns: [
      ...requiredSuccessRuns(),
      checkRun({
        name: "CodeQL",
        conclusion: "success",
        appId: 57789,
        appSlug: "github-advanced-security",
      }),
      checkRun({ name: "Analyze (go)", conclusion: "success" }),
      checkRun({ name: "Deploy", conclusion: "skipped" }),
    ],
    requiredChecks: [
      { context: "Lint", integration_id: ACTIONS_APP_ID },
      { context: "Build", integration_id: ACTIONS_APP_ID },
      { context: "CodeQL", integration_id: 57789 },
    ],
  });

  assert.equal(settled.shouldMerge, true);
});

test("a Codex rate-limit message never becomes a verdict", async () => {
  const result = await evaluateGate({ issueComments: [codexRateLimit()] });

  assert.equal(result.shouldMerge, false);
  assert.match(result.reasons.join("\n"), /has not reviewed head.*usage-limited/);
});

// #3378: the reviewer account ran out of credits and every open PR became
// unmergeable, because a usage-limited response was only ever cosmetic suffix
// text on a reason that waits for a verdict that cannot arrive. Observed
// usage-limited degrades to the existing manual-merge path; unknown does not.
test("an observed usage-limited reviewer degrades to maintainer review instead of waiting forever", async () => {
  const result = await evaluateGate({ issueComments: [codexRateLimit()] });

  assert.equal(result.manualMergeRequired, true, "the gate must degrade, not wait");
  assert.equal(result.shouldMerge, false, "degrading must never auto-merge");
  assert.match(result.summary, /^PASS:/);
  assert.match(result.summary, /usage-limited/);
  assert.match(result.summary, /has NOT been reviewed/);

  const github = fakeGateGithub({ checkRuns: happyCheckRuns() });
  const report = await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result,
    manual: false,
  });

  assert.equal(report.state, "manual");
  assert.equal(github.createdChecks[0].conclusion, "success");
  assert.match(github.createdChecks[0].output.title, /usage-limited/);
});

test("reviewer silence with no usage-limit evidence keeps blocking exactly as before", async () => {
  const result = await evaluateGate({ issueComments: [] });

  assert.equal(result.manualMergeRequired, false, "silence is not evidence of a usage limit");
  assert.equal(result.shouldMerge, false);
  assert.match(result.summary, /^BLOCKED:/);
  assert.match(result.reasons.join("\n"), /has not reviewed head/);
  assert.doesNotMatch(result.reasons.join("\n"), /usage-limited/);

  const github = fakeGateGithub({ checkRuns: happyCheckRuns() });
  await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result,
    manual: false,
  });

  assert.equal(github.createdChecks[0].conclusion, "failure");
});

// A usage-limit message proves the reviewer was out of quota when it answered,
// which says nothing about a head pushed after it: the reviewer may be back in
// quota and simply not there yet, which is the silence case. Without this the
// degradation is also sticky — one usage-limit comment would put the PR in
// manual-merge mode for every later push, forever.
test("a usage-limit response older than the head is stale evidence and keeps blocking", async () => {
  const result = await evaluateGate({
    headCommittedDate: "2026-07-09T01:30:00Z",
    issueComments: [codexRateLimit("2026-07-09T01:20:00Z")],
  });

  assert.equal(result.manualMergeRequired, false, "evidence about an older head is not evidence");
  assert.equal(result.shouldMerge, false);
  assert.match(result.summary, /^BLOCKED:/);
  assert.match(result.reasons.join("\n"), /predates this head/);
});

test("an unknown head timestamp never degrades the gate", async () => {
  const result = await evaluateGate({
    headCommittedDate: null,
    issueComments: [codexRateLimit()],
  });

  assert.equal(result.manualMergeRequired, false, "an unknown order is not a proven one");
  assert.equal(result.shouldMerge, false);
  assert.match(result.summary, /^BLOCKED:/);
});

// Degrading waives exactly one requirement: the verdict that cannot arrive.
// Every other gate is independent of the reviewer, and manualMergeRequired
// makes the decision pass, so waiving them together would let "the reviewer is
// down" quietly green-light a PR with a known finding or a missing label.
test("a usage-limited reviewer does not waive an unrelated blocker", async () => {
  const result = await evaluateGate({
    issueComments: [codexRateLimit()],
    files: ["app/termpane.go"],
  });

  assert.equal(result.manualMergeRequired, false, "an unrelated gate must still block");
  assert.equal(result.shouldMerge, false);
  assert.match(result.summary, /^BLOCKED:/);
  assert.match(result.reasons.join("\n"), /missing the play-tested label/);
  assert.match(result.reasons.join("\n"), /usage-limited/);
});

test("a usage-limited reviewer does not waive unresolved inline findings", async () => {
  const result = await evaluateGate({
    issueComments: [codexRateLimit()],
    reviewComments: [codexFinding({ id: 10, line: 32 })],
  });

  assert.equal(result.manualMergeRequired, false, "a live finding is not the missing verdict");
  assert.equal(result.shouldMerge, false);
  assert.match(result.summary, /^BLOCKED:/);
  assert.match(result.reasons.join("\n"), /1 unresolved live Codex inline finding/);
});

// "Latest response" has to mean latest across comments AND reviews. A review
// posted after the quota message proves the reviewer answered again, so the
// quota message is no longer current — reading only issue comments misses it.
test("a Codex review newer than the quota message means it is no longer current", async () => {
  const result = await evaluateGate({
    issueComments: [codexRateLimit("2026-07-09T01:20:00Z")],
    reviews: [codexReview(OTHER_SHA, "Suggestions for an earlier head.", "2026-07-09T01:25:00Z")],
  });

  assert.equal(result.manualMergeRequired, false, "the reviewer answered after the quota message");
  assert.equal(result.shouldMerge, false);
  assert.match(result.summary, /^BLOCKED:/);
});

// The detector is an unanchored substring match, so a review that discusses the
// gate itself trips it. Such a body already fails parseReviewedCommit, so it is
// not a verdict either — the safe landing is "keep blocking", never "degrade".
test("a review body that merely quotes the usage-limit text is not quota evidence", async () => {
  const quoting = {
    ...codexVerdict(HEAD_SHA),
    body:
      "### Codex Review\n\nThe detector fires on “reached your Codex usage limits for code " +
      `reviews” anywhere in a body.\n\n**Reviewed commit:** \`${HEAD_SHA.slice(0, 10)}\``,
  };
  const result = await evaluateGate({ issueComments: [quoting] });

  assert.equal(result.manualMergeRequired, false, "a review body is not a quota response");
  assert.equal(result.shouldMerge, false);
  assert.match(result.summary, /^BLOCKED:/);
});

test("a usage-limited reviewer unblocks the aggregate without merging anything", async () => {
  const github = fakeGateGithub({ nativeAutoMergeEnabled: true, issueComments: [codexRateLimit()] });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "manual");
  assert.equal(transaction.aggregate.ok, true, "the aggregate must stop sitting red");
  assert.equal(github.mergedWith, null, "degrading must never merge");
  assert.deepEqual(github.disabledAutoMergePullRequestIds, ["PR_node_1465"]);
  const exactDecision = github.createdChecks.find(
    (check) => check.name === decisionName(1465, HEAD_SHA),
  );
  assert.equal(exactDecision.conclusion, "success");
  assert.match(exactDecision.output.summary, /has NOT been reviewed/);
});

test("a usage-limited reviewer does not excuse an exact-head verdict carrying a finding", async () => {
  const verdictWithFinding = {
    ...codexVerdict(HEAD_SHA),
    body: `Codex Review: P1 — unsafe write ordering.\n\n**Reviewed commit:** \`${HEAD_SHA.slice(0, 10)}\``,
  };
  const result = await evaluateGate({
    issueComments: [verdictWithFinding, codexRateLimit("2026-07-09T01:25:00Z")],
  });

  assert.equal(result.manualMergeRequired, false, "a real verdict exists; nothing to degrade");
  assert.equal(result.shouldMerge, false);
  assert.match(result.summary, /^BLOCKED:/);
  assert.match(result.reasons.join("\n"), /P0-P3 finding/);
});

test("a clean exact-head verdict cannot override live Codex findings", async () => {
  const result = await evaluateGate({
    issueComments: [codexVerdict(HEAD_SHA)],
    reviewComments: [codexFinding({ id: 10, line: 32 })],
  });

  assert.equal(result.shouldMerge, false);
  assert.match(result.reasons.join("\n"), /1 unresolved live Codex inline finding/);
});

test("a stale Codex finding with a null line does not block", async () => {
  const result = await evaluateGate({
    reviewComments: [codexFinding({ id: 10, line: null })],
  });

  assert.equal(result.shouldMerge, true);
});

test("an allowed author resolves a live finding only with an explicit marker", async () => {
  const finding = codexFinding({ id: 10, line: 32 });
  const discussionOnly = await evaluateGate({
    reviewComments: [finding, findingReply({ id: 11, inReplyToId: 10, body: "I understand the tradeoff." })],
  });
  assert.equal(discussionOnly.shouldMerge, false);

  const resolved = await evaluateGate({
    reviewComments: [
      finding,
      findingReply({ id: 12, inReplyToId: 10, body: "The documented tradeoff is ACCEPTED." }),
    ],
  });
  assert.equal(resolved.shouldMerge, true);
});

test("an exact-head Codex PR review proves review after its finding is resolved", async () => {
  const finding = codexFinding({ id: 10, line: 32 });
  const result = await evaluateGate({
    issueComments: [],
    reviews: [codexReview(HEAD_SHA)],
    reviewComments: [
      finding,
      findingReply({ id: 12, inReplyToId: 10, body: "The documented tradeoff is ACCEPTED." }),
    ],
  });

  assert.equal(result.shouldMerge, true);
});

test("an exact-head Codex review body finding overrides a clean verdict", async () => {
  const result = await evaluateGate({
    issueComments: [codexVerdict(HEAD_SHA, "2026-07-09T01:20:00Z")],
    reviews: [
      codexReview(
        HEAD_SHA,
        "P1: a finding present only in the review body",
        "2026-07-09T01:21:00Z",
      ),
    ],
    reviewComments: [],
  });

  assert.equal(result.shouldMerge, false);
  assert.match(result.reasons.join("\n"), /latest exact-head Codex review body.*P0-P3 finding/);
});

test("a newer clean exact-head verdict supersedes an older body-only finding", async () => {
  const result = await evaluateGate({
    issueComments: [codexVerdict(HEAD_SHA, "2026-07-09T01:21:00Z")],
    reviews: [
      codexReview(
        HEAD_SHA,
        "P1: an older body-only finding",
        "2026-07-09T01:20:00Z",
      ),
    ],
    reviewComments: [],
  });

  assert.equal(result.shouldMerge, true);
});

test("a verdict for an older head does not verify the current head", async () => {
  const result = await evaluateGate({ issueComments: [codexVerdict(OTHER_SHA)] });

  assert.equal(result.shouldMerge, false);
  assert.match(result.reasons.join("\n"), /Codex has not reviewed head/);
});

test("issue-comment events resolve their pull request number", async () => {
  const result = await autoGate.evaluate({
    github: fakeGateGithub(),
    context: fakeContext({ issue: { number: 1465, pull_request: {}, state: "open" } }),
    core: fakeCore(),
    setOutputs: false,
  });

  assert.equal(result.prNumber, "1465");
  assert.equal(result.shouldMerge, true);
});

test("PR Validation workflow completion resolves every PR at its head", async () => {
  const github = fakeGateGithub({
    associatedPullRequests: [
      { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
    ],
    pullRequestsByNumber: {
      1465: { headRefOid: HEAD_SHA },
      2048: { headRefOid: HEAD_SHA },
    },
  });

  const targets = await autoGate.resolveTargets({
    github,
    context: { ...fakeContext({ workflow_run: { head_sha: HEAD_SHA } }), eventName: "workflow_run" },
    core: fakeCore(),
  });

  assert.deepEqual(targets.map((target) => target.prNumber), [1465, 2048]);
});

test("the happy path squash-merges the exact evaluated head", async () => {
  const github = fakeGateGithub({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        name: decisionName(1465, HEAD_SHA),
        externalId: decisionExternalId(1465, HEAD_SHA),
        conclusion: "success",
      }),
    ],
  });

  await autoGate.merge({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
  });

  assert.equal(github.mergedWith.sha, HEAD_SHA);
  assert.equal(github.mergedWith.merge_method, "squash");
});

test("merge invalidates the old-head aggregate before dispatching docs", async () => {
  const github = fakeGateGithub({
    files: ["docs/auto-gate.md"],
    associationError: new Error("post-merge association lookup unavailable"),
    associationErrorAtRead: 3,
  });

  await autoGate.merge({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
  });

  assert.deepEqual(github.operations, ["merge", "check:create", "docs:dispatch"]);
  assert.equal(github.createdChecks[0].name, "Auto Gate decision");
  assert.equal(github.createdChecks[0].conclusion, "failure");
  assert.equal(github.associationReads, 2, "invalidation after merge must be write-only");
});

test("a post-merge invalidation error cannot suppress the docs dispatch attempt", async () => {
  const github = fakeGateGithub({
    files: ["docs/auto-gate.md"],
    checkWriteError: new Error("check write unavailable"),
  });

  await assert.rejects(
    autoGate.merge({
      github,
      context: fakeContext(),
      core: fakeCore(),
      prNumber: 1465,
    }),
    /check write unavailable/,
  );

  assert.equal(github.mergedWith.sha, HEAD_SHA);
  assert.ok(github.operations.includes("docs:dispatch"));
});

test("a workflow dispatch failure remains single-shot after merge", async () => {
  const error = new Error("docs dispatch unavailable");
  error.status = 500;
  const github = fakeGateGithub({
    files: ["docs/auto-gate.md"],
    workflowDispatchError: error,
  });

  await assert.rejects(
    autoGate.merge({
      github,
      context: fakeContext(),
      core: fakeCore(),
      prNumber: 1465,
    }),
    /docs dispatch unavailable/,
  );

  assert.equal(github.mergedWith.sha, HEAD_SHA);
  assert.equal(github.workflowDispatchAttempts, 1);
});

test("merge freshly refuses a shared head whose other PR is waiting", async () => {
  const github = fakeGateGithub({
    associatedPullRequests: [
      { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
    ],
    pullRequestsByNumber: {
      2048: {
        reviewComments: [
          codexFinding({ id: 20, line: 18 }),
          codexFinding({ id: 21, line: 24 }),
        ],
      },
    },
  });

  await assert.rejects(
    autoGate.merge({
      github,
      context: fakeContext(),
      core: fakeCore(),
      prNumber: 1465,
    }),
    /fixed aggregate no longer passes[\s\S]*PR #2048[\s\S]*2 unresolved live Codex inline finding/,
  );
  assert.ok(github.reviewCommentReadsByNumber[2048] > 0, "merge must reevaluate PR #2048 fresh");
  assert.equal(github.mergedWith, null);
});

test("merge refuses when exact-head PR associations change during its fresh read", async () => {
  const pullA = { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } };
  const pullB = { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } };
  const github = fakeGateGithub({
    associatedPullRequestSnapshots: [[pullA], [pullA, pullB]],
  });

  await assert.rejects(
    autoGate.merge({
      github,
      context: fakeContext(),
      core: fakeCore(),
      prNumber: 1465,
    }),
    /fixed aggregate no longer passes[\s\S]*associations changed during the fresh merge evaluation/,
  );
  assert.equal(github.associationReads, 2);
  assert.equal(github.mergedWith, null);
});

test("merge propagates evaluation infrastructure errors instead of treating them as waiting", async () => {
  const github = fakeGateGithub({
    graphqlErrorsByNumber: { 1465: new Error("GraphQL unavailable") },
  });

  await assert.rejects(
    autoGate.merge({
      github,
      context: fakeContext(),
      core: fakeCore(),
      prNumber: 1465,
    }),
    (error) => {
      assert.match(error.message, /Auto Gate merge evaluation failed.*GraphQL unavailable/);
      assert.doesNotMatch(error.message, /^Refusing to merge/);
      return true;
    },
  );
  assert.equal(github.mergedWith, null);
});

test("merge reevaluates fresh instead of trusting a stale PASS decision", async () => {
  const github = fakeGateGithub({
    checkRuns: [
      ...happyCheckRuns(),
      checkRun({
        name: decisionName(1465, HEAD_SHA),
        externalId: decisionExternalId(1465, HEAD_SHA),
        conclusion: "success",
      }),
    ],
    reviewComments: [codexFinding({ id: 10, line: 32 })],
  });

  await assert.rejects(
    autoGate.merge({
      github,
      context: fakeContext(),
      core: fakeCore(),
      prNumber: 1465,
    }),
    /gate no longer passes.*unresolved live Codex inline finding/,
  );

  assert.equal(github.reviewCommentReads, 1, "merge must read current finding state");
  assert.equal(github.mergedWith, null);
});

test("merge refuses a head that escaped its serialized decision lane", async () => {
  const github = fakeGateGithub();

  await assert.rejects(
    autoGate.merge({
      github,
      context: fakeContext(),
      core: fakeCore(),
      prNumber: 1465,
      expectedHeadSha: OTHER_SHA,
    }),
    /serialized head.*does not match evaluated head/,
  );
  assert.equal(github.mergedWith, null);
});

test("draft and closed pull requests cannot merge", async () => {
  const draft = await evaluateGate({ isDraft: true });
  assert.equal(draft.shouldMerge, false);
  assert.match(draft.reasons.join("\n"), /PR is a draft/);

  const closed = await evaluateGate({ state: "CLOSED" });
  assert.equal(closed.shouldMerge, false);
  assert.match(closed.reasons.join("\n"), /PR is closed, not open/);
});

test("fork heads from non-allowed authors pass for manual merge but cannot auto-merge", async () => {
  const github = fakeGateGithub({
    author: "outside-contributor",
    pullRequestsByNumber: { 1465: { headRepository: "outside/fork" } },
  });
  const result = await autoGate.evaluate({
    github,
    context: fakeContext({ pull_request: { head: { repo: { fork: true } } } }),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, false);
  assert.equal(result.manualMergeRequired, true);
  assert.match(result.reasons.join("\n"), /head repository outside\/fork.*base-repository branch/);
  assert.match(result.summary, /maintainer must review and merge manually/);
});

test("Codex verdict parsing requires a real verdict and matches its short SHA", () => {
  assert.equal(__test.parseReviewedCommit(codexRateLimit().body), null);
  assert.equal(__test.parseReviewedCommit(codexVerdict(HEAD_SHA).body), HEAD_SHA.slice(0, 10));
  assert.equal(
    __test.parseReviewedCommit(
      `Codex Review: clean\n\nReviewed commit: \`${HEAD_SHA.slice(0, 10)}\``,
    ),
    HEAD_SHA.slice(0, 10),
  );
  assert.equal(__test.reviewedCommitMatchesHead(HEAD_SHA.slice(0, 10), HEAD_SHA), true);
  assert.equal(__test.reviewedCommitMatchesHead(OTHER_SHA.slice(0, 10), HEAD_SHA), false);
});

test("RESOLVED does not clear a finding when no commit followed it", async () => {
  // The #2799 race: finding filed at 01:15 on a head committed at 01:00, author
  // replies RESOLVED, no push. The claimed fix cannot be in the head.
  const result = await evaluateGate({
    headCommittedDate: "2026-07-09T01:00:00Z",
    reviewComments: [
      codexFinding({ id: 10, line: 32, createdAt: "2026-07-09T01:15:00Z" }),
      findingReply({ id: 11, inReplyToId: 10, body: "RESOLVED — fixed it." }),
    ],
  });

  assert.equal(result.shouldMerge, false);
  assert.match(result.reasons.join("\n"), /RESOLVED.*no commit pushed after/);
});

test("RESOLVED clears a finding once a commit lands after it", async () => {
  const result = await evaluateGate({
    headCommittedDate: "2026-07-09T01:18:00Z",
    issueComments: [codexVerdict(HEAD_SHA, "2026-07-09T01:20:00Z")],
    reviewComments: [
      codexFinding({ id: 10, line: 32, createdAt: "2026-07-09T01:15:00Z" }),
      findingReply({ id: 11, inReplyToId: 10, body: "RESOLVED — fixed it." }),
    ],
  });

  assert.equal(result.shouldMerge, true, result.reasons.join("\n"));
});

test("ACCEPTED needs no commit — it claims no code change is owed", async () => {
  const result = await evaluateGate({
    headCommittedDate: "2026-07-09T01:00:00Z",
    reviewComments: [
      codexFinding({ id: 10, line: 32, createdAt: "2026-07-09T01:15:00Z" }),
      findingReply({ id: 11, inReplyToId: 10, body: "ACCEPTED — does not apply here." }),
    ],
  });

  assert.equal(result.shouldMerge, true, result.reasons.join("\n"));
});

test("gate-ack needs no commit either", async () => {
  const result = await evaluateGate({
    headCommittedDate: "2026-07-09T01:00:00Z",
    reviewComments: [
      codexFinding({ id: 10, line: 32, createdAt: "2026-07-09T01:15:00Z" }),
      findingReply({ id: 11, inReplyToId: 10, body: "Root accepts this [gate-ack]." }),
    ],
  });

  assert.equal(result.shouldMerge, true, result.reasons.join("\n"));
});

test("a later ACCEPTED exempts a finding an earlier RESOLVED claimed to fix", async () => {
  const result = await evaluateGate({
    headCommittedDate: "2026-07-09T01:00:00Z",
    reviewComments: [
      codexFinding({ id: 10, line: 32, createdAt: "2026-07-09T01:15:00Z" }),
      findingReply({ id: 11, inReplyToId: 10, body: "RESOLVED — fixed it." }),
      findingReply({ id: 12, inReplyToId: 10, body: "ACCEPTED — on reflection it does not apply." }),
    ],
  });

  assert.equal(result.shouldMerge, true, result.reasons.join("\n"));
});

test("gate-ack remains an explicit finding resolution marker", () => {
  assert.equal(__test.hasResolutionMarker("Root accepts this [gate-ack]."), true);
  assert.equal(__test.hasResolutionMarker("accepted in discussion, not marked"), false);
});

async function evaluateGate(options = {}) {
  return autoGate.evaluate({
    github: fakeGateGithub(options),
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });
}

// invalidateGateScript extracts the invalidate-gate step's inline script from
// the workflow and compiles it the way actions/github-script does: an async
// function receiving github/context/core/require, with process reachable in
// scope. Executing the real step body is the point — a text-level assertion on
// the YAML stays green when the control flow inverts (#3224).
function invalidateGateScript() {
  const workflow = fs.readFileSync(AUTO_GATE_WORKFLOW, "utf8");
  const step = workflow.match(
    /- name: Make the aggregate non-green immediately[\s\S]*?script: \|\n([\s\S]*?)(?=\n {0,10}\S)/,
  );
  assert.ok(step, "the aggregate invalidation step script is missing from auto-gate.yml");
  const indent = step[1].match(/^( +)\S/m);
  assert.ok(indent, "the aggregate invalidation step script is empty");
  const body = step[1]
    .split("\n")
    .map((line) => line.slice(indent[1].length))
    .join("\n");
  const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
  return new AsyncFunction("github", "context", "core", "require", "process", body);
}

// runInvalidateGateStep executes the extracted step against a scripted helper:
// invalidateResults maps each head SHA to the sequence of results its
// invalidateAggregateDecision calls return, and attempts records the real call
// order the step made.
async function runInvalidateGateStep({ aggregateHeads, invalidateResults }) {
  const script = invalidateGateScript();
  const attempts = [];
  const helper = {
    invalidateAggregateDecision: async ({ headSha }) => {
      attempts.push(headSha);
      const remaining = invalidateResults[headSha];
      assert.ok(
        remaining && remaining.length > 0,
        `unexpected extra invalidation attempt for ${headSha}`,
      );
      return remaining.shift();
    },
  };
  const workspace = "/workspace";
  const helperPath = path.join(workspace, ".github/scripts/auto-gate.js");
  const requireStub = (id) => {
    if (id === "path") {
      return path;
    }
    if (id === helperPath) {
      return helper;
    }
    throw new Error(`unexpected require(${JSON.stringify(id)}) in the invalidate-gate step`);
  };
  const outputs = {};
  const warnings = [];
  const core = {
    ...fakeCore(),
    setOutput: (name, value) => {
      outputs[name] = value;
    },
    warning: (message) => warnings.push(message),
  };
  const env = { GITHUB_WORKSPACE: workspace, AGGREGATE_HEADS: JSON.stringify(aggregateHeads) };
  let error = null;
  try {
    await script({}, {}, core, requireStub, { env });
  } catch (stepError) {
    error = stepError;
  }
  return { attempts, outputs, warnings, error };
}

function fakeGateGithub({
  headSha = HEAD_SHA,
  headCommittedDate = "2026-07-09T01:00:00Z",
  author = "sachiniyer",
  nativeAutoMergeEnabled = false,
  // Arms native auto-merge AFTER the PR read that used to snapshot it — the
  // #3381 window. The snapshot says off, the live state says on.
  nativeAutoMergeArmedAfterRead = false,
  // The disable mutation reports success and leaves auto-merge armed anyway.
  nativeAutoMergeStaysArmed = false,
  isDraft = false,
  state = "OPEN",
  merged = false,
  mergeable = "MERGEABLE",
  mergeStateStatus = "CLEAN",
  checkRuns = happyCheckRuns(),
  statuses = [],
  issueComments = [codexVerdict(headSha)],
  reviews = [],
  reviewComments = [],
  files = [],
  associatedPullRequests = [
    { number: 1465, state: "open", base: { ref: "master" }, head: { sha: headSha } },
  ],
  associatedPullRequestSnapshots = null,
  requiredChecks = [
    { context: "Lint", integration_id: ACTIONS_APP_ID },
    { context: "Build", integration_id: ACTIONS_APP_ID },
  ],
  pullRequestsByNumber = {},
  graphqlErrorsByNumber = {},
  mergeError = null,
  checkWriteError = null,
  checkCreateAcceptedErrors = [],
  checkCreateErrors = [],
  checkUpdateErrors = [],
  workflowDispatchError = null,
  nativeAutoMergeDisableError = null,
  autoMergeStateError = null,
  associationError = null,
  associationErrorAtRead = 1,
  associationErrorEveryRead = false,
} = {}) {
  const listFiles = function listFiles() {};
  const listForRef = function listForRef() {};
  const listCommitStatusesForRef = function listCommitStatusesForRef() {};
  const listComments = function listComments() {};
  const listReviews = function listReviews() {};
  const listReviewComments = function listReviewComments() {};
  const listPullRequestsAssociatedWithCommit = function listPullRequestsAssociatedWithCommit() {};
  const merge = async function merge(options) {
    github.mergeAttempts += 1;
    if (mergeError) {
      throw mergeError;
    }
    github.operations.push("merge");
    github.mergedWith = options;
    return { data: { sha: "merge-sha" } };
  };
  const createCheck = async function createCheck(options) {
    github.checkCreateAttempts += 1;
    const acceptedError = checkCreateAcceptedErrors[github.checkCreateAttempts - 1];
    if (acceptedError) {
      github.operations.push("check:create");
      github.createdChecks.push(options);
      throw acceptedError;
    }
    const attemptError = checkCreateErrors[github.checkCreateAttempts - 1] || checkWriteError;
    if (attemptError) {
      throw attemptError;
    }
    github.operations.push("check:create");
    github.createdChecks.push(options);
    return { data: { id: 10000 + github.createdChecks.length - 1, ...options } };
  };
  const updateCheck = async function updateCheck(options) {
    github.checkUpdateAttempts += 1;
    const attemptError = checkUpdateErrors[github.checkUpdateAttempts - 1] || checkWriteError;
    if (attemptError) {
      throw attemptError;
    }
    github.operations.push("check:update");
    github.updatedChecks.push(options);
    return { data: options };
  };
  const responses = new Map([
    [listFiles, files.map((filename) => ({ filename }))],
    [listForRef, checkRuns],
    [listCommitStatusesForRef, statuses],
    [listComments, issueComments],
    [listReviews, reviews],
    [listReviewComments, reviewComments],
    [listPullRequestsAssociatedWithCommit, associatedPullRequests],
  ]);

  const github = {
    associationReads: 0,
    checkCreateAttempts: 0,
    checkListReads: 0,
    checkUpdateAttempts: 0,
    disabledAutoMergePullRequestIds: [],
    mergeAttempts: 0,
    nativeAutoMergeDisableAttempts: 0,
    nativeAutoMergeArmed: nativeAutoMergeEnabled,
    autoMergeStateReads: 0,
    operations: [],
    mergedWith: null,
    reviewCommentReads: 0,
    reviewCommentReadsByNumber: {},
    createdChecks: [],
    graphqlReadsByNumber: {},
    updatedChecks: [],
    workflowDispatchAttempts: 0,
    rest: {
      actions: {
        createWorkflowDispatch: async () => {
          github.workflowDispatchAttempts += 1;
          if (workflowDispatchError) {
            throw workflowDispatchError;
          }
          github.operations.push("docs:dispatch");
        },
      },
      checks: { create: createCheck, listForRef, update: updateCheck },
      issues: { listComments },
      repos: { listCommitStatusesForRef, listPullRequestsAssociatedWithCommit },
      pulls: { listFiles, listReviews, listReviewComments, merge },
    },
    graphql: async (_query, variables) => {
      if (_query.includes("mutation DisablePullRequestAutoMerge")) {
        github.nativeAutoMergeDisableAttempts += 1;
        if (nativeAutoMergeDisableError) {
          throw nativeAutoMergeDisableError;
        }
        github.operations.push("auto-merge:disable");
        github.disabledAutoMergePullRequestIds.push(variables.pullRequestId);
        // A real disable takes effect, so the confirming read sees it.
        github.nativeAutoMergeArmed = nativeAutoMergeStaysArmed;
        return { disablePullRequestAutoMerge: { pullRequest: { number: 1465 } } };
      }
      if (_query.includes("query AutoMergeState")) {
        github.autoMergeStateReads += 1;
        if (autoMergeStateError) {
          throw autoMergeStateError;
        }
        return {
          repository: {
            pullRequest: {
              autoMergeRequest: github.nativeAutoMergeArmed
                ? { enabledAt: "2026-07-09T01:05:00Z" }
                : null,
            },
          },
        };
      }
      github.graphqlReadsByNumber[variables.number] =
        (github.graphqlReadsByNumber[variables.number] || 0) + 1;
      if (nativeAutoMergeArmedAfterRead) {
        github.nativeAutoMergeArmed = true;
      }
      if (graphqlErrorsByNumber[variables.number]) {
        throw graphqlErrorsByNumber[variables.number];
      }
      const pullRequestOverride = pullRequestsByNumber[variables.number] || {};
      return {
        repository: {
          pullRequest: {
            id: pullRequestOverride.id || "PR_node_1465",
            number: variables.number,
            title: "Gate test",
            url: "https://example.invalid/pr/1465",
            baseRefName: pullRequestOverride.baseRefName || "master",
            headRefOid: pullRequestOverride.headRefOid || headSha,
            headRepository: {
              nameWithOwner:
                pullRequestOverride.headRepository || "sachiniyer/agent-factory",
            },
            isDraft: pullRequestOverride.isDraft ?? isDraft,
            state: pullRequestOverride.state || state,
            merged: pullRequestOverride.merged ?? merged,
            mergeable: pullRequestOverride.mergeable || mergeable,
            mergeStateStatus: pullRequestOverride.mergeStateStatus || mergeStateStatus,
            author: { login: author },
            autoMergeRequest:
              (pullRequestOverride.nativeAutoMergeEnabled ?? nativeAutoMergeEnabled)
                ? { enabledAt: "2026-07-09T01:05:00Z" }
                : null,
            labels: { nodes: [] },
            commits: {
              nodes: [{ commit: { committedDate: headCommittedDate } }],
            },
          },
        },
      };
    },
    paginate: async (fn, options = {}) => {
      const number = options.pull_number || options.issue_number;
      const pullRequestOverride = pullRequestsByNumber[number] || {};
      if (fn === listForRef) {
        github.checkListReads += 1;
        return [
          ...checkRuns,
          ...github.createdChecks.map((created, index) => ({
            id: 10000 + index,
            app: { id: ACTIONS_APP_ID, slug: "github-actions" },
            created_at: "2026-07-09T01:11:00Z",
            started_at: "2026-07-09T01:11:00Z",
            completed_at: "2026-07-09T01:11:00Z",
            ...created,
          })),
        ];
      }
      if (fn === listPullRequestsAssociatedWithCommit) {
        github.associationReads += 1;
        if (
          associationError &&
          (associationErrorEveryRead || github.associationReads === associationErrorAtRead)
        ) {
          throw associationError;
        }
        if (associatedPullRequestSnapshots) {
          const index = Math.min(
            github.associationReads - 1,
            associatedPullRequestSnapshots.length - 1,
          );
          return associatedPullRequestSnapshots[index];
        }
        return associatedPullRequests;
      }
      if (fn === listReviewComments) {
        github.reviewCommentReads += 1;
        github.reviewCommentReadsByNumber[number] =
          (github.reviewCommentReadsByNumber[number] || 0) + 1;
        if (pullRequestOverride.reviewComments) {
          return pullRequestOverride.reviewComments;
        }
      }
      if (fn === listComments && pullRequestOverride.issueComments) {
        return pullRequestOverride.issueComments;
      }
      if (fn === listReviews && pullRequestOverride.reviews) {
        return pullRequestOverride.reviews;
      }
      if (fn === listFiles && pullRequestOverride.files) {
        return pullRequestOverride.files.map((filename) => ({ filename }));
      }
      return responses.get(fn) || [];
    },
    request: async (route) => {
      if (route.includes("/rules/branches/")) {
        return {
          data: [
            {
              type: "required_status_checks",
              parameters: { required_status_checks: requiredChecks },
            },
          ],
        };
      }
      const error = new Error("not protected");
      error.status = 404;
      throw error;
    },
  };

  return github;
}

function fakeContext(payload = {}) {
  return {
    repo: { owner: "sachiniyer", repo: "agent-factory" },
    payload,
    eventName: "pull_request",
  };
}

function fakeCore() {
  return {
    info: () => {},
    notice: () => {},
    setOutput: () => {},
    warning: () => {},
  };
}

function happyCheckRuns() {
  return [
    ...requiredSuccessRuns(),
    checkRun({
      name: "CodeQL",
      conclusion: "success",
      appId: 57789,
      appSlug: "github-advanced-security",
    }),
    checkRun({ name: "Analyze (go)", conclusion: "success" }),
    checkRun({ name: "Deploy", conclusion: "skipped" }),
    checkRun({ name: "Evaluate auto-merge gate", status: "in_progress", conclusion: null }),
  ];
}

function requiredSuccessRuns() {
  return [
    checkRun({ name: "Lint", conclusion: "success" }),
    checkRun({ name: "Build", conclusion: "success" }),
  ];
}

function checkRun({
  id = 100,
  name,
  status = "completed",
  conclusion,
  appId = ACTIONS_APP_ID,
  appSlug = "github-actions",
  externalId = null,
}) {
  return {
    id,
    name,
    external_id: externalId,
    app: { id: appId, slug: appSlug },
    status,
    conclusion,
    created_at: "2026-07-09T01:05:00Z",
    started_at: "2026-07-09T01:06:00Z",
    completed_at: status === "completed" ? "2026-07-09T01:10:00Z" : null,
  };
}

function decisionName(prNumber, headSha) {
  return `Auto Gate decision / PR #${prNumber} / ${headSha}`;
}

function decisionExternalId(prNumber, headSha) {
  return `auto-gate:pr:${prNumber}:head:${headSha}`;
}

function aggregateExternalId(headSha) {
  return `auto-gate:aggregate:head:${headSha}`;
}

function decisionKey(prNumber, headSha) {
  return `pr-${prNumber}-head-${headSha}`;
}

function codexVerdict(sha, timestamp = "2026-07-09T01:20:00Z") {
  return {
    user: { login: "chatgpt-codex-connector[bot]" },
    body: `Codex Review: Didn't find any major issues.\n\n**Reviewed commit:** \`${sha.slice(0, 10)}\``,
    created_at: timestamp,
    updated_at: timestamp,
  };
}

function codexRateLimit(timestamp = "2026-07-09T01:20:00Z") {
  return {
    user: { login: "chatgpt-codex-connector[bot]" },
    body: "You have reached your Codex usage limits for code reviews.",
    created_at: timestamp,
    updated_at: timestamp,
  };
}

function codexReview(sha, summary = "Here are some suggestions.", timestamp = "2026-07-09T01:20:00Z") {
  return {
    user: { login: "chatgpt-codex-connector[bot]" },
    body: `### Codex Review\n\n${summary}\n\n**Reviewed commit:** \`${sha.slice(0, 10)}\``,
    submitted_at: timestamp,
  };
}

function codexFinding({ id, line, createdAt = "2026-07-09T01:15:00Z" }) {
  return {
    id,
    user: { login: "chatgpt-codex-connector[bot]" },
    body: "P1: this needs attention",
    created_at: createdAt,
    line,
  };
}

function findingReply({ id, inReplyToId, body }) {
  return {
    id,
    in_reply_to_id: inReplyToId,
    user: { login: "sachiniyer" },
    body,
    created_at: "2026-07-09T01:16:00Z",
    line: 32,
  };
}
