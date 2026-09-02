const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const autoGate = require("./auto-gate.js");
const { __test } = autoGate;

const HEAD_SHA = "0a5393dd71ddbbf66486d31939728f9947c843bb";
const OTHER_SHA = "da0a05ea3b9036a12f67a3b3877d16dd0dac893d";
const ACTIONS_APP_ID = 15368;
const CHECK_CREATED_AT = "2026-07-09T01:11:00Z";
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

  // Two reads for a PR that is not armed: the disarm pass finds nothing to do,
  // and the publish precondition confirms it again immediately before the write.
  // Nothing is disabled either way.
  assert.equal(github.autoMergeStateReads, 2);
  assert.equal(github.nativeAutoMergeDisableAttempts, 0);
});

test("one batched read covers every manual PR sharing a head", async () => {
  // Codex P1 (round 2): with several manual PRs on one head, a per-PR loop makes
  // the first PR's observation N-1 reads stale by the time the green is
  // published. One aliased query keeps the observation simultaneous.
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: true,
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
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "manual");
  // The two shared-head reads are batched: one snapshot for the disarm pass and
  // one for the publish precondition, each covering both PRs in a single query.
  // The extra reads are the per-mutation revalidations, which are unavoidably
  // per-PR — a stale head there would cancel a queue entry armed for a head this
  // transaction never evaluated.
  // The shape of the reads, stated exactly. The first and last queries each
  // carry BOTH PRs — the disarm snapshot and the publish precondition — and the
  // singles between them are the per-mutation revalidations, which are
  // necessarily per-PR. Adding PRs widens the two snapshots rather than adding
  // round trips; unbatched they would be two queries per PR.
  assert.deepEqual(github.autoMergeStateBatchSizes, [2, 1, 1, 2]);
  assert.equal(github.autoMergeStateBatchSizes.at(0), 2, "the disarm snapshot is batched");
  assert.equal(github.autoMergeStateBatchSizes.at(-1), 2, "the publish precondition is batched");
});

test("a head that moves between the snapshot and its mutation is not disabled", async () => {
  // Codex P2 (round 3): the batched read is one snapshot for the whole head and
  // the disables that follow are sequential, so a PR can synchronize while an
  // earlier one is being disabled. The mutation takes a stable node ID, so it
  // would cancel a queue entry armed for a head this transaction never
  // evaluated.
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: true,
    // Reads 1..N report the evaluated head; the revalidation read sees the move.
    autoMergeStateHeadAfterRead: { 1465: { after: 1, headSha: OTHER_SHA } },
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "association-changed");
  assert.equal(github.nativeAutoMergeDisableAttempts, 0, "a moved head must not be mutated");
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "no passing aggregate may be published for a head the PR has left",
  );
});

test("a PR that moved to a new head has its auto-merge left alone", async () => {
  // Codex P2 (round 2): the state read must be bound to the head this
  // transaction owns. A PR that synchronized has an auto-merge request armed for
  // the NEW head, and cancelling it would destroy a queue entry this transaction
  // never evaluated and has no claim over.
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: true,
    autoMergeStateHeadByNumber: { 1465: OTHER_SHA },
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "association-changed");
  assert.equal(github.nativeAutoMergeDisableAttempts, 0, "a foreign head must not be mutated");
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "no passing aggregate may be published for a head the PR has left",
  );
});

test("auto-merge armed during a write retry blocks the retried green", async () => {
  // Codex P1 (round 3): upsertAggregateCheck retries a transient write through
  // retryCheckUpdate, which sleeps and reissues. A precondition checked once
  // before that helper is stale for every attempt after the first, so auto-merge
  // armed during the backoff was consumed by the green the retry published.
  const transient = new Error("check update unavailable");
  transient.status = 500;
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: false,
    checkUpdateErrors: [transient],
    armNativeAutoMergeOnCheckUpdateFailure: true,
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
    /GitHub-native auto-merge is still armed/,
  );

  assert.equal(github.nativeAutoMergeArmed, true);
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "the retried write must not publish a green over a freshly armed auto-merge",
  );
});

test("a newer generation taken during a write retry stops the PASS", async () => {
  // Codex P1 (round 4): the generation-ownership check runs once, before the
  // write. Aggregate invalidation runs OUTSIDE this head's serialized lane, so a
  // newer event can take ownership during the retry's backoff and the reissued
  // write would publish PASS over it.
  const transient = new Error("check update unavailable");
  transient.status = 500;
  const github = fakeGateGithub({
    author: "detail-app",
    checkUpdateErrors: [transient],
    newerAggregateOnCheckUpdateFailure: true,
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "association-changed");
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "a superseded transaction must not publish PASS on the retry",
  );
});

test("ownership is re-established alongside the guard, not before it", async () => {
  // Codex P1 (round 5): confirmNativeAutoMergeDisabled performs its own
  // retrying read, so an ownership check sequenced BEFORE it is stale by that
  // read's duration — including its backoff. An invalidation landing in that
  // window was invisible and the transaction still published PASS to a
  // superseded check run.
  //
  // Concurrency is the claim, so the test records start AND end of each read:
  // issued together, the second read starts before the first finishes. Sequenced,
  // it cannot.
  const order = [];
  const github = fakeGateGithub({ author: "detail-app" });
  const realPaginate = github.paginate;
  github.paginate = async (fn, options) => {
    const ownership = fn === github.rest.checks.listForRef;
    if (ownership) {
      order.push("ownership:start");
    }
    const result = await realPaginate(fn, options);
    // Yield twice, so a sequential implementation cannot appear interleaved.
    await new Promise((resolve) => setTimeout(resolve, 0));
    if (ownership) {
      order.push("ownership:end");
    }
    return result;
  };
  const realGraphql = github.graphql;
  github.graphql = async (query, variables) => {
    const autoMerge = query.includes("query AutoMergeState");
    if (autoMerge) {
      order.push("auto-merge:start");
    }
    const result = await realGraphql(query, variables);
    await new Promise((resolve) => setTimeout(resolve, 0));
    if (autoMerge) {
      order.push("auto-merge:end");
    }
    return result;
  };
  const realUpdate = github.rest.checks.update;
  github.rest.checks.update = async (options) => {
    if (options.output?.title?.startsWith("PASS: every open master PR")) {
      order.push("publish");
    }
    return realUpdate(options);
  };

  await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  const publish = order.lastIndexOf("publish");
  assert.ok(publish > 0, "the aggregate PASS must have been published");
  const guardWindow = order.slice(0, publish);
  const ownershipStart = guardWindow.lastIndexOf("ownership:start");
  const ownershipEnd = guardWindow.lastIndexOf("ownership:end");
  const autoMergeStart = guardWindow.lastIndexOf("auto-merge:start");
  const autoMergeEnd = guardWindow.lastIndexOf("auto-merge:end");
  assert.ok(ownershipStart >= 0 && autoMergeStart >= 0, "both preconditions must run");
  // Interleaved: each starts before the other ends. Neither is stale by the
  // duration of the other's read — including that read's retry backoff.
  assert.ok(
    autoMergeStart < ownershipEnd && ownershipStart < autoMergeEnd,
    `the preconditions must be issued together, not sequenced; got ${JSON.stringify(order.slice(-8))}`,
  );
  // …and both finish immediately before the write.
  assert.ok(
    Math.max(ownershipEnd, autoMergeEnd) === publish - 1,
    `nothing may read between the preconditions and the green; got ${JSON.stringify(order.slice(-8))}`,
  );
});

test("a transient precondition read discards the whole round", async () => {
  // Codex P1 (round 6): issuing the two preconditions concurrently is not the
  // same as observing them simultaneously. If one retries internally, the
  // other's answer is already stale by that backoff when Promise.all resolves —
  // long enough for a newer invalidation to land unseen.
  //
  // So the reads are single-shot and a transient failure discards the ROUND:
  // every answer that counts comes from the same round. The oracle is that the
  // ownership read is re-issued when only the auto-merge read failed.
  const github = fakeGateGithub({ author: "detail-app" });
  let autoMergeReads = 0;
  let ownershipReads = 0;
  const realGraphql = github.graphql;
  github.graphql = async (query, variables) => {
    if (query.includes("query AutoMergeState")) {
      autoMergeReads += 1;
      // Fail only the first guard round; the disarm snapshot is read 1.
      if (autoMergeReads === 2) {
        throw new Error("fetch failed");
      }
    }
    return realGraphql(query, variables);
  };
  const realPaginate = github.paginate;
  github.paginate = async (fn, options) => {
    // Only the check-run reads that happen after the disarm snapshot, which is
    // where the guard rounds live. Earlier ones belong to the evaluation.
    if (fn === github.rest.checks.listForRef && autoMergeReads >= 1) {
      ownershipReads += 1;
    }
    return realPaginate(fn, options);
  };

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "manual", "a transient guard read must not fail the transaction");
  // Three auto-merge reads: the disarm snapshot, the round that failed, and the
  // round that succeeded.
  assert.equal(autoMergeReads, 3);
  // Three check-run reads after the disarm snapshot: the aggregate's own
  // evaluation, then ONE PER GUARD ROUND. Two rounds means two ownership reads —
  // the first round's answer is discarded rather than carried across the other
  // read's failure. Retrying inside the observations instead would leave two.
  assert.equal(
    ownershipReads,
    3,
    `ownership must be re-read for the retried round; saw ${ownershipReads} reads`,
  );
});

test("a throttled precondition sets the round's retry delay", async () => {
  // Codex P2 (round 7): every rejected precondition must succeed in the next
  // round, so taking the delay from rejected[0] alone can retry the whole round
  // inside another's throttle window — burning all three attempts on a condition
  // that was only rate-limited.
  const throttled = new Error("secondary rate limit");
  throttled.status = 403;
  throttled.response = { status: 403, headers: { "retry-after": "1" }, data: {} };
  const plain = new Error("fetch failed");

  const github = fakeGateGithub({ author: "detail-app" });
  let autoMergeReads = 0;
  let ownershipReads = 0;
  const realGraphql = github.graphql;
  github.graphql = async (query, variables) => {
    if (query.includes("query AutoMergeState")) {
      autoMergeReads += 1;
      // Throttle the auto-merge read of the first guard round only.
      if (autoMergeReads === 2) {
        throw throttled;
      }
    }
    return realGraphql(query, variables);
  };
  const realPaginate = github.paginate;
  github.paginate = async (fn, options) => {
    if (fn === github.rest.checks.listForRef && autoMergeReads >= 1) {
      ownershipReads += 1;
      // …and fail the ownership read of that same round, without any throttle
      // metadata. It is the first rejection, so a naive implementation reads its
      // fallback delay and ignores the throttle entirely.
      if (ownershipReads === 2) {
        throw plain;
      }
    }
    return realPaginate(fn, options);
  };

  const startedAt = Date.now();
  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });
  const elapsed = Date.now() - startedAt;

  assert.equal(transaction.state, "manual", "the round must recover after the throttle");
  // The throttle asked for a second; the fallback for this attempt is 250ms.
  assert.ok(
    elapsed >= 900,
    `the round must wait out the longest requested delay; waited ${elapsed}ms`,
  );
});

test("a guard read failure blocks cleanly instead of being retried as a write", async () => {
  // Codex P2 (round 4): a failing precondition is not a failing write. Left to
  // the write's retry classifier it was reissued three more times — nine reads —
  // and relabelled AutoGateCheckWriteError, losing the read-failure marker the
  // caller needs to publish a clean BLOCKED aggregate.
  // A transport failure on purpose: its message is what the write's retry
  // classifier matches on, so without the guard marker the wrapped failure looks
  // retryable to retryCheckUpdate and the whole three-attempt read is reissued
  // per write attempt.
  const unavailable = new Error("fetch failed");
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: false,
    autoMergeStateError: unavailable,
    // The disarm snapshot succeeds; the publish precondition is what fails.
    autoMergeStateErrorAfterRead: 1,
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "evaluation-error");
  assert.match(
    github.updatedChecks.at(-1).output.summary,
    /could not establish the publish preconditions for .* after 3 attempts: fetch failed/,
  );
  // Bounded: one disarm snapshot plus the guard's own three rounds. Not three of
  // those per write attempt, which is the nine-read behaviour this prevents.
  assert.equal(github.autoMergeStateReads, 4, "the guard must not be retried by the write");
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "no green may be published when the guard could not be established",
  );
});

test("a PR that joins the head after evaluation stops the PASS", async () => {
  // Codex P1 (round 4): the guarded set is frozen from this transaction's own
  // association snapshot, but reportAggregateDecision re-reads associations and
  // can pass for a LARGER set. A PR reopened or retargeted onto this head,
  // carrying a passing decision from an earlier transaction, would be inside the
  // green this write publishes and was never guarded here.
  const joined = {
    ...checkRun({
      id: 4242,
      name: decisionName(2048, HEAD_SHA),
      conclusion: "success",
      externalId: decisionExternalId(2048, HEAD_SHA),
    }),
    created_at: "2026-07-09T01:12:00Z",
    started_at: "2026-07-09T01:12:00Z",
    completed_at: "2026-07-09T01:12:00Z",
  };
  const github = fakeGateGithub({
    author: "detail-app",
    checkRuns: [...happyCheckRuns(), joined],
    // The transaction evaluates 1465 alone; the aggregate's own final read sees
    // 2048 as well.
    associatedPullRequestSnapshots: [
      // The transaction's own snapshot, then the aggregate's final read.
      [{ number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } }],
      [
        { number: 1465, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
        { number: 2048, state: "open", base: { ref: "master" }, head: { sha: HEAD_SHA } },
      ],
    ],
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "association-changed");
  assert.ok(
    !github.updatedChecks.some((check) => check.conclusion === "success"),
    "no green may cover a PR this transaction never evaluated",
  );
});

test("the publish precondition runs after the aggregate's own reads", async () => {
  // Codex P1 (round 2): reportAggregateDecision reads associations and check runs
  // BEFORE it writes, so a guard that ran before it was already stale. The
  // confirmation is now a precondition of the write itself.
  const github = fakeGateGithub({ author: "detail-app", nativeAutoMergeEnabled: true });
  const order = [];
  const realPaginate = github.paginate;
  github.paginate = async (fn, options) => {
    order.push(`read:${fn.name}`);
    return realPaginate(fn, options);
  };
  const realGraphql = github.graphql;
  github.graphql = async (query, variables) => {
    if (query.includes("query AutoMergeState")) {
      order.push("auto-merge:read");
    }
    return realGraphql(query, variables);
  };
  const realUpdate = github.rest.checks.update;
  github.rest.checks.update = async (options) => {
    if (options.output?.title?.startsWith("PASS: every open master PR")) {
      order.push("aggregate:publish");
    }
    return realUpdate(options);
  };

  await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  const publish = order.lastIndexOf("aggregate:publish");
  const lastConfirm = order.lastIndexOf("auto-merge:read");
  assert.ok(publish > 0, "the aggregate PASS must have been published");
  assert.equal(
    lastConfirm + 1,
    publish,
    `nothing may read between the final auto-merge confirmation and the green; got ${JSON.stringify(order.slice(-6))}`,
  );
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

test("an automatic review clears the gate from its summary row alone", async () => {
  // #3606: Codex emits the `Reviewed commit:` prose line when a review is
  // REQUESTED, and only edits its summary table when it reviews automatically on
  // a push. The head was reviewed, passed, and blocked forever on "has not
  // reviewed head … yet" until someone posted `@codex review`.
  const github = fakeGateGithub({
    issueComments: [codexSummaryTable(HEAD_SHA, { rowTime: "2026-07-09T01:20:00Z" })],
  });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, true, `blocked on: ${result.reasons.join("; ")}`);
  assert.ok(result.notes.includes(`Codex verdict matches head ${HEAD_SHA}`));
});

test("a Running summary row is progress, not a verdict", async () => {
  const github = fakeGateGithub({
    issueComments: [codexSummaryTable(HEAD_SHA, { status: "⏳ **Running**" })],
  });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, false);
  // …and the message says a review exists rather than sending the reader to look
  // for one that never ran: the row names this head, it just is not a verdict.
  assert.ok(
    result.reasons.some((reason) =>
      reason.includes(`a Codex review exists for head ${HEAD_SHA} but carried no parseable verdict`),
    ),
    `got: ${result.reasons.join("; ")}`,
  );
});

test("a summary row naming an older head satisfies nothing", async () => {
  // Freshness is untouched by the second pattern: the row must name THIS head.
  const github = fakeGateGithub({ issueComments: [codexSummaryTable(OTHER_SHA)] });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, false);
  // No artifact names this head at all, so this is the genuinely-missing message.
  assert.ok(
    result.reasons.some((reason) => reason.includes(`Codex has not reviewed head ${HEAD_SHA} yet`)),
    `got: ${result.reasons.join("; ")}`,
  );
});

test("a summary row older than the head is stale evidence", async () => {
  // The ROW's time is the artifact time, held to the same rule the prose line's
  // comment time is: a verdict that predates the head proves nothing about it.
  const github = fakeGateGithub({
    headCommittedDate: "2026-07-09T02:00:00Z",
    issueComments: [codexSummaryTable(HEAD_SHA, { rowTime: "2026-07-09T01:00:00Z" })],
  });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, false);
  assert.ok(
    result.reasons.some((reason) =>
      reason.includes("Codex verdict for the head commit is older than the head commit timestamp"),
    ),
    `got: ${result.reasons.join("; ")}`,
  );
});

test("the row's own time is used, not the summary comment's", async () => {
  // The summary comment is edited on every review activity, so its updated_at
  // says when Codex last touched anything. Reading that as the verdict time
  // would make a stale row look fresh the moment Codex did something else.
  const github = fakeGateGithub({
    headCommittedDate: "2026-07-09T02:00:00Z",
    issueComments: [
      codexSummaryTable(HEAD_SHA, {
        rowTime: "2026-07-09T01:00:00Z",
        commentTime: "2026-07-09T03:00:00Z",
      }),
    ],
  });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, false, "a comment touched later must not refresh an old row");
});

test("a summary row never clears a finding in a review body for the same head", async () => {
  // The row records that a review COMPLETED; it is not evidence that the review
  // was clean, and Codex edits the table when it posts a finding. Letting the
  // row supersede the finding-carrying body would clear the finding by the very
  // act of recording it.
  const github = fakeGateGithub({
    issueComments: [
      codexReview(HEAD_SHA, "P1: this is wrong.", "2026-07-09T01:20:00Z"),
      codexSummaryTable(HEAD_SHA, { rowTime: "2026-07-09T01:30:00Z" }),
    ],
  });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, false);
  assert.ok(
    result.reasons.includes("latest exact-head Codex review body contains a P0-P3 finding"),
    `got: ${result.reasons.join("; ")}`,
  );
});

test("summary rows are read positionally, and only when complete", () => {
  const { parseSummaryRows, parseVerdictArtifact } = __test;
  const rows = parseSummaryRows(codexSummaryTable(HEAD_SHA).body);
  assert.equal(rows.length, 1, "the header and separator rows are not Code Review rows");
  assert.equal(rows[0].completed, true);
  assert.equal(rows[0].commit, HEAD_SHA.slice(0, 7));
  assert.equal(typeof rows[0].time, "number");

  // Each missing piece makes it a non-verdict rather than a malformed one.
  const running = codexSummaryTable(HEAD_SHA, { status: "⏳ **Running**" });
  assert.equal(parseVerdictArtifact(running, HEAD_SHA), null);
  const noTime = codexSummaryTable(HEAD_SHA, { rowTime: "" });
  assert.equal(parseVerdictArtifact(noTime, HEAD_SHA), null);
  const noCommit = codexSummaryTable(HEAD_SHA, { commitCell: "—" });
  assert.equal(parseVerdictArtifact(noCommit, HEAD_SHA), null);

  // The prose form is untouched and still reports its own kind.
  assert.equal(parseVerdictArtifact(codexVerdict(HEAD_SHA), HEAD_SHA).kind, "prose");
  assert.equal(parseVerdictArtifact(codexSummaryTable(HEAD_SHA), HEAD_SHA).kind, "summary-row");
});

test("a table-looking body without the summary marker is not a verdict", async () => {
  // Codex P1: parseSummaryRows ran on ANY Codex body, so a review that merely
  // QUOTES this table format — which reviewing this gate does — parsed as a
  // `summary-row` artifact. The P0-P3 check reads bound artifacts, and a body
  // misclassified this way could carry a finding past it.
  const quoting = {
    user: { login: "chatgpt-codex-connector[bot]" },
    body: [
      "Codex Review: P1 this is wrong.",
      "",
      "It should look like:",
      "| Review | Status | Commit | Review trigger |",
      "| --- | --- | --- | --- |",
      `| 📝 **Code Review** | ✅ **Completed** <relative-time datetime="2026-07-09T01:30:00Z">x</relative-time> | \`${HEAD_SHA.slice(0, 7)}\` | New commits |`,
    ].join("\n"),
    created_at: "2026-07-09T01:30:00Z",
    updated_at: "2026-07-09T01:30:00Z",
    commit_id: HEAD_SHA,
  };
  const github = fakeGateGithub({ issueComments: [quoting] });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, false);
  assert.equal(__test.parseVerdictArtifact(quoting, HEAD_SHA), null, "no marker, no verdict");
  // …and because it is bound to the head by commit_id, its finding is caught.
  assert.ok(
    result.reasons.includes("latest exact-head Codex review body contains a P0-P3 finding"),
    `got: ${result.reasons.join("; ")}`,
  );
});

test("a finding in an automatic review body blocks even with a Completed row", async () => {
  // Codex P1: an automatic review can carry a P0-P3 with NO `Reviewed commit:`
  // line — the very artifact shape this PR exists to support. It was filtered out
  // of the verdict set, so the Completed row passed while the finding beside it
  // was never inspected. Reviews are bound to the head by commit_id.
  const github = fakeGateGithub({
    issueComments: [codexSummaryTable(HEAD_SHA, { rowTime: "2026-07-09T01:30:00Z" })],
    reviews: [
      {
        user: { login: "chatgpt-codex-connector[bot]" },
        body: "Codex Review\n\nP1: this is wrong.",
        commit_id: HEAD_SHA,
        submitted_at: "2026-07-09T01:40:00Z",
      },
    ],
  });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.shouldMerge, false);
  assert.ok(
    result.reasons.includes("latest exact-head Codex review body contains a P0-P3 finding"),
    `got: ${result.reasons.join("; ")}`,
  );
});

test("a body finding blocks the manual-merge decision, not just the auto one", async () => {
  // Codex P1: the manual path consumes findingBlockers, not reasons. A body
  // finding recorded only in reasons publishes a PASSING manual decision, so the
  // required aggregate turns green and a maintainer merges with the finding
  // live — exactly what #3591 closed for inline findings.
  const github = fakeGateGithub({
    author: "outside-contributor",
    issueComments: [codexSummaryTable(HEAD_SHA, { rowTime: "2026-07-09T01:30:00Z" })],
    reviews: [
      {
        user: { login: "chatgpt-codex-connector[bot]" },
        body: "Codex Review\n\nP1: this is wrong.",
        commit_id: HEAD_SHA,
        submitted_at: "2026-07-09T01:40:00Z",
      },
    ],
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.notEqual(transaction.state, "manual", "a live body finding must not publish a manual PASS");
  const decision = github.createdChecks.find((check) => check.name === decisionName(1465, HEAD_SHA));
  assert.equal(decision.conclusion, "failure");
  assert.match(decision.output.summary, /P0-P3 finding/);
  // The remedy must be one that can actually terminate: no thread exists, so no
  // reply can clear it.
  assert.match(decision.output.summary, /request a fresh `@codex review`/);
  assert.doesNotMatch(decision.output.summary, /reply RESOLVED[^\n]*to clear this/);
});

test("a row whose cell prefixes a different commit does not match this head", () => {
  // The commit cell is matched as a prefix of the head, with no lookup: the head
  // SHA is already known, so there is nothing to resolve. What keeps a stale row
  // out is that its cell must equal THIS head's prefix — a cell that prefixes
  // some other commit does not, however valid that other commit is.
  const { parseVerdictArtifact } = __test;
  assert.notEqual(HEAD_SHA.slice(0, 7), OTHER_SHA.slice(0, 7), "the fixtures must differ");

  const otherHead = codexSummaryTable(OTHER_SHA);
  assert.equal(parseVerdictArtifact(otherHead, HEAD_SHA), null);
  assert.notEqual(parseVerdictArtifact(otherHead, OTHER_SHA), null, "it is a verdict for its own head");

  // The prose form is held to the same rule.
  assert.equal(parseVerdictArtifact(codexVerdict(OTHER_SHA), HEAD_SHA), null);
  assert.notEqual(parseVerdictArtifact(codexVerdict(HEAD_SHA), HEAD_SHA), null);
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

  assert.deepEqual(github.operations, [
    "merge",
    "check:create",
    "dispatch:build.yml",
    "dispatch:docs.yml",
    "dispatch:lint.yml",
    "dispatch:web-selftest.yml",
  ]);
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
  assert.ok(github.operations.includes("dispatch:docs.yml"));
});

test("a workflow dispatch failure remains single-shot after merge", async () => {
  const error = new Error("docs dispatch unavailable");
  // Retryable by status on purpose: a dispatch is a write, so it must NOT be
  // retried even when the classifier would call the failure transient. A second
  // accepted dispatch is a duplicate run, and for docs.yml a duplicate deploy.
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
  // One attempt per master-verify workflow and no more: a failing dispatch is
  // never retried, and one unavailable workflow does not cost master the rest.
  assert.equal(github.workflowDispatchAttempts, __test.MASTER_PUSH_WORKFLOWS.length);
});

test("an auto-gate merge re-raises every gate its own push suppressed", async () => {
  const github = fakeGateGithub({ files: ["session/storage.go"] });

  await autoGate.merge({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
  });

  assert.equal(github.mergedWith.sha, HEAD_SHA);
  assert.deepEqual(
    github.dispatchedWorkflows.map((dispatch) => dispatch.workflow_id),
    __test.MASTER_PUSH_WORKFLOWS,
  );
  // A dispatch takes a branch ref, never a SHA. master is the commit the merge
  // above just created.
  for (const dispatch of github.dispatchedWorkflows) {
    assert.equal(dispatch.ref, "master");
  }
  // Only docs.yml carries an input, and it is the merge COMMIT, never a path
  // list: docs.yml still owns which paths mean "deploy". A copy of that list
  // here would be a second source of truth, and it had already drifted.
  for (const dispatch of github.dispatchedWorkflows) {
    if (dispatch.workflow_id === "docs.yml") {
      assert.deepEqual(dispatch.inputs, { verify_sha: "merge-sha" });
    } else {
      assert.equal(dispatch.inputs, undefined);
    }
  }
});

test("a merge that changes workflows warns that its own set may be stale", async () => {
  // Codex P2: auto-gate.yml pins every checkout to the default branch, so this
  // loop re-raises the set known to master's PRE-merge copy of the gate. A merge
  // that ADDS a push-gated workflow cannot re-raise it for its own landing
  // commit — the running copy's list predates it. Deriving the set from the
  // merged tree would mean re-parsing triggers at merge time, where a misparse
  // silently skips a dispatch or reds a merge that already landed; the gap is
  // made visible instead.
  const warnings = [];
  const github = fakeGateGithub({ files: [".github/workflows/new-gate.yml"] });

  await autoGate.merge({
    github,
    context: fakeContext(),
    core: { ...fakeCore(), warning: (message) => warnings.push(message) },
    prNumber: 1465,
  });

  assert.equal(github.mergedWith.sha, HEAD_SHA);
  assert.ok(
    warnings.some((warning) => /changed workflow definitions/.test(warning)),
    `a workflow-changing merge must say its dispatch set may be stale; got ${JSON.stringify(warnings)}`,
  );

  // A merge that touches no workflow definition says nothing.
  const quiet = [];
  const other = fakeGateGithub({ files: ["session/storage.go"] });
  await autoGate.merge({
    github: other,
    context: fakeContext(),
    core: { ...fakeCore(), warning: (message) => quiet.push(message) },
    prNumber: 1465,
  });
  assert.deepEqual(quiet, []);
});

test("the re-raised Docs run decides deployment from its own path list", () => {
  // The gate must not carry a second copy of docs.yml's deploy paths. It had
  // one, and it had drifted: gate.docsChanged knew only `docs/` and `mkdocs.yml`
  // while docs.yml also deploys for README.md, commands/docs_gen.go,
  // requirements-docs.txt and scripts/gen-docs.sh — so an auto-gate merge
  // touching any of those left Pages stale.
  // Comments may name the paths — explaining the drift is the point. Code may
  // not carry them, so the scan is over the file with comments stripped.
  const gate = fs
    .readFileSync(path.join(__dirname, "auto-gate.js"), "utf8")
    .split("\n")
    .filter((line) => !/^\s*(\/\/|\*|\/\*)/.test(line))
    .join("\n");
  const docs = fs.readFileSync(path.join(__dirname, "..", "workflows", "docs.yml"), "utf8");

  // The gate names none of those paths, and passes no deploy input.
  for (const deployPath of [
    "mkdocs.yml",
    "README.md",
    "commands/docs_gen.go",
    "requirements-docs.txt",
    "scripts/gen-docs.sh",
  ]) {
    assert.doesNotMatch(
      gate,
      new RegExp(deployPath.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")),
      `${deployPath} is a docs.yml deploy path and must not be copied into the gate`,
    );
  }
  assert.doesNotMatch(gate, /deploy_docs/);

  // What the gate DOES name is the commit — publishing is not monotonic, so a
  // run raised for this merge must decide on this merge's paths even if master
  // advanced before it started.
  assert.match(gate, /verify_sha: response\.data\.sha/);
  assert.match(docs, /verify_sha:[\s\S]*?default: ''/);
  // The sha reaches the script through the ENVIRONMENT and is validated as a
  // full commit SHA. verify_sha is free-form input, and this step's run can
  // publish Pages, so substituting it into the program text would be a script
  // injection with a publish primitive on the end of it.
  assert.match(docs, /env:\s+EVENT_NAME:[\s\S]*?VERIFY_SHA: \$\{\{ inputs\.verify_sha \}\}/);
  assert.match(docs, /if \[\[ -n "\$VERIFY_SHA" && ! "\$VERIFY_SHA" =~ \^\[0-9a-f\]\{40\}\$ \]\]/);
  assert.match(docs, /after="\$\{VERIFY_SHA:-\$EVENT_SHA\}"/);
  assert.match(docs, /if \[\[ -n "\$VERIFY_SHA" \|\| -z "\$before"/);
  // No ${{ }} expansion may remain inside the step's shell program.
  const step = /- name: Check docs deploy paths[\s\S]*?\n      - name: /.exec(docs)[0];
  const program = step.slice(step.indexOf("run: |"));
  assert.doesNotMatch(program, /\$\{\{/, "no expression may be substituted into the shell program");

  // docs.yml accepts a dispatch that defers to its own rule, and falls through
  // to the same path diff a push uses when it does.
  assert.match(docs, /deploy_docs:[\s\S]*?default: auto[\s\S]*?options: \[auto, 'true', 'false'\]/);
  assert.match(docs, /"\$EVENT_NAME" == "workflow_dispatch" && "\$DEPLOY_DOCS" != "auto"/);
  assert.match(docs, /case "\$path" in\s+docs\/\*\|mkdocs\.yml\|README\.md/);
});

test("one unavailable master-verify workflow does not suppress the others", async () => {
  const github = fakeGateGithub({
    files: ["session/storage.go"],
    workflowDispatchErrorsByWorkflow: { "build.yml": new Error("build dispatch unavailable") },
  });

  await assert.rejects(
    autoGate.merge({
      github,
      context: fakeContext(),
      core: fakeCore(),
      prNumber: 1465,
    }),
    // The merge itself succeeded; the failure is reported as a post-merge error
    // so the run goes red and a human re-raises the missing gate by hand.
    /merged, but post-merge operation\(s\) failed[\s\S]*build dispatch unavailable/,
  );

  assert.equal(github.mergedWith.sha, HEAD_SHA);
  assert.deepEqual(
    github.dispatchedWorkflows.map((dispatch) => dispatch.workflow_id),
    __test.MASTER_PUSH_WORKFLOWS.filter((workflow) => workflow !== "build.yml"),
  );
});

test("the trigger scan asks a question no YAML spelling can hide", () => {
  // Six review rounds of parsing these triggers by hand each found another valid
  // spelling — shorthand, flow mappings, deeper indentation, comments in four
  // positions, unclosed flow sequences — and every one failed the same way: it
  // read as "no trigger here" rather than "cannot read this", so the workflow
  // left the comparison and MASTER_PUSH_WORKFLOWS together, silently.
  //
  // The parser is gone. The question is now lexical — does this file's trigger
  // section mention a push trigger? — which no indentation, flow style or
  // comment can change the answer to. It over-includes a push trigger scoped to
  // some other branch, and that is the point: over-inclusion is a test failure a
  // human resolves with an explicit exception, under-inclusion is a master
  // commit nothing verifies.
  const forms = [
    "on:\n  push:\n    branches: [master]\n\njobs:\n",
    "on:\n  push:\n    branches: [ master ]\n\njobs:\n",
    "on:\n  push:\n    branches:\n      - master\n\njobs:\n",
    "on:\n    push:\n      branches: [master]\n\njobs:\n",
    "on:\n  push:\n\njobs:\n",
    "on:\n  push: {}\n\njobs:\n",
    "on:\n  push: # every branch\n\njobs:\n",
    "on: # workflow events\n  push:\n\njobs:\n",
    "on:\n  pull_request:\n# master signal\n  push:\n\njobs:\n",
    "on: push\njobs:\n",
    "on: [push]\njobs:\n",
    "on: [push, pull_request]\njobs:\n",
    'on: {"push": {"branches": ["master"]}}\njobs:\n',
    "on:\n  push:\n    branches: [\n      master,\n    ]\n\njobs:\n",
    "on:\n  push:\n    branches: ['**']\n\njobs:\n",
  ];
  for (const form of forms) {
    assert.equal(
      mentionsPushTrigger(onSection(form)),
      true,
      `a push trigger must be seen in: ${JSON.stringify(form)}`,
    );
  }

  // Negatives: no push trigger at all, and a push mentioned only in a comment or
  // in a job — neither is a trigger, and comments are stripped before the scan.
  for (const form of [
    "on:\n  pull_request:\n    branches: [master]\n\njobs:\n",
    "on:\n  workflow_dispatch:\n\njobs:\n",
    "on:\n  # we deliberately do not run on push here\n  pull_request:\n\njobs:\n",
    "on:\n  pull_request:\n\njobs:\n  push:\n    runs-on: ubuntu-latest\n",
  ]) {
    assert.equal(
      mentionsPushTrigger(onSection(form)),
      false,
      `no push trigger may be seen in: ${JSON.stringify(form)}`,
    );
  }

  // A file with no readable `on:` section at all is null, which the anti-rot
  // test turns into a failure naming the file rather than a silent exclusion.
  assert.equal(onSection("name: x\njobs:\n  a:\n    runs-on: ubuntu-latest\n"), null);
});


test("the dispatch contract survives the spellings that hid from the parser", () => {
  // Flow mappings put `required:` mid-line, which the anchored check missed —
  // the same blind spot the parser had, reintroduced lexically.
  for (const flow of [
    "    inputs: { token: { required: true, type: string } }",
    "      token: { required: true }",
    "      token:\n        required: true",
  ]) {
    assert.match(withoutComments(flow), /required:\s*true\b/, `a required input must be seen in: ${flow}`);
  }
  assert.doesNotMatch(withoutComments("      token:\n        required: false"), /required:\s*true\b/);

  // A condition admits a dispatch only if it lets one through. Naming the event
  // in order to EXCLUDE it is the opposite.
  assert.equal(admitsDispatch("github.event_name == 'push' || github.event_name == 'workflow_dispatch'"), true);
  assert.equal(admitsDispatch("'push' == github.event_name"), false);
  assert.equal(admitsDispatch("github.event_name == 'push'"), false);
  assert.equal(admitsDispatch("github.event_name != 'workflow_dispatch'"), false);
  assert.equal(admitsDispatch("'workflow_dispatch' != github.event_name"), false);
  // Admitted only when an input the gate does not supply happens to be set: the
  // dispatch succeeds and the job it was raised for is skipped anyway.
  assert.equal(
    admitsDispatch(
      "github.event_name == 'push' || (github.event_name == 'workflow_dispatch' && inputs.run_checks)",
    ),
    false,
  );

  // A push-payload predicate never names the event and is true only for a push:
  // `github.event.head_commit` is absent on a dispatch, so the job is skipped.
  assert.equal(admitsDispatch("github.event.head_commit != null"), false);
  assert.equal(admitsDispatch("github.event.head_commit != null || github.event_name == 'workflow_dispatch'"), true);

  // Admission must be UNCONDITIONAL: any extra predicate on the dispatch branch
  // can make it false, whatever context it comes from. Enumerating `inputs.` was
  // the wrong shape — `vars.`, `env.`, `needs.` and whatever GitHub adds next
  // gate it just as well.
  assert.equal(
    admitsDispatch("github.event_name == 'push' || (github.event_name == 'workflow_dispatch' && vars.RUN_CHECKS == 'true')"),
    false,
  );
  assert.equal(
    admitsDispatch("github.event_name == 'push' || (github.event_name == 'workflow_dispatch' && inputs.run_checks)"),
    false,
  );
  assert.equal(
    admitsDispatch("(github.event_name == 'push' && github.ref == 'refs/heads/master') || github.event_name == 'workflow_dispatch'"),
    true,
  );
  assert.equal(admitsDispatch("'workflow_dispatch' == github.event_name"), true);
  // A bare dispatch test nested inside a conjunction is not a top-level disjunct.
  assert.equal(
    admitsDispatch("(github.event_name == 'workflow_dispatch' || github.event_name == 'push') && vars.ENABLED"),
    false,
  );

  // Indexed access is the same predicate as dotted access.
  assert.equal(consultsEvent("github['event']['head_commit'] != null"), true);
  assert.equal(consultsEvent("github[\"event_name\"] == 'push'"), true);
  assert.equal(consultsEvent("github.event.head_commit != null"), true);
  assert.equal(consultsEvent("github.event_name == 'push'"), true);
  assert.equal(consultsEvent("github.ref == 'refs/heads/master'"), false);

  // The type comes from the DIRECT property, never from prose that contains it.
  const prose = [
    "      verify_sha:",
    "        description: 'Expected type: string SHA'",
    "        type: boolean",
  ].join("\n");
  assert.equal(directProperties(prose).type, "boolean");
  assert.equal(
    directProperties("      verify_sha:\n        type: string").type,
    "string",
  );
  // A block scalar's content is nested deeper and is not a direct property.
  const scalar = [
    "      verify_sha:",
    "        description: |",
    "          type: string",
    "        type: boolean",
  ].join("\n");
  assert.equal(directProperties(scalar).type, "boolean");

  // Quoted shorthand is as valid as the bare form.
  for (const quoted of ["on: 'push'\njobs:\n", 'on: "push"\njobs:\n', "on: ['push']\njobs:\n", 'on: ["push", "pull_request"]\njobs:\n']) {
    assert.equal(
      mentionsPushTrigger(onSection(quoted)),
      true,
      `a quoted push trigger must be seen in: ${JSON.stringify(quoted)}`,
    );
  }
  assert.equal(mentionsPushTrigger(onSection("on: ['pull_request']\njobs:\n")), false);

  // A YAML alias resolves to a trigger GitHub honours and this scan cannot read,
  // so it is reported unreadable rather than answering "no push".
  assert.equal(usesUnresolvableYaml(onSection("on: *push_event\njobs:\n")), true);
  assert.equal(usesUnresolvableYaml(onSection("on:\n  <<: *base\n  push:\n\njobs:\n")), true);
  assert.equal(usesUnresolvableYaml(onSection("on:\n  push:\n    branches: [master]\n\njobs:\n")), false);
  // A glob in a paths filter is not an alias.
  assert.equal(
    usesUnresolvableYaml(onSection("on:\n  push:\n    paths:\n      - '**.go'\n\njobs:\n")),
    false,
  );

  // An expectation scoped to its own declaration cannot be satisfied by a LATER
  // input's properties.
  const twoInputs = [
    "    inputs:",
    "      verify_sha:",
    "        type: boolean",
    "      other:",
    "        type: string",
  ].join("\n");
  assert.doesNotMatch(
    declarationBlock(twoInputs, /^ *verify_sha:/) || "",
    /type: string/,
    "a later input's type must not satisfy verify_sha's expectation",
  );
  assert.match(
    declarationBlock(twoInputs.replace("type: boolean", "type: string"), /^ *verify_sha:/) || "",
    /type: string/,
  );

  // An exception holds only while the triggers it was justified against stand.
  const recorded = "on:\n  push:\n    branches: [release]\n";
  PUSH_TRIGGER_EXCEPTIONS["zz-probe.yml"] = { reason: "release-only", triggers: recorded };
  try {
    assert.equal(exemptedByRecordedTriggers("zz-probe.yml", onSection(recorded)), true);
    // Widened to include master: the reason no longer describes it, so the
    // exemption lapses and the workflow re-enters the comparison.
    assert.equal(
      exemptedByRecordedTriggers(
        "zz-probe.yml",
        onSection("on:\n  push:\n    branches: [release, master]\n"),
      ),
      false,
    );
    // Comment and whitespace churn alone does not lapse it.
    assert.equal(
      exemptedByRecordedTriggers(
        "zz-probe.yml",
        onSection("on:\n  push: # release train\n    branches: [release]   \n"),
      ),
      true,
    );
  } finally {
    delete PUSH_TRIGGER_EXCEPTIONS["zz-probe.yml"];
  }
});

test("the master-verify list names every workflow that gates master on push", () => {
  // The anti-rot mechanism for MASTER_PUSH_WORKFLOWS. A `push:` trigger cannot
  // be computed at runtime, so the gate carries a literal copy; this reads the
  // workflow directory and fails if that copy stops describing it — the same
  // arrangement web-selftest-scope.test.js uses for SELFTEST_PATHS.
  const workflowDir = path.join(__dirname, "..", "workflows");
  const workflowFiles = fs
    .readdirSync(workflowDir)
    .filter((name) => name.endsWith(".yml") || name.endsWith(".yaml"));
  const sections = new Map(
    workflowFiles.map((name) => [
      name,
      onSection(fs.readFileSync(path.join(workflowDir, name), "utf8")),
    ]),
  );

  // A trigger section this scan cannot read is a failure naming the file, never
  // a silent exclusion — that confusion is what every missed spelling had in
  // common.
  for (const [name, section] of sections) {
    assert.notEqual(
      section,
      null,
      `${name}: no \`on:\` section could be located, so this scan cannot tell whether the ` +
        "workflow runs on a master push",
    );
    assert.equal(
      usesUnresolvableYaml(section),
      false,
      `${name}: its \`on:\` section uses a YAML alias or merge key, which GitHub resolves and ` +
        "this scan cannot; inline the trigger or teach the scan to resolve it",
    );
  }

  const mentionsPush = workflowFiles
    .filter((name) => mentionsPushTrigger(sections.get(name)))
    .filter((name) => !exemptedByRecordedTriggers(name, sections.get(name)))
    .sort();

  assert.deepEqual(
    [...__test.MASTER_PUSH_WORKFLOWS].sort(),
    mentionsPush,
    "a workflow with a push trigger is missing from MASTER_PUSH_WORKFLOWS (or vice versa); " +
      "an auto-gate merge would land on master without it ever running. If its push trigger " +
      "genuinely cannot reach master, add it to PUSH_TRIGGER_EXCEPTIONS with the reason",
  );

  for (const name of __test.MASTER_PUSH_WORKFLOWS) {
    const section = sections.get(name);
    const file = fs.readFileSync(path.join(workflowDir, name), "utf8");

    // A dispatch is how the gate re-raises them, so each has to accept one.
    assert.match(
      withoutComments(section),
      /^ *workflow_dispatch:/m,
      `${name} is re-raised by the gate but declares no workflow_dispatch trigger`,
    );

    // …and has to accept the call merge() actually makes. A required input with
    // no value from the gate is a 422 AFTER the commit has landed, which is the
    // verification lost silently again. Deliberately blunt: any required input
    // at all must be one the gate supplies, so adding one forces the decision
    // rather than passing on a parse this test got wrong.
    // Anywhere in the section, not anchored to a line: a flow mapping such as
    // `inputs: { token: { required: true } }` puts it mid-line, which is exactly
    // the blind spot the previous anchored form had.
    const required = withoutComments(section).match(/required:\s*true\b/);
    assert.equal(
      required,
      null,
      `${name} declares a required workflow_dispatch input, but the gate sends only ` +
        `${JSON.stringify(SUPPLIED_DISPATCH_INPUTS[name] || [])}; either give it a default or ` +
        "supply it from merge()",
    );

    // The dispatched run must reach the same jobs the suppressed push would
    // have. Keyed on any `if:` condition that consults the EVENT at all — its
    // name or its payload — rather than on one spelling of one comparison.
    // `'push' == github.event_name` and `github.event_name != 'workflow_dispatch'`
    // exclude a dispatch just as effectively as the canonical form, and a payload
    // predicate like `github.event.head_commit != null` never mentions the name
    // while being true only for a push. Non-conditions that read the event — an
    // env passthrough, a concurrency `group:` — gate nothing and are not
    // conditions.
    const lines = file.split("\n");
    for (const [index, line] of lines.entries()) {
      if (!/^\s*if:/.test(line)) {
        continue;
      }
      const condition = declarationBlock(lines.slice(index).join("\n"), /^\s*if:/);
      if (!consultsEvent(condition)) {
        continue;
      }
      assert.ok(
        admitsDispatch(condition),
        `${name} gates a job or step on the event without admitting workflow_dispatch, so ` +
          `the re-raised run would skip it: ${condition.trim()}`,
      );
    }

    // Every input the gate supplies must still be declared, and still take the
    // kind of value the gate sends.
    for (const [input, expectation] of Object.entries(SUPPLIED_DISPATCH_INPUTS[name] || {})) {
      const declaration = new RegExp(`^ *${input}:[ \\t]*(?:#[^\\n]*)?$`, "m");
      assert.match(
        withoutComments(section),
        declaration,
        `the gate sends dispatch input "${input}" to ${name}, which no longer declares it; ` +
          "GitHub would reject the dispatch after the merge has landed",
      );
      const properties = directProperties(declarationBlock(withoutComments(section), declaration) || "");
      for (const [property, value] of Object.entries(expectation)) {
        assert.equal(
          properties[property],
          value,
          `${name}'s "${input}" input declares ${property}: ${properties[property]}, but merge() ` +
            `sends a value that needs ${property}: ${value}`,
        );
      }
    }
  }
});


test("a NOT_FOUND for the PR's own node id is retried instead of throwing", async () => {
  // #3396: this is not retryable by status (404), so before this change it went
  // straight out of retryTransient, past evaluate(), and reddened master as an
  // unhandled error — for an id the gate had resolved seconds earlier.
  const github = fakeGateGithub({
    readErrorsByFn: { listReviews: [selfContradictoryNotFound()] },
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: false,
  });

  assert.equal(transaction.state, "pass");
  assert.equal(transaction.aggregate.ok, true);
  assert.equal(github.readAttemptsByFn.listReviews, 2, "the contradicted read must be retried");
  assert.equal(github.pullGetReads, 1, "the retry must be justified by a cross-check");
});

test("a self-contradictory NOT_FOUND that never clears blocks instead of throwing", async () => {
  // Retries exhausted is still not an unhandled error: it is a read failure, so
  // the aggregate publishes a clean BLOCKED verdict a human can act on.
  const github = fakeGateGithub({
    readErrorsByFn: {
      listReviews: [
        selfContradictoryNotFound(),
        selfContradictoryNotFound(),
        selfContradictoryNotFound(),
      ],
    },
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: false,
  });

  assert.equal(transaction.state, "evaluation-error");
  assert.equal(transaction.aggregate.ok, false);
  assert.equal(github.readAttemptsByFn.listReviews, 3);
});

test("a NOT_FOUND for an id the gate did not resolve stays loud", async () => {
  // The property #3346 established on purpose. NOT_FOUND is only tolerated for
  // the node id THIS run resolved; anything else is real breakage and is not
  // retried even once.
  const github = fakeGateGithub({
    readErrorsByFn: { listReviews: [selfContradictoryNotFound("PR_some_other_node")] },
  });

  await assert.rejects(
    autoGate.processAggregateHead({
      github,
      context: fakeContext(),
      core: fakeCore(),
      headSha: HEAD_SHA,
      targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
      mergeEnabled: false,
    }),
    /Could not resolve to a node with the global id of 'PR_some_other_node'/,
  );

  assert.equal(github.readAttemptsByFn.listReviews, 1, "an unrelated NOT_FOUND must not retry");
  assert.equal(github.pullGetReads, 0, "an unrelated NOT_FOUND must not be cross-checked");
});

test("a PR that genuinely vanished mid-run concludes cleanly", async () => {
  // Deleted or transferred between the resolve and the read. A gone PR cannot be
  // evaluated and there is nothing to report on it, so the run concludes rather
  // than failing — and leaves any existing decision untouched.
  const notFound = new Error("Not Found");
  notFound.status = 404;
  const github = fakeGateGithub({
    readErrorsByFn: { listReviews: [selfContradictoryNotFound()] },
    pullGetError: notFound,
  });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.isOpen, false);
  assert.equal(result.shouldMerge, false);
  assert.deepEqual(result.reasons, ["PR #1465 no longer exists"]);
  // Not an evaluation error, so it never becomes the "Auto Gate evaluation
  // failed for PR #N" throw that reddens master.
  assert.doesNotMatch(result.summary, /auto-gate evaluation error/);

  // Nothing is published for a PR that is not there: no head was resolved, so
  // there is no (PR, head) decision to write.
  const write = await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result,
  });
  assert.equal(write.state, "unreported");
  assert.equal(github.createdChecks.length, 0);
});

test("a vanished PR ends its aggregate transaction without throwing", async () => {
  const notFound = new Error("Not Found");
  notFound.status = 404;
  const github = fakeGateGithub({
    readErrorsByFn: { listReviews: [selfContradictoryNotFound()] },
    pullGetError: notFound,
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  // The aggregate stays non-green — a head whose PR disappeared is not a head
  // that passed — but the run ends cleanly instead of as an unhandled error,
  // and nothing merges.
  assert.equal(transaction.state, "association-changed");
  assert.equal(github.mergedWith, null);
});

test("an unreadable ruleset never becomes an absent one", async () => {
  // Codex P1 (round 3): the 404 handler reads "this repository does not use
  // rulesets" and falls back to classic branch protection. A retry-exhausted
  // self-contradictory 404 carries status 404 too, so it was swallowed the same
  // way — and with neither mechanism readable the gate reports NO required
  // checks, which is a fail-open a merge can walk through.
  const notFound = selfContradictoryNotFound();
  const github = fakeGateGithub({ requestErrors: [notFound, notFound, notFound] });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "evaluation-error");
  assert.equal(github.mergedWith, null, "nothing may merge while required checks are unknown");
  assert.match(
    github.updatedChecks.at(-1).output.summary,
    /could not read branch rules for master after 3 attempts/,
  );
});

test("a PR that vanishes before its decision write concludes cleanly", async () => {
  // Codex P2 (round 3): the gone marker was converted only inside evaluate(),
  // so a deletion landing on reportDecision's reads still rethrew and reddened
  // the run.
  const gone = new Error("Not Found");
  gone.status = 404;
  const github = fakeGateGithub({ pullGetError: gone });
  let thrown = 0;
  const realPaginate = github.paginate;
  github.paginate = async (fn, options) => {
    // Let the evaluation finish, then fail the decision-reporting read.
    if (fn === github.rest.checks.listForRef && github.reviewCommentReads > 0 && thrown === 0) {
      thrown += 1;
      throw selfContradictoryNotFound();
    }
    return realPaginate(fn, options);
  };

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(thrown, 1);
  assert.equal(transaction.state, "association-changed");
  assert.equal(github.mergedWith, null);
});

test("an unclear cross-check answer is never read as a vanished PR", async () => {
  // Fail-closed on the unknown: only a definite 404 is evidence of absence. A
  // 500 during a degradation would otherwise conclude the PR was deleted.
  const unavailable = new Error("upstream unavailable");
  unavailable.status = 500;
  const github = fakeGateGithub({
    readErrorsByFn: { listReviews: [selfContradictoryNotFound()] },
    pullGetError: unavailable,
  });

  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });

  assert.equal(result.isOpen, true);
  assert.ok(!result.reasons.includes("PR #1465 no longer exists"));
  assert.equal(github.readAttemptsByFn.listReviews, 2, "an unknown answer still retries the read");
});

test("a NOT_FOUND paired with the resolved id in a different error is not tolerated", () => {
  // Codex P2: an envelope-wide substring test pairs a NOT_FOUND about one object
  // with our id appearing in some other error of a multi-error response. That
  // combination says nothing about the object this run resolved, so it must stay
  // loud.
  const { isSelfContradictoryNotFound } = __test;
  const nodeId = "PR_kwDORdIFwM8AAAABACUcDg";
  const split = new Error("Request failed due to following response errors:");
  split.name = "GraphqlResponseError";
  split.errors = [
    { type: "NOT_FOUND", message: "Could not resolve to a node with the global id of 'ISSUE_other'." },
    { type: "FORBIDDEN", message: `Resource not accessible: '${nodeId}'.` },
  ];
  assert.equal(isSelfContradictoryNotFound(split, nodeId), false);

  // The same two facts in one record is the real thing, and is tolerated.
  const together = new Error("Request failed due to following response errors:");
  together.name = "GraphqlResponseError";
  together.errors = [
    { type: "FORBIDDEN", message: "Resource not accessible: 'ISSUE_other'." },
    { type: "NOT_FOUND", message: `Could not resolve to a node with the global id of '${nodeId}'.` },
  ];
  assert.equal(isSelfContradictoryNotFound(together, nodeId), true);
});

test("a NOT_FOUND for a longer id that merely starts with ours is not tolerated", () => {
  // Node ids are opaque base64url, so a substring test accepts a different
  // object whose id happens to extend ours.
  const { isSelfContradictoryNotFound } = __test;
  const nodeId = "PR_kwDORdIFwM8AAAABACUcDg";
  assert.equal(isSelfContradictoryNotFound(selfContradictoryNotFound(nodeId + "XyZ"), nodeId), false);
  assert.equal(isSelfContradictoryNotFound(selfContradictoryNotFound("QQ" + nodeId), nodeId), false);
  // Exactly the id, delimited by the quotes around it, still matches.
  assert.equal(isSelfContradictoryNotFound(selfContradictoryNotFound(nodeId), nodeId), true);
});

test("a REST 404 whose structured body splits the pairing is not tolerated", () => {
  // Codex P2 (round 2): the per-record test correctly rejects a split
  // NOT_FOUND/node-id pairing, but the transport-message fallback then accepted
  // the same body because a 404's message is that body serialized. The fallback
  // is only for errors with no structured record at all.
  const { isSelfContradictoryNotFound } = __test;
  const nodeId = "PR_kwDORdIFwM8AAAABACUcDg";
  const body = {
    errors: [
      { type: "NOT_FOUND", message: "Could not resolve to a node with the global id of 'ISSUE_other'." },
      { type: "FORBIDDEN", message: `Resource not accessible: '${nodeId}'.` },
    ],
  };
  const split = new Error(`Not Found: ${JSON.stringify(body)}`);
  split.status = 404;
  split.response = { status: 404, data: body };
  assert.equal(isSelfContradictoryNotFound(split, nodeId), false);

  // The same transport with the two facts in ONE record is the real thing.
  const together = {
    errors: [
      { type: "FORBIDDEN", message: "Resource not accessible: 'ISSUE_other'." },
      { type: "NOT_FOUND", message: `Could not resolve to a node with the global id of '${nodeId}'.` },
    ],
  };
  const real = new Error(`Not Found: ${JSON.stringify(together)}`);
  real.status = 404;
  real.response = { status: 404, data: together };
  assert.equal(isSelfContradictoryNotFound(real, nodeId), true);
});

test("a decision-reporting read carries the resolved subject too", async () => {
  // Codex P2 (round 2): the reads inside reportDecision run after the PR was
  // resolved, so the same NOT_FOUND arriving there was non-retryable and
  // rethrew unhandled — the very outcome this PR removes. Driven directly at
  // reportDecision so the failure lands on ITS read rather than on whichever
  // check-run read happens to come first in a whole transaction.
  const github = fakeGateGithub();
  const result = await autoGate.evaluate({
    github,
    context: fakeContext(),
    core: fakeCore(),
    prNumber: 1465,
    setOutputs: false,
  });
  assert.equal(result.shouldMerge, true, "the fixture must reach reportDecision cleanly");

  const before = github.readAttemptsByFn.listForRef || 0;
  let thrown = 0;
  const realPaginate = github.paginate;
  github.paginate = async (fn, options) => {
    if (fn === github.rest.checks.listForRef && thrown === 0) {
      thrown += 1;
      throw selfContradictoryNotFound();
    }
    return realPaginate(fn, options);
  };

  const write = await autoGate.reportDecision({
    github,
    context: fakeContext(),
    core: fakeCore(),
    result,
  });

  assert.equal(thrown, 1);
  assert.equal(write.state, "pass", "the decision must publish rather than throw");
  assert.ok(
    (github.readAttemptsByFn.listForRef || 0) > before,
    "the contradicted read must be retried rather than rethrown",
  );
  assert.ok(github.pullGetReads > 0, "the retry must be justified by a cross-check");
});

test("the tolerated NOT_FOUND is exactly the one naming the resolved id", () => {
  const { isSelfContradictoryNotFound } = __test;
  const nodeId = "PR_kwDORdIFwM8AAAABACUcDg";

  assert.equal(isSelfContradictoryNotFound(selfContradictoryNotFound(nodeId), nodeId), true);
  // A GraphQL-transport NOT_FOUND for the same id, rather than a REST 404 body.
  const graphqlError = new Error(`Could not resolve to a node with the global id of '${nodeId}'.`);
  graphqlError.name = "GraphqlResponseError";
  graphqlError.errors = [{ type: "NOT_FOUND", message: graphqlError.message }];
  assert.equal(isSelfContradictoryNotFound(graphqlError, nodeId), true);

  // Everything else is untouched.
  assert.equal(isSelfContradictoryNotFound(selfContradictoryNotFound(nodeId), "PR_other"), false);
  assert.equal(isSelfContradictoryNotFound(selfContradictoryNotFound(nodeId), null), false);
  assert.equal(isSelfContradictoryNotFound(selfContradictoryNotFound(nodeId), ""), false);
  const forbidden = new Error(`FORBIDDEN for '${nodeId}'`);
  forbidden.status = 403;
  forbidden.response = { status: 403, data: { type: "FORBIDDEN", message: forbidden.message } };
  assert.equal(isSelfContradictoryNotFound(forbidden, nodeId), false);
  const plainNotFound = new Error("Not Found");
  plainNotFound.status = 404;
  assert.equal(isSelfContradictoryNotFound(plainNotFound, nodeId), false);
});

function mergeRefusal(message, status = 405) {
  const error = new Error(message);
  error.status = status;
  error.response = { status, data: { message } };
  return error;
}

test("a merge already in progress is conceded once the winner has landed", async () => {
  // #3434 verbatim: the maintainer merged PR #3411 by hand while the gate was
  // mid-evaluation on the same head. The gate's merge write lost, and the losing
  // evaluation reddened an Auto Gate run on master for an outcome that had
  // converged correctly.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Merge already in progress"),
    pullGetSnapshots: [{ merged: true, merge_commit_sha: "winner-sha" }],
  });

  const { notices, error } = await runApplyGateStep({ github });

  assert.equal(error, null, "a conceded merge race must not fail the run");
  assert.ok(
    notices.some((notice) =>
      /Conceding merge-refused race for PR #1465: the winning outcome already merged winner-sha\./.test(
        notice,
      ),
    ),
    `the concession notice must name the winner; got ${JSON.stringify(notices)}`,
  );
});

test("a merge already in progress is conceded after the winner settles", async () => {
  // The refusal names a merge that has STARTED. A single confirming read can
  // race ahead of it and see a PR that is still open, so this shape re-reads.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Merge already in progress"),
    pullGetSnapshots: [
      { merged: false, merge_commit_sha: null },
      { merged: true, merge_commit_sha: "settled-sha" },
    ],
  });

  const { notices, error } = await runApplyGateStep({ github });

  assert.equal(error, null);
  assert.ok(github.pullGetReads > 1, "a settling refusal must re-read before concluding");
  assert.ok(notices.some((notice) => notice.includes("already merged settled-sha")));
});

test("the settlement window outlasts a slow in-flight merge", async () => {
  // Codex P2: the settlement re-reads reused the generic read-retry delays, so
  // the winner had 1.25s total to land. A merge GitHub has already STARTED can
  // take longer than that, and the run then read back "nobody merged" and failed
  // — refusing the concession this shape exists to grant.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Merge already in progress"),
    // Still open through the first three reads; merged only on the fourth, which
    // the old 250ms + 1s window could never have reached.
    pullGetSnapshots: [
      { merged: false, merge_commit_sha: null },
      { merged: false, merge_commit_sha: null },
      { merged: false, merge_commit_sha: null },
      { merged: true, merge_commit_sha: "slow-winner-sha" },
    ],
  });

  const { notices, error } = await runApplyGateStep({ github });

  assert.equal(error, null, "a merge that lands late is still a conceded race");
  assert.ok(notices.some((notice) => notice.includes("already merged slow-winner-sha")));
  assert.equal(github.pullGetReads, 4);
});

test("a merge already in progress on a head nobody merged still fails loudly", async () => {
  // The loud path the concession must not swallow: a refusal with the PR still
  // open and no winner is a genuinely unmergeable head.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Merge already in progress"),
    pullGetSnapshots: [{ merged: false, merge_commit_sha: null }],
  });

  const { notices, error } = await runApplyGateStep({ github });

  assert.match(error?.message || "", /Merge already in progress/);
  assert.equal(error?.autoGateMergeConceded, undefined);
  assert.ok(!notices.some((notice) => notice.includes("Conceding merge-refused race")));
  // Bounded: exhausting the settlement window concludes "nobody merged" rather
  // than holding the serialized lane open waiting for one. Four reads — the
  // first plus one per MERGE_SETTLE_DELAYS_MS step.
  assert.equal(github.pullGetReads, 4);
});

test("a transient read inside the settlement window does not abandon it", async () => {
  // Codex P2: a raw read would replace the merge refusal with the read error AND
  // skip the remaining confirmations — so the winning merge could land inside the
  // very window this loop exists to wait through, and the run would still fail.
  const unavailable = new Error("pulls.get unavailable");
  unavailable.status = 500;
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Merge already in progress"),
    pullGetErrors: [unavailable],
    pullGetSnapshots: [
      { merged: false, merge_commit_sha: null },
      { merged: true, merge_commit_sha: "settled-sha" },
    ],
  });

  const { notices, error } = await runApplyGateStep({ github });

  assert.equal(error, null, "a failed confirmation must not become the raised error");
  assert.ok(notices.some((notice) => notice.includes("already merged settled-sha")));
});

test("a newer-owner concession does not steal the head back from its winner", async () => {
  // Codex P1 (round 1): the generic merge-error catch invalidated the aggregate
  // on the way out, creating a WAITING generation NEWER than the winner's. The
  // winning transaction would then see itself superseded and refuse to publish,
  // while nothing was left to finish the generation the losing run had created.
  //
  // The marker is the contract between the workflow's merge wrapper and this
  // helper — `the apply-gate step delegates refusal classification to the
  // helper` pins that the wrapper sets it — so the refusal is injected already
  // carrying it. Injecting a newer check through the fake instead would also be
  // seen by the aggregate evaluation, which would refuse to publish PASS and the
  // merge would never be attempted at all.
  const conceded = new Error("Conceding merge-refused race for PR #1465: newer owner");
  conceded.autoGateMergeConceded = true;
  conceded.autoGateConcessionReason = "newer-owner";
  const github = fakeGateGithub({ mergeError: conceded });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(transaction.state, "conceded");
  assert.equal(github.mergeAttempts, 1);
  // This transaction's own PASS is the last aggregate write. No WAITING
  // generation follows it, so the winner keeps ownership of the head.
  assert.equal(github.updatedChecks.at(-1).conclusion, "success");
  assert.ok(
    !github.createdChecks
      .slice(1)
      .some((check) => check.output?.title?.startsWith("WAITING")),
    "a newer-owner concession must not create a newer WAITING generation",
  );
});

test("a merged-PR concession still turns the shared head non-green", async () => {
  // Codex P1 (round 2): conceding because THIS PR merged says nothing about a
  // second PR sharing the head, which would otherwise inherit the PASS this
  // transaction just published — authorization built on a master that has since
  // advanced. A token-authenticated winning merge raises no event, so nothing
  // would come along to repair it.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Merge already in progress"),
    pullGetSnapshots: [{ merged: true, merge_commit_sha: "winner-sha" }],
  });

  const { error, notices } = await runApplyGateStep({ github });

  assert.equal(error, null, "the race is still conceded rather than reddening the run");
  assert.ok(notices.some((notice) => notice.includes("already merged winner-sha")));
  // …and the head is left non-green, exactly as a successful merge leaves it.
  assert.ok(
    github.createdChecks
      .slice(1)
      .some((check) => check.output?.title?.startsWith("WAITING")),
    "a merged-PR concession must not leave a stale PASS for a shared head",
  );
});

test("a genuinely failed merge still invalidates the aggregate before propagating", async () => {
  // The concession skip must not weaken the loud path: an unconceded merge error
  // still turns the published aggregate non-green on its way out.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Base branch was modified"),
    pullGetSnapshots: [{ merged: false, merge_commit_sha: null }],
  });

  const { error } = await runApplyGateStep({ github });

  assert.match(error?.message || "", /Base branch was modified/);
  assert.ok(
    github.createdChecks
      .slice(1)
      .some((check) => check.output?.title?.startsWith("WAITING")),
    "an unconceded merge failure must still invalidate the aggregate",
  );
});

test("every conceded refusal shape is conceded only against a proven winner", async () => {
  // Guards the whole list at once, including the two shapes #3329 added that had
  // no test at all — which is how a fourth shape reached the unhandled path.
  for (const shape of __test.CONCEDED_MERGE_REFUSALS) {
    const message = shape.pattern.source.replace(/\\b/g, "");

    const winner = fakeGateGithub({
      mergeError: mergeRefusal(message, shape.status),
      pullGetSnapshots: [{ merged: true, merge_commit_sha: "winner-sha" }],
    });
    const conceded = await runApplyGateStep({ github: winner });
    assert.equal(conceded.error, null, `${message} must be conceded when the PR is merged`);

    const nobody = fakeGateGithub({
      mergeError: mergeRefusal(message, shape.status),
      pullGetSnapshots: [{ merged: false, merge_commit_sha: null }],
    });
    const loud = await runApplyGateStep({ github: nobody });
    assert.match(
      loud.error?.message || "",
      new RegExp(message),
      `${message} must stay loud when nothing won the head`,
    );
  }
});

test("a live newer owner outranks merged evidence on a shared head", async () => {
  // Codex P1 (round 3): the two evidence paths are not mutually exclusive. A
  // shared head can have THIS PR merged while a newer transaction is mid-flight
  // on the same commit. Labelling that "merged" sends the caller down the
  // invalidation path and supersedes the active winner — the ownership theft the
  // newer-owner branch exists to avoid.
  const github = fakeGateGithub();
  const ownedAggregateCheck = { id: 1, created_at: "2026-07-09T01:00:00Z" };
  github.rest.pulls.get = async () => ({
    data: { merged: true, merge_commit_sha: "winner-sha" },
  });
  github.paginate = async () => [
    {
      id: 2,
      created_at: "2026-07-09T01:30:00Z",
      name: "Auto Gate decision",
      external_id: aggregateExternalId(HEAD_SHA),
      app: { id: ACTIONS_APP_ID },
      conclusion: "failure",
      output: { title: "WAITING: refreshing every PR/head decision at this commit" },
      html_url: "https://example.invalid/checks/2",
    },
  ];

  const concession = await autoGate.resolveMergeRefusal({
    github,
    error: mergeRefusal("Merge already in progress"),
    options: { owner: "sachiniyer", repo: "agent-factory", pull_number: 1465, sha: HEAD_SHA },
    ownedAggregateCheck,
  });

  assert.equal(
    concession.reason,
    "newer-owner",
    "a live owner must outrank merged evidence, so the caller does not write over it",
  );

  // With no newer owner, the same merged read is still a "merged" concession.
  github.paginate = async () => [];
  const merged = await autoGate.resolveMergeRefusal({
    github,
    error: mergeRefusal("Merge already in progress"),
    options: { owner: "sachiniyer", repo: "agent-factory", pull_number: 1465, sha: HEAD_SHA },
    ownedAggregateCheck,
  });
  assert.equal(merged.reason, "merged");
});

test("an unlisted merge refusal is never conceded, even on a merged PR", async () => {
  // The concession is granted on shape AND evidence. A refusal shape nobody has
  // audited must reach the loud path however healthy the PR looks.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Base branch was modified"),
    pullGetSnapshots: [{ merged: true, merge_commit_sha: "winner-sha" }],
  });

  const { error } = await runApplyGateStep({ github });

  assert.match(error?.message || "", /Base branch was modified/);
  assert.equal(github.pullGetReads, 0, "an unlisted shape must not even be investigated");
});

test("a merged PR with an unreadable owner concedes without writing", async () => {
  // Codex P1: the merged branch fell back with `||`, which discards the
  // ownership-unknown marker and answers "merged" — sending the caller into the
  // generic invalidation. Both facts have to survive: the race is conceded
  // because the PR really did merge, AND the aggregate must not be written,
  // because a newer generation created blind supersedes whoever owns the head.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Merge already in progress"),
    pullGetSnapshots: [{ merged: true, merge_commit_sha: "winner-sha" }],
  });
  const realPaginate = github.paginate;
  github.paginate = async (fn, options) => {
    if (fn === github.rest.checks.listForRef && options?.filter === "all") {
      throw new Error("fetch failed");
    }
    return realPaginate(fn, options);
  };

  const { error, notices } = await runApplyGateStep({ github });

  assert.equal(error, null, "a merged PR is still a conceded race");
  assert.ok(notices.some((notice) => notice.includes("already merged winner-sha")));
  assert.ok(
    notices.some((notice) => notice.includes("ownership could not be determined")),
    "the notice must say why the aggregate was left alone",
  );
  assert.ok(
    !github.createdChecks
      .slice(1)
      .some((check) => check.output?.title?.startsWith("WAITING")),
    "an undetermined owner must not be overwritten by a blind invalidation",
  );
});

test("an unreadable ownership check never becomes 'nobody owns this head'", async () => {
  // Codex P1: readOrNull turned a transient listForRef failure into an EMPTY
  // list, which reads as "no newer owner" — and the caller acts on that by
  // creating another invalidation, superseding the very transaction the failed
  // read could not see. "No answer" and "no owner" must not be the same value.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Repository rule violations found"),
    pullGetSnapshots: [{ merged: false, merge_commit_sha: null }],
  });
  const unavailable = new Error("fetch failed");
  const realPaginate = github.paginate;
  let checkReads = 0;
  github.paginate = async (fn, options) => {
    if (fn === github.rest.checks.listForRef && options?.filter === "all") {
      checkReads += 1;
      throw unavailable;
    }
    return realPaginate(fn, options);
  };

  const { error } = await runApplyGateStep({ github });

  // The refusal stays LOUD: nothing proved another actor won, the PR is still
  // open, and the merge did not happen — conceding would leave the aggregate's
  // PASS standing for a merge that never occurred.
  assert.match(error?.message || "", /Repository rule violations found/);
  // Retried before giving up, then reported as unknown rather than as absent.
  assert.equal(checkReads, 3);
  // …and crucially, no new WAITING generation: a blind invalidation here is what
  // supersedes whichever transaction actually owns the head.
  assert.ok(
    !github.createdChecks
      .slice(1)
      .some((check) => check.output?.title?.startsWith("WAITING")),
    "an undetermined owner must not be overwritten by a blind invalidation",
  );
});

test("a newer transaction owning the head concedes a refused merge", async () => {
  // The second evidence path: no merge happened, but a newer generation of the
  // aggregate has taken ownership of this head, so this run no longer decides.
  const github = fakeGateGithub({
    mergeError: mergeRefusal("Repository rule violations found"),
    pullGetSnapshots: [{ merged: false, merge_commit_sha: null }],
  });
  const ownedAggregateCheck = { id: 1, created_at: "2026-07-09T01:00:00Z" };
  const newer = {
    id: 2,
    created_at: "2026-07-09T01:30:00Z",
    name: "Auto Gate decision",
    external_id: aggregateExternalId(HEAD_SHA),
    app: { id: ACTIONS_APP_ID },
    conclusion: "failure",
    output: { title: "WAITING: refreshing every PR/head decision at this commit" },
    html_url: "https://example.invalid/checks/2",
  };
  github.paginate = async () => [newer];

  const concession = await autoGate.resolveMergeRefusal({
    github,
    error: mergeRefusal("Repository rule violations found"),
    options: { owner: "sachiniyer", repo: "agent-factory", pull_number: 1465, sha: HEAD_SHA },
    ownedAggregateCheck,
  });

  assert.equal(concession.reason, "newer-owner");
  assert.match(concession.message, /is newer Auto Gate check https:\/\/example\.invalid\/checks\/2/);

  // An OLDER check is not a winner: generation order is what makes this safe.
  const older = { ...newer, id: 0, created_at: "2026-07-09T00:30:00Z" };
  github.paginate = async () => [older];
  assert.equal(
    await autoGate.resolveMergeRefusal({
      github,
      error: mergeRefusal("Repository rule violations found"),
      options: { owner: "sachiniyer", repo: "agent-factory", pull_number: 1465, sha: HEAD_SHA },
      ownedAggregateCheck,
    }),
    null,
  );
});

test("an unknown owned generation concedes nothing", async () => {
  // Fail-closed on the unknown. Reading a missing created_at as generation zero
  // would make every check on the head — this transaction's own included — look
  // like a later owner, turning the safety check into a blanket concession.
  const github = fakeGateGithub({});
  github.paginate = async () => [
    {
      id: 10000,
      created_at: CHECK_CREATED_AT,
      name: "Auto Gate decision",
      external_id: aggregateExternalId(HEAD_SHA),
      app: { id: ACTIONS_APP_ID },
      conclusion: "failure",
      output: { title: "WAITING: refreshing every PR/head decision at this commit" },
    },
  ];

  for (const owned of [null, { id: 1 }, { created_at: "2026-07-09T01:00:00Z" }, { id: 1, created_at: "nonsense" }]) {
    assert.equal(
      await autoGate.resolveMergeRefusal({
        github,
        error: mergeRefusal("Repository rule violations found"),
        options: { owner: "sachiniyer", repo: "agent-factory", pull_number: 1465, sha: HEAD_SHA },
        ownedAggregateCheck: owned,
      }),
      null,
      `an owned check of ${JSON.stringify(owned)} must not concede`,
    );
  }
});

test("the apply-gate step delegates refusal classification to the helper", () => {
  // The shapes are auditable in one place only if the workflow has no second
  // copy of them. Nothing in the step may name a refusal shape of its own.
  const workflow = fs.readFileSync(AUTO_GATE_WORKFLOW, "utf8");

  assert.match(workflow, /await autoGate\.resolveMergeRefusal\(\{/);
  assert.match(workflow, /if \(concession\) \{\s+concede\(concession\.message, error, concession\.reason\);/);
  // The reason must reach processAggregateHead, which is what decides whether
  // the transaction may still write to the aggregate on its way out.
  assert.match(workflow, /concession\.autoGateConcessionReason = reason;/);
  for (const shape of __test.CONCEDED_MERGE_REFUSALS) {
    assert.doesNotMatch(
      workflow,
      new RegExp(shape.pattern.source, "i"),
      "a conceded refusal shape is named in the workflow as well as the helper",
    );
  }
});

test("a merged head branch is deleted", async () => {
  // #3603: delete_branch_on_merge does not fire for a GITHUB_TOKEN merge, so
  // every auto-gate merge left its branch behind and origin regrew to 201.
  const github = fakeGateGithub();

  await autoGate.merge({ github, context: fakeContext(), core: fakeCore(), prNumber: 1465 });

  assert.equal(github.mergedWith.sha, HEAD_SHA);
  assert.deepEqual(github.deletedRefs, ["heads/siyer/fix-3603"]);
});

test("a fork's head branch is never deleted", async () => {
  // (a) Not ours to delete, and the token could not anyway. Driven directly:
  // a fork head cannot reach this step through merge(), because it fails the
  // gate first — so asserting through merge() would pass whatever this check
  // did.
  const github = fakeGateGithub();

  await __test.deleteMergedHeadRef({
    github,
    context: fakeContext(),
    core: fakeCore(),
    gate: {
      headRefName: "siyer/fix-3603",
      headRepository: "contributor/agent-factory",
      headSha: HEAD_SHA,
    },
    prNumber: 1465,
  });

  assert.deepEqual(github.deletedRefs, []);
  assert.deepEqual(github.refReads, [], "a fork head is not even inspected");

  // The same branch on this repository is deleted, so the fixture proves the
  // check and not merely an inert path.
  await __test.deleteMergedHeadRef({
    github,
    context: fakeContext(),
    core: fakeCore(),
    gate: {
      headRefName: "siyer/fix-3603",
      headRepository: "sachiniyer/agent-factory",
      headSha: HEAD_SHA,
    },
    prNumber: 1465,
  });
  assert.deepEqual(github.deletedRefs, ["heads/siyer/fix-3603"]);
});

test("a head branch that moved after the merge is kept", async () => {
  // (b) A lane that pushed after the merge keeps its branch: that work is not in
  // master, and the pushed ref may be the only copy of it anywhere.
  const github = fakeGateGithub({ remoteRefSha: OTHER_SHA });

  await autoGate.merge({ github, context: fakeContext(), core: fakeCore(), prNumber: 1465 });

  assert.equal(github.mergedWith.sha, HEAD_SHA);
  assert.deepEqual(github.deletedRefs, [], "a moved ref carries work this merge did not take");
});

test("a head branch that another open PR is based on is kept", async () => {
  // (c) Deleting it would close that PR and throw away its review.
  const github = fakeGateGithub({ dependentPullRequests: [{ number: 2048 }] });

  await autoGate.merge({ github, context: fakeContext(), core: fakeCore(), prNumber: 1465 });

  assert.deepEqual(github.deletedRefs, []);
});

test("a head branch already gone is a no-op, not a failure", async () => {
  const github = fakeGateGithub({ remoteRefSha: null });

  await autoGate.merge({ github, context: fakeContext(), core: fakeCore(), prNumber: 1465 });

  assert.deepEqual(github.deletedRefs, [], "nothing left to prune is the desired end state");
});

test("a deletion failure never reds a merge that already landed", async () => {
  // The whole point of the step being non-fatal: the merge has happened, and no
  // pruning problem is worth reporting it as a failure.
  const gone = new Error("Reference does not exist");
  gone.status = 422;
  const github = fakeGateGithub({ deleteRefError: gone });

  await autoGate.merge({ github, context: fakeContext(), core: fakeCore(), prNumber: 1465 });
  assert.equal(github.mergedWith.sha, HEAD_SHA);

  // An unexpected failure is reported, but only as a post-merge error — the
  // merge itself still stands.
  const broken = new Error("ref service unavailable");
  broken.status = 500;
  const failing = fakeGateGithub({ deleteRefError: broken });
  await assert.rejects(
    autoGate.merge({ github: failing, context: fakeContext(), core: fakeCore(), prNumber: 1465 }),
    /merged, but post-merge operation\(s\) failed[\s\S]*ref service unavailable/,
  );
  assert.equal(failing.mergedWith.sha, HEAD_SHA, "the merge still happened");
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

// #3558. The manual-merge path passed the required decision check for EVERY
// blocker, so a maintainer's `gh pr merge` shipped live Codex findings — three
// times on 2026-09-01 (#3534, #3545, #3546), each one producing a master-health
// issue. A live finding is a claim about the CODE; it does not become less true
// because of who opened the PR, and the usage-limit degradation two blocks below
// already refuses to waive one for the analogous reason.
test("a live finding blocks the manual-merge decision for a non-allowed author", async () => {
  const result = await evaluateGate({
    author: "detail-app",
    issueComments: [codexVerdict(HEAD_SHA)],
    reviewComments: [codexFinding({ id: 10, line: 32 })],
  });

  assert.equal(result.manualMergeRequired, true, "the PR is still maintainer-merged");
  assert.deepEqual(
    result.manualMergeBlockers.map((blocker) => blocker.reason),
    ["1 unresolved live Codex inline finding(s)"],
  );
  // The remedy travels WITH the blocker rather than being inferred from its text
  // when the summary is rendered.
  assert.match(result.manualMergeBlockers[0].remedy, /RESOLVED, ACCEPTED or \[gate-ack\]/);
  assert.match(result.summary, /^BLOCKED:/, "the decision must not read as a pass");
  assert.match(result.summary, /1 unresolved live Codex inline finding/);
  // The summary is where a maintainer finds out what to do about it, so it names
  // the clearing mechanism rather than only the count.
  assert.match(result.summary, /RESOLVED, ACCEPTED or \[gate-ack\]/);
  assert.doesNotMatch(
    result.summary,
    /Unmet automatic-merge requirements/,
    "a blocker is lifted out of the advisory list, not repeated inside it",
  );
});

// A blocker and an advisory note in one decision. They must not be presented as
// one list: the whole failure was a hard blocker reading like another line the
// maintainer could weigh up on the way to merging.
test("a blocked manual decision separates the blocker from the advisory notes", async () => {
  const result = await evaluateGate({
    author: "detail-app",
    files: ["app/termpane.go"],
    issueComments: [codexVerdict(HEAD_SHA)],
    reviewComments: [codexFinding({ id: 10, line: 32 })],
  });

  const [blocking, advisory] = result.summary.split("Unmet automatic-merge requirements:");
  assert.match(blocking, /^BLOCKED:/);
  assert.match(blocking, /1 unresolved live Codex inline finding/);
  assert.ok(advisory, "the advisory section must still be rendered");
  assert.match(advisory, /missing the play-tested label/);
  assert.doesNotMatch(advisory, /unresolved live Codex inline finding/);
});

// The required check a hand merge is actually gated on is the fixed-name
// aggregate, which greens only when every per-PR decision is completed/success.
// Asserting the per-PR conclusion alone would leave the fix unproven where it
// has to bite.
test("a live finding keeps the required aggregate red for a non-allowed author", async () => {
  const github = fakeGateGithub({
    author: "detail-app",
    nativeAutoMergeEnabled: true,
    issueComments: [codexVerdict(HEAD_SHA)],
    reviewComments: [codexFinding({ id: 10, line: 32 })],
  });

  const transaction = await autoGate.processAggregateHead({
    github,
    context: fakeContext(),
    core: fakeCore(),
    headSha: HEAD_SHA,
    targets: [{ prNumber: 1465, headSha: HEAD_SHA }],
    mergeEnabled: true,
  });

  assert.equal(github.mergedWith, null, "nothing may merge automatically either");
  const exactDecision = github.createdChecks.find(
    (check) => check.name === decisionName(1465, HEAD_SHA),
  );
  assert.equal(exactDecision.conclusion, "failure");
  assert.match(exactDecision.output.title, /^BLOCKED:/);
  assert.equal(
    transaction.aggregate.ok,
    false,
    "the required aggregate must stay red, or a hand merge is still allowed",
  );
});

// A RESOLVED reply with no commit after it is the #2878 defect: the claimed fix
// cannot be in a head that predates the claim. That is a live finding too, so it
// blocks the manual path alongside the unanswered kind.
test("a RESOLVED claim with no pushed commit blocks the manual-merge decision", async () => {
  const result = await evaluateGate({
    author: "detail-app",
    headCommittedDate: "2026-07-09T01:00:00Z",
    issueComments: [codexVerdict(HEAD_SHA)],
    reviewComments: [
      codexFinding({ id: 10, line: 32 }),
      findingReply({ id: 11, inReplyToId: 10, body: "RESOLVED — fixed." }),
    ],
  });

  assert.equal(result.manualMergeRequired, true);
  assert.match(
    result.manualMergeBlockers.map((blocker) => blocker.reason).join("\n"),
    /marked RESOLVED with no commit pushed/,
  );
  assert.match(result.manualMergeBlockers[0].remedy, /push the commit/);
  assert.match(result.summary, /^BLOCKED:/);
});

// #3591 review. The two finding blockers do NOT clear the same way, and a single
// blanket instruction is wrong for one of them. `unpushedFixClaims` requires
// `claimedFixed.has(id)` — the thread ALREADY carries a RESOLVED reply — and then
// turns on `lastPushTime <= filedAt`, which another RESOLVED reply does not move.
// Telling the maintainer to reply RESOLVED there is a permanently failing retry
// loop; only a newer commit, or withdrawing the claim with ACCEPTED / [gate-ack],
// clears it.
test("each finding blocker carries the recovery that actually clears it", async () => {
  const unanswered = await evaluateGate({
    author: "detail-app",
    issueComments: [codexVerdict(HEAD_SHA)],
    reviewComments: [codexFinding({ id: 10, line: 32 })],
  });

  assert.match(unanswered.summary, /reply RESOLVED, ACCEPTED or \[gate-ack\]/);

  const unpushed = await evaluateGate({
    author: "detail-app",
    headCommittedDate: "2026-07-09T01:00:00Z",
    issueComments: [codexVerdict(HEAD_SHA)],
    reviewComments: [
      codexFinding({ id: 10, line: 32 }),
      findingReply({ id: 11, inReplyToId: 10, body: "RESOLVED — fixed." }),
    ],
  });

  assert.match(unpushed.summary, /marked RESOLVED with no commit pushed/);
  assert.match(
    unpushed.summary,
    /push the commit/,
    "the remedy must name the only thing that moves lastPushTime",
  );
  assert.match(unpushed.summary, /ACCEPTED/);
  assert.doesNotMatch(
    unpushed.summary,
    /reply RESOLVED, ACCEPTED or \[gate-ack\]/,
    "replying RESOLVED again cannot clear this blocker, so it must not be advertised for it",
  );
});

// The block has to be clearable, or it is a permanent stop rather than a gate.
// The maintainer clears it exactly as on any other PR: a threaded reply carrying
// RESOLVED / ACCEPTED / [gate-ack].
test("answering the finding restores the manual-merge pass for a non-allowed author", async () => {
  const result = await evaluateGate({
    author: "detail-app",
    issueComments: [codexVerdict(HEAD_SHA)],
    reviewComments: [
      codexFinding({ id: 10, line: 32 }),
      findingReply({ id: 11, inReplyToId: 10, body: "ACCEPTED — deliberate, see the PR body." }),
    ],
  });

  assert.equal(result.manualMergeRequired, true);
  assert.deepEqual(result.manualMergeBlockers, []);
  assert.match(result.summary, /^PASS:/);
});

// Scope: this blocks on FINDINGS, not on everything. A missing play-tested label
// and an absent verdict are still notes on the manual path, exactly as before —
// widening the block would have made every external PR unmergeable.
test("a non-allowed author's other unmet requirements stay notes, not blockers", async () => {
  const result = await evaluateGate({
    author: "detail-app",
    files: ["app/termpane.go"],
    issueComments: [],
  });

  assert.equal(result.manualMergeRequired, true);
  assert.deepEqual(result.manualMergeBlockers, []);
  assert.match(result.summary, /^PASS:/);
  assert.match(result.reasons.join("\n"), /missing the play-tested label/);
  assert.match(result.reasons.join("\n"), /Codex has not reviewed head/);
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

// The apply-gate step's inline script, extracted from auto-gate.yml and made
// callable — the same arrangement invalidateGateScript() uses. The merge-race
// concession is wired up in that script, so a test that does not run it proves
// nothing about whether a conceded refusal actually stops reddening the run.
function applyGateScript() {
  const workflow = fs.readFileSync(AUTO_GATE_WORKFLOW, "utf8");
  const step = workflow.match(
    // The `|$` alternative matters: this is the last step of the last job, so
    // the block ends at end-of-file rather than at the next low-indent line.
    /- name: Evaluate, report, and merge the serialized head[\s\S]*?script: \|\n([\s\S]*?)(?=\n {0,10}\S|$)/,
  );
  assert.ok(step, "the apply-gate step script is missing from auto-gate.yml");
  const indent = step[1].match(/^( +)\S/m);
  assert.ok(indent, "the apply-gate step script is empty");
  const body = step[1]
    .split("\n")
    .map((line) => line.slice(indent[1].length))
    .join("\n");
  const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
  return new AsyncFunction("github", "context", "core", "require", "process", body);
}

// runApplyGateStep executes that step against the REAL helper module, so the
// verdict covers the workflow wrapper, the helper's refusal classifier, and the
// step's decision to swallow a conceded error, as one unit.
async function runApplyGateStep({
  github,
  headSha = HEAD_SHA,
  targets = [{ pr_number: 1465, head_sha: HEAD_SHA }],
  mergeEnabled = true,
  readFailure = "",
}) {
  const script = applyGateScript();
  const workspace = "/workspace";
  const helperPath = path.join(workspace, ".github/scripts/auto-gate.js");
  const requireStub = (id) => {
    if (id === "path") {
      return path;
    }
    if (id === helperPath) {
      return autoGate;
    }
    throw new Error(`unexpected require(${JSON.stringify(id)}) in the apply-gate step`);
  };
  const notices = [];
  const core = { ...fakeCore(), notice: (message) => notices.push(message) };
  const env = {
    GITHUB_WORKSPACE: workspace,
    HEAD_SHA: headSha,
    TARGETS_JSON: JSON.stringify(targets),
    MERGE_ENABLED: mergeEnabled ? "true" : "false",
    READ_FAILURE: readFailure,
  };
  let error = null;
  try {
    await script(github, fakeContext(), core, requireStub, { env });
  } catch (stepError) {
    error = stepError;
  }
  return { notices, error };
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
  mergeErrors = [],
  pullGetSnapshots = null,
  pullGetErrors = [],
  checkWriteError = null,
  checkCreateAcceptedErrors = [],
  checkCreateErrors = [],
  checkUpdateErrors = [],
  workflowDispatchError = null,
  workflowDispatchErrorsByWorkflow = {},
  nativeAutoMergeDisableError = null,
  autoMergeStateError = null,
  autoMergeStateHeadByNumber = {},
  autoMergeStateHeadAfterRead = {},
  armNativeAutoMergeOnCheckUpdateFailure = false,
  autoMergeStateErrorAfterRead = 0,
  newerAggregateOnCheckUpdateFailure = false,
  associationError = null,
  associationErrorAtRead = 1,
  associationErrorEveryRead = false,
  headRefName = "siyer/fix-3603",
  // What heads/<headRefName> points at now. null means the ref is gone.
  remoteRefSha = undefined,
  refReadError = null,
  deleteRefError = null,
  // Open PRs based on the head branch.
  dependentPullRequests = [],
  readErrorsByFn = {},
  pullGetError = null,
  requestErrors = [],
} = {}) {
  const listFiles = function listFiles() {};
  const listOpenPullRequests = function listOpenPullRequests() {};
  const listForRef = function listForRef() {};
  const listCommitStatusesForRef = function listCommitStatusesForRef() {};
  const listComments = function listComments() {};
  const listReviews = function listReviews() {};
  const listReviewComments = function listReviewComments() {};
  const listPullRequestsAssociatedWithCommit = function listPullRequestsAssociatedWithCommit() {};
  const merge = async function merge(options) {
    github.mergeAttempts += 1;
    const attemptError = mergeErrors[github.mergeAttempts - 1] || mergeError;
    if (attemptError) {
      throw attemptError;
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
    // created_at matters: generation comparison reads it off the check the
    // transaction owns, and a response without one makes every other check look
    // newer. Same stamp paginate() synthesizes below, so a transaction's own
    // check is not mistaken for a later transaction's.
    return {
      data: { id: 10000 + github.createdChecks.length - 1, created_at: CHECK_CREATED_AT, ...options },
    };
  };
  const updateCheck = async function updateCheck(options) {
    github.checkUpdateAttempts += 1;
    const attemptError = checkUpdateErrors[github.checkUpdateAttempts - 1] || checkWriteError;
    if (attemptError) {
      // Someone arms native auto-merge during the write's backoff — the window a
      // precondition checked once, outside the retry, cannot see.
      if (armNativeAutoMergeOnCheckUpdateFailure) {
        github.nativeAutoMergeArmed = true;
      }
      // A newer Auto Gate event invalidates the head during the write's backoff.
      if (newerAggregateOnCheckUpdateFailure) {
        github.injectedCheckRuns.push({
          id: 99999,
          name: "Auto Gate decision",
          external_id: aggregateExternalId(headSha),
          app: { id: ACTIONS_APP_ID, slug: "github-actions" },
          status: "completed",
          conclusion: "failure",
          created_at: "2026-07-09T09:00:00Z",
          started_at: "2026-07-09T09:00:00Z",
          completed_at: "2026-07-09T09:00:00Z",
          output: { title: "WAITING: refreshing every PR/head decision at this commit" },
        });
      }
      throw attemptError;
    }
    github.operations.push("check:update");
    github.updatedChecks.push(options);
    // An update returns the whole check run, id and original created_at included
    // — not a bare echo of the patch. Generation comparison reads both off it.
    return {
      data: { id: options.check_run_id, created_at: CHECK_CREATED_AT, ...options },
    };
  };
  const responses = new Map([
    [listFiles, files.map((filename) => ({ filename }))],
    [listOpenPullRequests, dependentPullRequests],
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
    pullGetReads: 0,
    requestReads: 0,
    readAttemptsByFn: {},
    checkUpdateAttempts: 0,
    disabledAutoMergePullRequestIds: [],
    dispatchedWorkflows: [],
    mergeAttempts: 0,
    pullGetReads: 0,
    nativeAutoMergeDisableAttempts: 0,
    nativeAutoMergeArmed: nativeAutoMergeEnabled,
    autoMergeStateReads: 0,
    autoMergeStateReadsByNumber: {},
    autoMergeStateBatchSizes: [],
    injectedCheckRuns: [],
    operations: [],
    mergedWith: null,
    refReads: [],
    deletedRefs: [],
    reviewCommentReads: 0,
    reviewCommentReadsByNumber: {},
    createdChecks: [],
    graphqlReadsByNumber: {},
    updatedChecks: [],
    workflowDispatchAttempts: 0,
    rest: {
      actions: {
        createWorkflowDispatch: async (options) => {
          github.workflowDispatchAttempts += 1;
          const perWorkflowError = workflowDispatchErrorsByWorkflow[options.workflow_id];
          if (workflowDispatchError || perWorkflowError) {
            throw workflowDispatchError || perWorkflowError;
          }
          github.operations.push(`dispatch:${options.workflow_id}`);
          github.dispatchedWorkflows.push(options);
        },
      },
      checks: { create: createCheck, listForRef, update: updateCheck },
      git: {
        getRef: async ({ ref }) => {
          github.refReads.push(ref);
          if (refReadError) {
            throw refReadError;
          }
          const sha = remoteRefSha === undefined ? headSha : remoteRefSha;
          if (sha === null) {
            const gone = new Error("Not Found");
            gone.status = 404;
            throw gone;
          }
          return { data: { object: { sha } } };
        },
        deleteRef: async ({ ref }) => {
          github.deletedRefs.push(ref);
          if (deleteRefError) {
            throw deleteRefError;
          }
        },
      },
      issues: { listComments },
      repos: {
        listCommitStatusesForRef,
        listPullRequestsAssociatedWithCommit,
      },
      pulls: {
        list: listOpenPullRequests,
        listFiles,
        listReviews,
        listReviewComments,
        merge,
        // The REST cross-check resolvedPullRequest() uses — deliberately a
        // different code path from the GraphQL read that resolved the PR — and
        // the settling re-read a conceded merge race walks through. Successive
        // reads walk pullGetSnapshots then hold on the last, so a test can say
        // "still open, then merged".
        get: async () => {
          const scriptedError = pullGetError || pullGetErrors[github.pullGetReads];
          const snapshots = pullGetSnapshots || [{ merged, merge_commit_sha: null }];
          const index = Math.min(github.pullGetReads, snapshots.length - 1);
          github.pullGetReads += 1;
          if (scriptedError) {
            throw scriptedError;
          }
          return { data: { number: 1465, ...snapshots[index] } };
        },
      },
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
        if (autoMergeStateError && github.autoMergeStateReads > autoMergeStateErrorAfterRead) {
          throw autoMergeStateError;
        }
        // Aliased per PR, the way the batched read asks for it.
        const repository = {};
        github.autoMergeStateBatchSizes.push(
          Object.keys(variables).filter((key) => /^n\d+$/.test(key)).length,
        );
        for (const [key, number] of Object.entries(variables)) {
          const alias = /^n(\d+)$/.exec(key);
          if (!alias) {
            continue;
          }
          github.autoMergeStateReadsByNumber[number] =
            (github.autoMergeStateReadsByNumber[number] || 0) + 1;
          const reads = github.autoMergeStateReadsByNumber[number];
          const movedAt = autoMergeStateHeadAfterRead[number];
          repository[`pr${alias[1]}`] = {
            number,
            headRefOid:
              movedAt && reads > movedAt.after
                ? movedAt.headSha
                : autoMergeStateHeadByNumber[number] || headSha,
            autoMergeRequest: github.nativeAutoMergeArmed
              ? { enabledAt: "2026-07-09T01:05:00Z" }
              : null,
          };
        }
        return { repository };
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
            headRefName: pullRequestOverride.headRefName ?? headRefName,
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
      const attempt = (github.readAttemptsByFn[fn.name] || 0) + 1;
      github.readAttemptsByFn[fn.name] = attempt;
      const scriptedError = (readErrorsByFn[fn.name] || [])[attempt - 1];
      if (scriptedError) {
        throw scriptedError;
      }
      const number = options.pull_number || options.issue_number;
      const pullRequestOverride = pullRequestsByNumber[number] || {};
      if (fn === listForRef) {
        github.checkListReads += 1;
        return [
          ...checkRuns,
          ...github.injectedCheckRuns,
          ...github.createdChecks.map((created, index) => ({
            id: 10000 + index,
            app: { id: ACTIONS_APP_ID, slug: "github-actions" },
            created_at: CHECK_CREATED_AT,
            started_at: CHECK_CREATED_AT,
            completed_at: CHECK_CREATED_AT,
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
      github.requestReads += 1;
      const scriptedError = requestErrors[github.requestReads - 1];
      if (scriptedError) {
        throw scriptedError;
      }
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

// ---------------------------------------------------------------------------
// Workflow trigger scan.
//
// Lexical, not a parser. Six review rounds of reading these triggers as YAML by
// hand each turned up another valid spelling — shorthand, flow mappings, deeper
// indentation, comments in four positions, an unclosed flow sequence — and every
// one failed identically: it read as "no trigger here" rather than "cannot read
// this", so the workflow dropped out of the comparison AND out of
// MASTER_PUSH_WORKFLOWS at the same moment, silently. The repo has no YAML
// library (no package.json; web-selftest-scope.test.js reads its workflow
// textually too), so the answer was to stop needing one.
//
// The question is now "does this file's trigger section mention a push
// trigger?", which no formatting can change the answer to. It over-includes a
// push trigger scoped to a branch other than master; that is a visible test
// failure resolved with an explicit exception below, which is the direction that
// cannot cost master an unverified commit.
// ---------------------------------------------------------------------------

// Workflows whose trigger section mentions push but which genuinely cannot run
// on a master push. Empty today; an entry needs a reason, because adding one
// means the gate will NOT re-raise that workflow after a merge.
const PUSH_TRIGGER_EXCEPTIONS = {
  // "name.yml": { reason: "why its push trigger cannot reach master",
  //               triggers: "<the on: section this reason was written against>" },
};

// An exception holds only while the triggers it was justified against are
// unchanged. A name-keyed exemption is permanent, so a workflow later widened to
// include master would stay exempt and its commits unverified — the exemption
// outliving the reason for it.
function exemptedByRecordedTriggers(name, section) {
  const exception = PUSH_TRIGGER_EXCEPTIONS[name];
  if (!exception) {
    return false;
  }
  return normalizeTriggers(section) === normalizeTriggers(exception.triggers);
}

function normalizeTriggers(section) {
  return withoutComments(section || "")
    .split("\n")
    .map((line) => line.trimEnd())
    .filter((line) => line.trim().length > 0)
    .join("\n");
}

// The dispatch inputs merge() sends, and what each one's declaration must still
// accept. A rename, a removal, or a type change on any of these is a 422 after
// the commit has already landed.
const SUPPLIED_DISPATCH_INPUTS = {
  "docs.yml": { verify_sha: { type: "string" } },
};

// A declaration's DIRECT properties: the lines one level under it, excluding
// anything nested deeper. A regex over the whole block is satisfied by prose —
// `description: 'Expected type: string SHA'` inside the same declaration reads
// as the type itself — so the value has to come from the property, not from the
// text containing it.
function directProperties(block) {
  const [, ...rest] = block.split("\n");
  const content = rest.filter((line) => line.trim().length > 0 && !line.trim().startsWith("#"));
  if (content.length === 0) {
    return {};
  }
  const indent = Math.min(...content.map((line) => line.match(/^ */)[0].length));
  const properties = {};
  for (const line of content) {
    if (line.match(/^ */)[0].length !== indent) {
      continue;
    }
    const property = /^ *(?<key>[A-Za-z0-9_-]+):[ \t]*(?<value>.*)$/.exec(line);
    if (property) {
      properties[property.groups.key] = property.groups.value.trim().replace(/^["']|["']$/g, "");
    }
  }
  return properties;
}

// A declaration and everything indented under it: the matched line plus every
// following line indented deeper. One bounded extraction so an expectation
// cannot drift into a LATER input's properties — a lazy match across the whole
// section would accept `verify_sha: boolean` as long as some other input was a
// string.
function declarationBlock(text, pattern) {
  const lines = text.split("\n");
  const start = lines.findIndex((line) => pattern.test(line));
  if (start < 0) {
    return null;
  }
  const indent = lines[start].match(/^ */)[0].length;
  const block = [lines[start]];
  for (const line of lines.slice(start + 1)) {
    if (line.trim().length === 0) {
      block.push(line);
      continue;
    }
    if (line.match(/^ */)[0].length <= indent) {
      break;
    }
    block.push(line);
  }
  return block.join("\n");
}

// Whether a condition lets a dispatched run through UNCONDITIONALLY. Naming the
// event is not enough in two different ways: `!= 'workflow_dispatch'` names it in
// order to exclude it, and `… && inputs.run_checks` admits it only when an input
// the gate does not supply happens to be set — the dispatch then succeeds and
// the job it was raised for is skipped anyway.
function admitsDispatch(condition) {
  // Unconditional reachability, checked structurally rather than by listing the
  // contexts that could gate it. Enumerating `inputs.` was the wrong shape:
  // `vars.`, `env.`, `secrets.`, `needs.` and anything GitHub adds later gate a
  // dispatch branch just as well. So the requirement is positive — one top-level
  // disjunct must be the dispatch test AND NOTHING ELSE.
  return topLevelDisjuncts(condition).some((disjunct) =>
    /^github\.event_name\s*==\s*'workflow_dispatch'$|^'workflow_dispatch'\s*==\s*github\.event_name$/.test(
      disjunct,
    ),
  );
}

// A condition's top-level `||` operands, with wrapping parentheses stripped.
// Depth-aware so `(a || b) && c` is one operand, not two — reading it as two
// would see a bare dispatch test inside a conjunction that can still exclude it.
function topLevelDisjuncts(condition) {
  const operands = [];
  let depth = 0;
  let current = "";
  for (let index = 0; index < condition.length; index += 1) {
    const character = condition[index];
    if (character === "(") {
      depth += 1;
    } else if (character === ")") {
      depth -= 1;
    }
    if (depth === 0 && character === "|" && condition[index + 1] === "|") {
      operands.push(current);
      current = "";
      index += 1;
      continue;
    }
    current += character;
  }
  operands.push(current);
  return operands.map((operand) => {
    let text = operand.trim();
    while (text.startsWith("(") && text.endsWith(")")) {
      text = text.slice(1, -1).trim();
    }
    return text.replace(/\s+/g, " ");
  });
}

// Whether a condition consults the event at all — its name or its payload, in
// dotted or indexed form. `github['event']['head_commit']` is the same predicate
// as `github.event.head_commit`, and a scan that reads only the dotted spelling
// answers "this condition does not depend on the event".
function consultsEvent(condition) {
  return /github\s*(?:\.\s*event(?:_name|\s*\.)|\[\s*['"]event)/.test(condition);
}

// A YAML alias or merge key in the trigger section — `on: *push_event`,
// `<<: *base`. GitHub resolves them; this scan cannot, and an unresolved alias
// reads as "no push trigger", which is the silent direction. Anything aliased is
// reported unreadable so it fails by name instead.
function usesUnresolvableYaml(section) {
  const triggers = withoutComments(section || "");
  return /(?:^|[:\-[,{])\s*\*[A-Za-z_]/m.test(triggers) || /^\s*<<\s*:/m.test(triggers);
}

// The `on:` section: from the `on:` line to the next real top-level key. Comment
// and blank lines stay inside it, since a standalone comment between events is
// valid YAML and truncating there loses whatever follows.
function onSection(text) {
  const lines = text.split("\n");
  const start = lines.findIndex((line) => /^on:/.test(line));
  if (start < 0) {
    return null;
  }
  const section = [lines[start]];
  for (const line of lines.slice(start + 1)) {
    if (/^[A-Za-z_]/.test(line)) {
      break;
    }
    section.push(line);
  }
  return section.join("\n");
}

// Comments removed, so prose about push in a trigger section cannot be mistaken
// for a trigger. Quoted `#` in a workflow trigger section does not occur, and a
// false strip would only over-include, which fails visibly.
function withoutComments(text) {
  return text
    .split("\n")
    .map((line) => line.replace(/(^|\s)#.*$/, "$1"))
    .join("\n");
}

// Whether the trigger section mentions a push trigger, in any spelling.
function mentionsPushTrigger(section) {
  if (section == null) {
    return false;
  }
  const triggers = withoutComments(section);
  // `on: push`, `on: [push, …]`, `  push:` and `"push":` all contain the token;
  // `pull_request` and `workflow_dispatch` do not, and a job named push lives
  // outside this section.
  // The closing quote is optional on BOTH sides: `on: 'push'` and `on: ['push']`
  // are as valid as their bare forms, and a scan that requires the token to end
  // the line answers "no push trigger" for them.
  return /(?:^|[\s[{,"'])push["']?\s*(?::|,|\]|\}|$)/m.test(triggers);
}

// The exact wire shape from Auto Gate run 32044471684: GET
// /repos/:o/:r/pulls/3395/reviews answered HTTP 404 with a GraphQL NOT_FOUND
// body naming the PR's OWN node id, during a wider GitHub degradation.
function selfContradictoryNotFound(nodeId = "PR_node_1465") {
  const body = {
    type: "NOT_FOUND",
    path: ["node"],
    message: `Could not resolve to a node with the global id of '${nodeId}'.`,
  };
  const error = new Error(`Not Found: ${JSON.stringify(body)}`);
  error.status = 404;
  error.response = { status: 404, data: body };
  return error;
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

// Codex's automatic-review artifact, captured verbatim from a real summary
// comment (PR #3550, comment 5500591835) with only the commit, row timestamp and
// trigger substituted. An automatic review edits this table and emits no
// "Reviewed commit:" prose line at all, which is the whole of #3606.
function codexSummaryTable(
  sha,
  {
    status = "✅ **Completed**",
    rowTime = "2026-07-09T01:20:00Z",
    trigger = "New commits",
    commentTime = "2026-07-09T01:20:00Z",
    commitCell = null,
  } = {},
) {
  const cell = commitCell ?? `\`${sha.slice(0, 7)}\``;
  const time = rowTime
    ? ` <relative-time datetime="${rowTime}">${rowTime}</relative-time>`
    : "";
  return {
    user: { login: "chatgpt-codex-connector[bot]" },
    body: [
      "<!-- codex-pull-request-review-summary -->",
      "",
      "## Codex Review Summary",
      "",
      "This comment shows the latest Codex review activity on this pull request.",
      "",
      "| Review | Status | Commit | Review trigger |",
      "| --- | --- | --- | --- |",
      `| 📝 **Code Review** | ${status}${time} | ${cell} | ${trigger} |`,
      "",
      "",
      "",
      "<details> <summary>ℹ️ About Codex in GitHub</summary>",
      "<br/>",
      "",
      "[Your team has set up Codex to review pull requests in this repo](https://chatgpt.com/codex/cloud/settings/general). Reviews are triggered when you",
      "- Open a pull request for review",
      "- Mark a draft as ready",
      '- Comment "@codex review" or "@codex security review".',
      "",
      "</details>",
    ].join("\n"),
    created_at: commentTime,
    updated_at: commentTime,
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
