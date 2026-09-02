const { randomUUID } = require("node:crypto");

const ALLOWED_AUTHORS = new Set(["sachiniyer", "app-detail-app", "app-detail-app[bot]"]);
const TUI_PATH_PREFIXES = ["app/", "ui/", "session/tmux/"];
const DOCS_DEPLOY_PATHS = ["docs/", "mkdocs.yml"];
// GitHub check runs live on commits, but the underlying gate evidence is
// PR-scoped. The full (PR, head) pair is therefore part of every composite
// decision identifier. AUTO_GATE_DECISION_CHECK is the fixed-name aggregate
// required by the master ruleset: it passes only when every open master PR at
// the commit has its own passing composite decision.
const AUTO_GATE_DECISION_CHECK = "Auto Gate decision";
const AUTO_GATE_AGGREGATE_EXTERNAL_ID_PREFIX = "auto-gate:aggregate:head:";
const GITHUB_ACTIONS_APP_ID = 15368;
const CODEX_REVIEWER = "chatgpt-codex-connector[bot]";
const CODEX_REVIEW_RE = /\bCodex Review\b/i;
const CODEX_RATE_LIMIT_RE = /reached your Codex usage limits for code reviews/i;
const CODEX_BODY_FINDING_RE = /\bP[0-3]\b/i;
const REVIEWED_COMMIT_RE = /(?:\*\*Reviewed commit:\*\*|Reviewed commit:)\s*`([0-9a-f]{7,40})`/i;
// Docs/Deploy is deliberately conditional and is skipped on pull_request runs.
const ALLOWED_SKIPPED_CHECKS = new Set(["Deploy"]);
const RESOLUTION_MARKER_RE = /\b(?:RESOLVED|ACCEPTED)\b/;
// RESOLVED claims a code change was made; ACCEPTED and [gate-ack] claim none is
// owed. The distinction is load-bearing: only the first implies a commit must
// exist, so only the first is checked against the head (#2878).
const FIX_CLAIM_RE = /\bRESOLVED\b/;
const NO_CHANGE_CLAIM_RE = /\bACCEPTED\b/;
const MANUAL_MERGE_AUTHOR_REASON =
  "Auto Gate does not auto-merge PRs from this author; a maintainer must review and " +
  "merge manually.";
// Read by a human deciding whether to merge, so it has to be impossible to
// mistake for an approval: it names the degradation and says outright that no
// review happened.
const MANUAL_MERGE_REVIEWER_UNAVAILABLE_REASON =
  "The Codex reviewer is usage-limited, so no verdict for this head can arrive; Auto Gate " +
  "degraded to maintainer review. This PR has NOT been reviewed — a maintainer must review " +
  "and merge it manually.";
const RETRY_DELAYS_MS = [250, 1000];
const MAX_RATE_LIMIT_DELAY_MS = 10000;

// GitHub reads are side-effect-free, and check-run updates are idempotent when
// replayed by check_run_id. Check-run creates are different: external_id is
// correlation metadata, not a uniqueness key, so an ambiguous create is issued
// once and then reconciled by a per-attempt marker instead of being replayed.
// This is especially important for aggregate invalidation, which closes a
// stale-green safety window. Squash merge, workflow dispatch, and GraphQL
// mutations are also non-idempotent; keep those calls single-shot and never
// route them through these helpers.
async function retryRead(label, operation) {
  return retryTransient(label, operation, {
    failureName: "AutoGateReadError",
    readFailure: true,
  });
}

async function retryCheckUpdate(label, operation) {
  return retryTransient(label, operation, {
    failureName: "AutoGateCheckWriteError",
    readFailure: false,
  });
}

async function createCheckRun({
  github,
  owner,
  repo,
  headSha,
  name,
  externalId,
  decision,
  label,
  beforeWrite,
}) {
  const marker = `<!-- auto-gate-check-create:${randomUUID()} -->`;
  const markedDecision = {
    ...decision,
    output: {
      ...decision.output,
      text: [decision.output.text, marker].filter(Boolean).join("\n\n"),
    },
  };
  for (let attempt = 0; ; attempt += 1) {
    try {
      // Re-established on every attempt, not once before the loop: a retried
      // create sleeps between attempts, and a precondition checked before the
      // loop is stale for each one after the first.
      if (typeof beforeWrite === "function") {
        await beforeWrite();
      }
      return await github.rest.checks.create({
        owner,
        repo,
        head_sha: headSha,
        name,
        external_id: externalId,
        ...markedDecision,
      });
    } catch (error) {
      // An explicit rate-limit response rejected the request, so no check was
      // created and replay is safe. Transport failures remain ambiguous and
      // must reconcile the marker without issuing another POST.
      if (isDefinitiveRateLimitResponse(error)) {
        if (attempt >= RETRY_DELAYS_MS.length) {
          throw retryFailure(label, attempt + 1, error, "AutoGateCheckWriteError", false);
        }
        await delay(retryDelayMilliseconds(error, RETRY_DELAYS_MS[attempt]));
        continue;
      }
      if (!isRetryableGitHubError(error)) {
        throw error;
      }
      return retryTransient(
        `${label} after ambiguous create failure (${error.message || String(error)})`,
        async () => {
          const checkRuns = await github.paginate(github.rest.checks.listForRef, {
            owner,
            repo,
            ref: headSha,
            per_page: 100,
          });
          const created = checkRuns
            .filter(
              (run) =>
                run.name === name &&
                run.external_id === externalId &&
                run.app?.id === GITHUB_ACTIONS_APP_ID &&
                run.output?.text?.includes(marker),
            )
            .sort((left, right) => latestRunTime(right) - latestRunTime(left))[0];
          if (!created) {
            const missing = new Error("the matching check run is not visible yet");
            missing.status = 503;
            throw missing;
          }
          return { data: created };
        },
        { failureName: "AutoGateCheckWriteError", readFailure: false },
      );
    }
  }
}

async function retryTransient(label, operation, { failureName, readFailure }) {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await operation();
    } catch (error) {
      if (!isRetryableGitHubError(error)) {
        throw error;
      }
      if (attempt >= RETRY_DELAYS_MS.length) {
        throw retryFailure(label, attempt + 1, error, failureName, readFailure);
      }
      await delay(retryDelayMilliseconds(error, RETRY_DELAYS_MS[attempt]));
    }
  }
}

function retryFailure(label, attempts, error, failureName, readFailure) {
  const failure = new Error(`${label} after ${attempts} attempts: ${error.message || String(error)}`);
  failure.name = failureName;
  failure.autoGateReadFailure = readFailure;
  failure.status = error.status;
  failure.cause = error;
  return failure;
}

function isRetryableGitHubError(error) {
  const status = Number(error?.status);
  if (Number.isFinite(status)) {
    return status === 408 || status === 429 || status >= 500 || isRateLimitError(error);
  }
  if (isRetryableGraphQLError(error)) {
    return true;
  }
  const detail = `${error?.code || ""} ${error?.name || ""} ${error?.message || ""}`;
  return /fetch failed|network|socket|ECONNRESET|ETIMEDOUT|EAI_AGAIN/i.test(detail);
}

function isRetryableGraphQLError(error) {
  const errors = graphQLResponseErrors(error);
  return errors.length > 0 && errors.every((responseError) => {
    const message = responseError?.message || "";
    return (
      /^Something went wrong while executing your query\b/i.test(message) ||
      isGraphQLRateLimitError(responseError)
    );
  });
}

function graphQLResponseErrors(error) {
  if (error?.name !== "GraphqlResponseError") {
    return [];
  }
  const errors = error.errors || error.response?.errors;
  return Array.isArray(errors) ? errors : [];
}

function isGraphQLRateLimitError(error) {
  return (
    String(error?.type || "").toUpperCase() === "RATE_LIMITED" ||
    /(?:API|secondary) rate limit|rate limit exceeded|abuse detection/i.test(error?.message || "")
  );
}

function isRateLimitError(error) {
  const status = Number(error?.status);
  if (status === 429) {
    return true;
  }
  if (!Number.isFinite(status)) {
    const errors = graphQLResponseErrors(error);
    return errors.length > 0 && errors.every(isGraphQLRateLimitError);
  }
  if (status !== 403) {
    return false;
  }
  const headers = githubErrorHeaders(error);
  return (
    headers["x-ratelimit-remaining"] === "0" ||
    headers["retry-after"] !== undefined ||
    /(?:API|secondary) rate limit|abuse detection/i.test(error?.message || "")
  );
}

function isDefinitiveRateLimitResponse(error) {
  return Boolean(error?.response) && isRateLimitError(error);
}

function retryDelayMilliseconds(error, fallback) {
  if (!isRateLimitError(error)) {
    return fallback;
  }
  const headers = githubErrorHeaders(error);
  const retryAfterSeconds = Number(headers["retry-after"]);
  const resetSeconds = Number(headers["x-ratelimit-reset"]);
  const requestedDelay = Number.isFinite(retryAfterSeconds) && retryAfterSeconds >= 0
    ? retryAfterSeconds * 1000
    : Number.isFinite(resetSeconds)
      ? Math.max(0, resetSeconds * 1000 - Date.now())
      : fallback;
  // Honor GitHub's throttle timing without letting a bounded retry hold the
  // serialized merge lane indefinitely. Exhaustion still fails closed.
  return Math.min(Math.max(fallback, requestedDelay), MAX_RATE_LIMIT_DELAY_MS);
}

function githubErrorHeaders(error) {
  const headers = error?.response?.headers || error?.headers || {};
  return Object.fromEntries(
    Object.entries(headers).map(([name, value]) => [name.toLowerCase(), String(value)]),
  );
}

function isReadFailure(error) {
  return error?.autoGateReadFailure === true;
}

function delay(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

async function evaluate({ github, context, core, prNumber, setOutputs = true }) {
  try {
    return await evaluatePullRequest({ github, context, core, prNumber, setOutputs });
  } catch (error) {
    const message = formatError(error);
    const warning = isReadFailure(error) ? message : error?.stack || message;
    core.warning(warning);
    return finish(core, setOutputs, {
      prNumber: prNumber ? String(prNumber) : "",
      shouldMerge: false,
      isOpen: false,
      docsChanged: false,
      readFailure: isReadFailure(error),
      reasons: [`auto-gate evaluation error: ${message}`],
      notes: [],
    });
  }
}

async function evaluatePullRequest({ github, context, core, prNumber, setOutputs = true }) {
  const number = prNumber || (await findPullRequestNumber({ github, context, core }));

  if (!number) {
    return finish(core, setOutputs, {
      prNumber: "",
      shouldMerge: false,
      isOpen: false,
      docsChanged: false,
      reasons: ["No open pull request found for this event."],
      notes: [],
    });
  }

  const pr = await getPullRequest({ github, context, number });
  const reasons = [];
  const notes = [];
  // Every cause that makes this PR maintainer-merged rather than auto-merged.
  // Each one is a full sentence because the decision summary is where a human
  // finds out why the gate stopped short of merging.
  const manualMergeReasons = [];
  if (!ALLOWED_AUTHORS.has(pr.author)) {
    manualMergeReasons.push(MANUAL_MERGE_AUTHOR_REASON);
  }

  core.info(`Evaluating auto-gate for PR #${pr.number}: ${pr.title}`);
  core.info(`PR URL: ${pr.url}`);
  core.info(`Author: ${pr.author || "(unknown)"}`);
  core.info(`Base: ${pr.baseRefName}; head SHA: ${pr.headRefOid}`);
  core.info(`Mergeable: ${pr.mergeable}; merge state: ${pr.mergeStateStatus}`);

  if (pr.state !== "OPEN" || pr.merged) {
    reasons.push(`PR is ${pr.merged ? "already merged" : pr.state.toLowerCase()}, not open`);
  }

  if (pr.baseRefName !== "master") {
    reasons.push(`base branch is ${pr.baseRefName}, not master`);
  }

  if (pr.isDraft) {
    reasons.push("PR is a draft");
  }

  const baseRepository = `${context.repo.owner}/${context.repo.repo}`;
  if (pr.headRepository !== baseRepository) {
    reasons.push(
      `head repository ${pr.headRepository || "(unknown)"} is not ${baseRepository}; ` +
        "Auto Gate requires a base-repository branch",
    );
  }

  if (pr.mergeable === "CONFLICTING" || pr.mergeStateStatus === "DIRTY") {
    reasons.push(`mergeability is blocked (${pr.mergeable}/${pr.mergeStateStatus})`);
  } else if (pr.mergeable !== "MERGEABLE") {
    reasons.push(`mergeability is still ${pr.mergeable}`);
  }

  const files = await listPullRequestFiles({ github, context, number: pr.number });
  const touchesTui = files.some((path) => TUI_PATH_PREFIXES.some((prefix) => path.startsWith(prefix)));
  const docsChanged = files.some((path) =>
    DOCS_DEPLOY_PATHS.some((docsPath) => path === docsPath || path.startsWith(docsPath)),
  );
  const labels = new Set(pr.labels.map((label) => label.toLowerCase()));

  if (touchesTui && !labels.has("play-tested")) {
    reasons.push("PR touches visible TUI/pane paths and is missing the play-tested label");
  } else if (touchesTui) {
    notes.push("TUI path gate passed with play-tested label");
  } else {
    notes.push("TUI path gate not required");
  }

  if (docsChanged) {
    notes.push("Docs deploy dispatch required after merge");
  }

  const requiredChecks = await evaluateRequiredChecks({
    github,
    context,
    branch: pr.baseRefName,
    sha: pr.headRefOid,
    core,
  });
  if (!requiredChecks.ok) {
    reasons.push(...requiredChecks.reasons);
  }
  notes.push(...requiredChecks.notes);

  const codex = await evaluateCodex({
    github,
    context,
    number: pr.number,
    sha: pr.headRefOid,
    lastCommitDate: pr.lastCommitDate,
  });
  if (!codex.ok) {
    reasons.push(...codex.reasons);
  }
  notes.push(...codex.notes);

  // A reviewer that is out of quota cannot ever produce the verdict this gate
  // waits for, so waiting is not caution — it is a permanent stop on the whole
  // repository (#3378). Degrade to the maintainer-merge path that already
  // exists for non-auto-merge authors: the decision check passes so branch
  // protection does not sit red, and nothing merges automatically. This is
  // reached ONLY on observed evidence — the reviewer's own usage-limit message
  // on its latest comment. Silence stays blocking, because an absent verdict
  // with no explanation is unknown, not proven-unavailable.
  //
  // It waives exactly ONE requirement: the verdict that cannot arrive. Every
  // other gate — unresolved findings, the play-tested label, required checks,
  // mergeability — is independent of the reviewer's quota, and since
  // manualMergeRequired makes the decision pass, waiving them alongside it
  // would let "the reviewer is down" green-light a PR with a known finding.
  const otherBlockers = codex.reviewerUnavailable
    ? reasons.filter((reason) => reason !== codex.reviewerUnavailableReason)
    : reasons;
  const degradedForUnavailableReviewer =
    Boolean(codex.reviewerUnavailable) && otherBlockers.length === 0;
  if (degradedForUnavailableReviewer) {
    manualMergeReasons.push(MANUAL_MERGE_REVIEWER_UNAVAILABLE_REASON);
  }
  const manualMergeRequired = manualMergeReasons.length > 0;

  return finish(core, setOutputs, {
    prNumber: String(pr.number),
    pullRequestId: pr.id,
    shouldMerge: !manualMergeRequired && reasons.length === 0,
    manualMergeRequired,
    manualMergeReasons,
    degradedForUnavailableReviewer,
    isOpen: pr.state === "OPEN" && !pr.merged,
    baseRefName: pr.baseRefName,
    headSha: pr.headRefOid,
    docsChanged,
    reasons,
    notes,
  });
}

async function reportDecision({ github, context, core, result, manual = false }) {
  if (!result?.headSha || !result.prNumber) {
    core.info("Auto Gate decision was not reported because no pull-request head was resolved.");
    return { state: "unreported", priorDecision: false };
  }
  if (result.isOpen === false) {
    core.notice(`Leaving the existing Auto Gate decision unchanged for closed PR #${result.prNumber}.`);
    return { state: "closed", priorDecision: false };
  }
  if (result.baseRefName && result.baseRefName !== "master") {
    core.notice(
      `Auto Gate decision was not reported for PR #${result.prNumber} because its base is ` +
        `${result.baseRefName}, not master.`,
    );
    return { state: "ineligible", priorDecision: false };
  }

  const identity = decisionIdentity(result.prNumber, result.headSha);
  const { owner, repo } = context.repo;
  const checkRuns = await retryRead(
    `could not read check runs for PR #${result.prNumber} at ${result.headSha}`,
    () =>
      github.paginate(github.rest.checks.listForRef, {
        owner,
        repo,
        ref: result.headSha,
        per_page: 100,
      }),
  );
  const priorDecision = checkRuns
    .filter(
      (run) =>
        run.name === identity.checkName &&
        run.external_id === identity.externalId &&
        run.app?.id === GITHUB_ACTIONS_APP_ID,
    )
    .sort((left, right) => latestRunTime(right) - latestRunTime(left))[0];
  const decisionPasses = result.shouldMerge || result.manualMergeRequired;
  const state =
    manual && !priorDecision
      ? "never-ran"
      : result.manualMergeRequired
        ? "manual"
        : result.shouldMerge
          ? "pass"
          : "waiting";
  const title =
    state === "never-ran"
      ? `NEVER_RAN: no prior decision; recovery ${decisionPasses ? "passed" : "is waiting"}`
      : result.manualMergeRequired
        ? result.degradedForUnavailableReviewer
          ? "PASS: reviewer usage-limited; maintainer review and manual merge required"
          : "PASS: maintainer review and manual merge required"
        : result.shouldMerge
          ? "PASS: Auto Gate requirements are satisfied"
          : "WAITING: Auto Gate requirements are not yet satisfied";

  const decision = {
    status: "completed",
    conclusion: decisionPasses ? "success" : "failure",
    output: {
      title,
      summary: result.summary,
    },
  };
  try {
    if (priorDecision) {
      await retryCheckUpdate(`could not update Auto Gate decision for PR #${result.prNumber}`, () =>
        github.rest.checks.update({
          owner,
          repo,
          check_run_id: priorDecision.id,
          ...decision,
        }),
      );
    } else {
      await createCheckRun({
        github,
        owner,
        repo,
        headSha: result.headSha,
        name: identity.checkName,
        externalId: identity.externalId,
        decision,
        label: `could not create Auto Gate decision for PR #${result.prNumber}`,
      });
    }
  } catch (error) {
    // pull_request-family events from forks can receive a read-only GITHUB_TOKEN
    // even though this workflow requests checks: write. Leave absence observable
    // and recoverable by a base-repository workflow_dispatch; other write errors
    // remain fatal so a repository permission regression cannot pass silently.
    if (!isReadOnlyForkCheckError(error, context)) {
      throw error;
    }
    core.warning(
      `Auto Gate could not publish the decision for fork PR #${result.prNumber} with its ` +
        "read-only token; run workflow_dispatch from the base repository to recover it.",
    );
    return { state: "read-only", priorDecision: Boolean(priorDecision) };
  }
  core.notice(`${title} for PR #${result.prNumber}.`);
  return { state, priorDecision: Boolean(priorDecision) };
}

function resolveAggregateHeads({ context, targets = [] }) {
  const payload = context.payload || {};
  const candidates = [
    payload.pull_request?.head?.sha,
    payload.after,
    ...targets.map((target) => target.headSha),
    payload.check_suite?.head_sha,
    payload.workflow_run?.head_sha,
    payload.sha,
    payload.before,
  ];
  return [...new Set(candidates.map(normalizeHeadSha).filter(Boolean))];
}

async function invalidateAggregateDecision({ github, context, core, headSha }) {
  const sha = normalizeHeadSha(headSha);
  if (!sha) {
    throw new Error(`Invalid head SHA for Auto Gate aggregate invalidation: ${headSha}`);
  }
  const decision = {
    // Create a new failure before any API-dependent reads. If target resolution,
    // association lookup, or evaluation fails, the newest fixed check is already
    // non-green and the prior PASS cannot remain authoritative.
    status: "completed",
    conclusion: "failure",
    output: {
      title: "WAITING: refreshing every PR/head decision at this commit",
      summary:
        "This fixed-name check is commit-scoped. Auto Gate is refreshing every open " +
        "master PR and exact (PR, head) decision at this commit.",
    },
  };
  const write = await createAggregateCheck({
    github,
    context,
    core,
    headSha: sha,
    decision,
  });
  const state = write.writeState === "read-only" ? "read-only" : "pending";
  if (state === "pending") {
    core.notice(`Marked the fixed Auto Gate aggregate non-green on ${sha}.`);
  }
  return {
    ok: false,
    headSha: sha,
    pullNumbers: [],
    blockers: ["The fixed aggregate is waiting for a fresh evaluation"],
    checkRuns: [],
    summary: decision.output.summary,
    ...write,
    state,
  };
}

async function blockAggregateEvaluation({
  github,
  context,
  core,
  headSha,
  checkRunId,
  reason,
}) {
  const sha = normalizeHeadSha(headSha);
  if (!sha) {
    throw new Error(`Invalid head SHA for blocked Auto Gate aggregate: ${headSha}`);
  }
  const detail = String(reason || "required GitHub read failed").replace(/^BLOCKED:\s*/i, "");
  const summary =
    `${detail}. Auto Gate did not infer an empty PR set or any PR state from the failed read; ` +
    "this commit remains blocked until a complete evaluation succeeds.";
  const decision = {
    status: "completed",
    conclusion: "failure",
    output: {
      title: "BLOCKED: Auto Gate could not complete a required GitHub read",
      summary,
    },
  };
  const write = await upsertAggregateCheck({
    github,
    context,
    core,
    headSha: sha,
    checkRuns: [],
    decision,
    checkRunId,
  });
  core.notice(`BLOCKED: ${detail}`);
  return {
    ok: false,
    headSha: sha,
    pullNumbers: [],
    blockers: [detail],
    checkRuns: [],
    summary,
    ...write,
    checkRunId,
    state: "evaluation-error",
  };
}

async function beginAggregateDecision({ github, context, core, headSha }) {
  const invalidated = await invalidateAggregateDecision({
    github,
    context,
    core,
    headSha,
  });
  if (invalidated.writeState === "read-only") {
    return invalidated;
  }
  const sha = invalidated.headSha;
  let aggregate;
  try {
    aggregate = await evaluateAggregateDecision({ github, context, headSha: sha });
  } catch (error) {
    if (!isReadFailure(error)) {
      throw error;
    }
    return blockAggregateEvaluation({
      github,
      context,
      core,
      headSha: sha,
      checkRunId: invalidated.checkRunId,
      reason: formatError(error),
    });
  }
  return {
    ...aggregate,
    writeState: invalidated.writeState,
    priorAggregate: invalidated.priorAggregate,
    checkRunId: invalidated.checkRunId,
    state: "pending",
  };
}

async function reportAggregateDecision({
  github,
  context,
  core,
  headSha,
  checkRunId,
  beforePublish,
}) {
  let aggregate;
  try {
    aggregate = await evaluateAggregateDecision({ github, context, headSha });
  } catch (error) {
    if (!isReadFailure(error)) {
      throw error;
    }
    return blockAggregateEvaluation({
      github,
      context,
      core,
      headSha,
      checkRunId,
      reason: formatError(error),
    });
  }
  if (checkRunId) {
    const identity = aggregateIdentity(aggregate.headSha);
    const latestGeneration = newestCheckGeneration(
      aggregate.checkRuns.filter(
        (run) =>
          run.name === identity.checkName &&
          run.external_id === identity.externalId &&
          run.app?.id === GITHUB_ACTIONS_APP_ID,
      ),
    );
    if (latestGeneration?.id !== checkRunId) {
      const blocker =
        "A newer Auto Gate event invalidated this commit while the serialized refresh was running";
      core.notice(`${blocker}; this older transaction will not publish PASS.`);
      return {
        ...aggregate,
        ok: false,
        blockers: [...aggregate.blockers, blocker],
        summary: formatAggregateSummary(
          aggregate.pullNumbers,
          [...aggregate.blockers, blocker],
          false,
        ),
        writeState: "superseded",
        checkRunId,
        state: "superseded",
      };
    }
  }
  const decision = {
    status: "completed",
    conclusion: aggregate.ok ? "success" : "failure",
    output: {
      title: aggregate.ok
        ? "PASS: every open master PR at this commit passes Auto Gate"
        : `BLOCKED: ${aggregate.blockers.length} associated PR decision(s) are not passing`,
      summary: aggregate.summary,
    },
  };
  const write = await upsertAggregateCheck({
    github,
    context,
    core,
    headSha: aggregate.headSha,
    checkRuns: aggregate.checkRuns,
    decision,
    checkRunId,
    // Handed down rather than run here, and deliberately AFTER the association
    // and check-run reads above: those are round trips too, and a guard that
    // runs before them is stale by the time the green is published. Only a PASS
    // carries it — a red publish authorizes nothing.
    beforePublish: aggregate.ok ? beforePublish : undefined,
  });
  core.notice(`${aggregate.ok ? "PASS" : "BLOCKED"}: fixed Auto Gate aggregate on ${aggregate.headSha}.`);
  return { ...aggregate, ...write, state: aggregate.ok ? "pass" : "waiting" };
}

async function processAggregateHead({
  github,
  context,
  core,
  headSha,
  targets = [],
  mergeEnabled = false,
  manual = false,
  readFailureReason = "",
}) {
  // Target resolution happens before the serialized head lane. If that read
  // exhausted its retries, publish its failure without performing a second
  // evaluation that could silently turn the same run green or merge the PR.
  if (readFailureReason) {
    const invalidated = await invalidateAggregateDecision({
      github,
      context,
      core,
      headSha,
    });
    if (invalidated.writeState === "read-only") {
      return { state: "read-only", pending: invalidated };
    }
    const aggregate = await blockAggregateEvaluation({
      github,
      context,
      core,
      headSha: invalidated.headSha,
      checkRunId: invalidated.checkRunId,
      reason: readFailureReason,
    });
    return { state: "evaluation-error", pending: invalidated, aggregate };
  }

  const pending = await beginAggregateDecision({ github, context, core, headSha });
  if (pending.writeState === "read-only") {
    return { state: "read-only", pending };
  }
  if (pending.state === "evaluation-error") {
    return { state: "evaluation-error", pending, aggregate: pending };
  }

  let manualMergeRequired = false;
  const manualMergeResults = [];
  for (const prNumber of pending.pullNumbers) {
    const result = await evaluate({ github, context, core, prNumber, setOutputs: false });
    if (evaluationFailed(result)) {
      if (result.readFailure) {
        const aggregate = await blockAggregateEvaluation({
          github,
          context,
          core,
          headSha: pending.headSha,
          checkRunId: pending.checkRunId,
          reason: `PR #${prNumber} could not be evaluated: ${result.summary}`,
        });
        return { state: "evaluation-error", pending, aggregate };
      }
      throw new Error(`Auto Gate evaluation failed for PR #${prNumber}: ${result.summary}`);
    }
    if (result.headSha !== pending.headSha) {
      // The association changed after the snapshot. Keep the aggregate red;
      // the event for that change will reevaluate both affected heads.
      core.notice(
        `Keeping aggregate ${pending.headSha} non-green because PR #${prNumber} ` +
          `now evaluates at ${result.headSha || "no open master head"}.`,
      );
      return { state: "association-changed", pending };
    }
    let write;
    try {
      if (result.manualMergeRequired) {
        manualMergeRequired = true;
        manualMergeResults.push(result);
      }
      write = await reportDecision({ github, context, core, result, manual });
    } catch (error) {
      if (!isReadFailure(error)) {
        throw error;
      }
      const aggregate = await blockAggregateEvaluation({
        github,
        context,
        core,
        headSha: pending.headSha,
        checkRunId: pending.checkRunId,
        reason: formatError(error),
      });
      return { state: "evaluation-error", pending, aggregate };
    }
    if (write.state === "read-only") {
      return { state: "read-only", pending };
    }
  }

  // Disarm, then re-confirm as the very last thing before the green. The fixed
  // aggregate is the ONLY green branch protection consumes — the per-PR
  // decisions carry their own names and gate nothing — so the confirmation is
  // handed to reportAggregateDecision as a precondition rather than run here,
  // where the aggregate's own association and check-run reads would still sit
  // between it and the write.
  //
  // That final write cannot itself be conditioned on the observation: GitHub has
  // no compare-and-set for a check run. The residual is one write, which is the
  // minimum the API permits, and the gate's auto_merge_enabled subscription is
  // what covers it.
  let aggregate;
  try {
    if (manualMergeResults.length > 0) {
      const disarmed = await ensureNativeAutoMergeDisabled({
        github,
        context,
        core,
        results: manualMergeResults,
      });
      if (disarmed.state === "head-changed") {
        core.notice(
          `Keeping aggregate ${pending.headSha} non-green because PR #${disarmed.prNumber} ` +
            `now points at ${disarmed.headSha || "no head"}.`,
        );
        return { state: "association-changed", pending };
      }
    }
    aggregate = await reportAggregateDecision({
      github,
      context,
      core,
      headSha: pending.headSha,
      checkRunId: pending.checkRunId,
      beforePublish:
        manualMergeResults.length > 0
          ? () => confirmNativeAutoMergeDisabled({ github, context, results: manualMergeResults })
          : undefined,
    });
  } catch (error) {
    if (!isReadFailure(error)) {
      throw error;
    }
    const blocked = await blockAggregateEvaluation({
      github,
      context,
      core,
      headSha: pending.headSha,
      checkRunId: pending.checkRunId,
      reason: formatError(error),
    });
    return { state: "evaluation-error", pending, aggregate: blocked };
  }
  if (aggregate.writeState === "read-only" || !aggregate.ok || !mergeEnabled) {
    return { state: aggregate.state, pending, aggregate };
  }
  if (manualMergeRequired) {
    core.notice(
      `Leaving aggregate ${pending.headSha} green for maintainer merge; ` +
        "at least one associated PR requires maintainer review and manual merge.",
    );
    return { state: "manual", pending, aggregate };
  }

  const targetNumbers = new Set(
    targets
      .filter((target) => normalizeHeadSha(target.headSha) === pending.headSha)
      .map((target) => Number(target.prNumber)),
  );
  for (const prNumber of aggregate.pullNumbers.filter((number) => targetNumbers.has(number))) {
    try {
      const merged = await merge({
        github,
        context,
        core,
        prNumber,
        expectedHeadSha: pending.headSha,
      });
      // A successful merge advances master and invalidates every other
      // candidate's mergeability snapshot. Its resulting event will serialize
      // a new transaction; never merge a second shared-head PR from this one.
      return {
        state: "merged",
        pending,
        aggregate,
        invalidated: merged.invalidated,
        mergedPrNumber: prNumber,
      };
    } catch (error) {
      const message = error && error.message ? error.message : String(error);
      let invalidated;
      try {
        invalidated = await invalidateAggregateDecision({
          github,
          context,
          core,
          headSha: pending.headSha,
        });
        if (invalidated.writeState === "read-only") {
          throw new Error(`Could not invalidate aggregate ${pending.headSha}`);
        }
      } catch (invalidationError) {
        throw new AggregateError(
          [error, invalidationError],
          `Merge attempt and aggregate invalidation both failed on ${pending.headSha}`,
        );
      }
      if (isReadFailure(error)) {
        const blocked = await blockAggregateEvaluation({
          github,
          context,
          core,
          headSha: pending.headSha,
          checkRunId: invalidated.checkRunId,
          reason: formatError(error),
        });
        return { state: "evaluation-error", pending, aggregate: blocked, invalidated };
      }
      if (!message.startsWith(`Refusing to merge PR #${prNumber};`)) {
        throw error;
      }
      // A fresh refusal is ordinary waiting state. The fixed aggregate remains
      // the red enforcement record; infrastructure and merge API errors remain
      // fatal to the workflow job.
      core.notice(message);
      return { state: "waiting", pending, aggregate, invalidated };
    }
  }
  return { state: "pass", pending, aggregate };
}

async function evaluateAggregateDecision({ github, context, headSha }) {
  const sha = normalizeHeadSha(headSha);
  if (!sha) {
    throw new Error(`Invalid head SHA for Auto Gate aggregate: ${headSha}`);
  }
  const pulls = await listOpenMasterPullRequestsForHead({ github, context, headSha: sha });
  const { owner, repo } = context.repo;
  const checkRuns = await retryRead(`could not read check runs at commit ${sha}`, () =>
    github.paginate(github.rest.checks.listForRef, {
      owner,
      repo,
      ref: sha,
      per_page: 100,
    }),
  );
  const blockers = [];
  if (pulls.length === 0) {
    // Never pre-authorize a commit before a PR exists. A vacuously successful
    // aggregate could be inherited by a newly opened PR until its event run
    // makes the check non-green.
    blockers.push("No open pull request to master currently owns this commit");
  }
  for (const pull of pulls) {
    const identity = decisionIdentity(pull.number, sha);
    const decision = checkRuns
      .filter(
        (run) =>
          run.name === identity.checkName &&
          run.external_id === identity.externalId &&
          run.app?.id === GITHUB_ACTIONS_APP_ID,
      )
      .sort((left, right) => latestRunTime(right) - latestRunTime(left))[0];
    if (!decision) {
      blockers.push(
        `PR #${pull.number} at this commit has no exact PR/head Auto Gate decision; ` +
          `run Auto Gate manually for PR #${pull.number}`,
      );
      continue;
    }
    if (decision.status !== "completed" || decision.conclusion !== "success") {
      blockers.push(`PR #${pull.number} at this commit is waiting: ${decisionWaitingReason(decision)}`);
    }
  }
  const pullNumbers = pulls.map((pull) => pull.number);
  return {
    ok: blockers.length === 0,
    headSha: sha,
    pullNumbers,
    blockers,
    checkRuns,
    summary: formatAggregateSummary(pullNumbers, blockers, false),
  };
}

async function evaluateAggregateFresh({ github, context, core, headSha }) {
  const sha = normalizeHeadSha(headSha);
  if (!sha) {
    throw new Error(`Invalid head SHA for fresh Auto Gate aggregate: ${headSha}`);
  }
  const before = await listOpenMasterPullRequestsForHead({ github, context, headSha: sha });
  const blockers = [];
  if (before.length === 0) {
    blockers.push("No open pull request to master currently owns this commit");
  }
  for (const pull of before) {
    const result = await evaluate({
      github,
      context,
      core,
      prNumber: pull.number,
      setOutputs: false,
    });
    if (evaluationFailed(result)) {
      throw evaluationFailure(
        `Auto Gate fresh aggregate evaluation failed for PR #${pull.number}: ${result.summary}`,
        result,
      );
    }
    if (result.headSha !== sha) {
      blockers.push(`PR #${pull.number} no longer evaluates at this commit`);
    } else if (!result.shouldMerge) {
      blockers.push(`PR #${pull.number} at this commit is waiting: ${decisionWaitingReason(result)}`);
    }
  }
  const after = await listOpenMasterPullRequestsForHead({ github, context, headSha: sha });
  const beforeNumbers = before.map((pull) => pull.number);
  const afterNumbers = after.map((pull) => pull.number);
  if (!sameNumbers(beforeNumbers, afterNumbers)) {
    blockers.push(
      "Open master PR associations changed during the fresh merge evaluation; run Auto Gate again",
    );
  }
  return {
    ok: blockers.length === 0,
    headSha: sha,
    pullNumbers: afterNumbers,
    blockers,
    summary: formatAggregateSummary(afterNumbers, blockers, false),
  };
}

async function upsertAggregateCheck({
  github,
  context,
  core,
  headSha,
  checkRuns,
  decision,
  checkRunId,
  beforePublish,
}) {
  const { owner, repo } = context.repo;
  const identity = aggregateIdentity(headSha);
  const prior = checkRunId
    ? { id: checkRunId }
    : checkRuns
        .filter(
          (run) =>
            run.name === identity.checkName &&
            run.external_id === identity.externalId &&
            run.app?.id === GITHUB_ACTIONS_APP_ID,
        )
        .sort((left, right) => latestRunTime(right) - latestRunTime(left))[0];
  try {
    // Inside the retry, not outside it. A retried write sleeps and reissues, and
    // a precondition checked once before the helper is stale for every attempt
    // after the first — long enough for auto-merge to be armed during the
    // backoff and consumed by the green the retry then publishes. Each attempt
    // re-establishes it.
    const guard = async () => {
      if (typeof beforePublish === "function") {
        await beforePublish();
      }
    };
    if (prior) {
      await retryCheckUpdate(`could not update aggregate check at ${headSha}`, async () => {
        await guard();
        return github.rest.checks.update({
          owner,
          repo,
          check_run_id: prior.id,
          ...decision,
        });
      });
    } else {
      await createCheckRun({
        github,
        owner,
        repo,
        headSha,
        name: identity.checkName,
        externalId: identity.externalId,
        decision,
        label: `could not create aggregate check at ${headSha}`,
        beforeWrite: guard,
      });
    }
  } catch (error) {
    if (!isReadOnlyForkCheckError(error, context)) {
      throw error;
    }
    core.warning(
      `Auto Gate could not publish the aggregate for fork head ${headSha} with its read-only ` +
        "token; run workflow_dispatch from the base repository to recover it.",
    );
    return { writeState: "read-only", priorAggregate: Boolean(prior) };
  }
  return { writeState: prior ? "updated" : "created", priorAggregate: Boolean(prior) };
}

async function createAggregateCheck({ github, context, core, headSha, decision }) {
  const { owner, repo } = context.repo;
  const identity = aggregateIdentity(headSha);
  try {
    const response = await createCheckRun({
      github,
      owner,
      repo,
      headSha,
      name: identity.checkName,
      externalId: identity.externalId,
      decision,
      label: `could not invalidate aggregate ${headSha}`,
    });
    return { writeState: "created", priorAggregate: false, checkRunId: response.data.id };
  } catch (error) {
    if (!isReadOnlyForkCheckError(error, context)) {
      throw error;
    }
    core.warning(
      `Auto Gate could not invalidate the aggregate for fork head ${headSha} with its ` +
        "read-only token; the writable pull_request_target run must handle it.",
    );
    return { writeState: "read-only", priorAggregate: false, checkRunId: null };
  }
}

function aggregateIdentity(headSha) {
  const sha = normalizeHeadSha(headSha);
  if (!sha) {
    throw new Error(`Invalid head SHA for Auto Gate aggregate: ${headSha}`);
  }
  return {
    checkName: AUTO_GATE_DECISION_CHECK,
    externalId: `${AUTO_GATE_AGGREGATE_EXTERNAL_ID_PREFIX}${sha}`,
  };
}

function normalizeHeadSha(value) {
  const sha = String(value || "").toLowerCase();
  return /^[0-9a-f]{40}$/.test(sha) ? sha : "";
}

function decisionWaitingReason(decision) {
  const detail = decision.summary || decision.output?.summary || decision.output?.title || "";
  if (detail) {
    return detail.replace(/^(?:BLOCKED|WAITING|NEVER_RAN):\s*/i, "");
  }
  if (decision.status !== "completed") {
    return `its exact PR/head decision is ${decision.status}`;
  }
  return `its exact PR/head decision concluded ${decision.conclusion || "without a conclusion"}`;
}

function sameNumbers(left, right) {
  return left.length === right.length && left.every((number, index) => number === right[index]);
}

function evaluationFailed(result) {
  return result.reasons?.some((reason) => reason.startsWith("auto-gate evaluation error:"));
}

function evaluationFailure(message, result) {
  const error = new Error(message);
  if (result?.readFailure) {
    error.autoGateReadFailure = true;
  }
  return error;
}

function formatAggregateSummary(pullNumbers, blockers, pending) {
  const shared = pullNumbers.length > 1;
  const coupling =
    "This fixed-name check is commit-scoped and passes only when every open master PR " +
    "sharing the commit has a passing exact (PR, head) Auto Gate decision.";
  const context =
    pullNumbers.length === 0
      ? "No open master PR currently points at this commit.\n\n"
      : shared
        ? `This commit is shared by open master PRs ${joinPullNumbers(pullNumbers)}. ` +
          `${coupling}\n\n`
        : "";
  if (pending) {
    return `${context}Auto Gate is refreshing the associated decisions now.`;
  }
  if (blockers.length === 0) {
    return `${context}Every associated decision passes.`;
  }
  const recovery =
    "To decouple without merging another PR, push either branch to a distinct commit, close " +
    "the other PR, or retarget it away from master; then run Auto Gate manually by PR number.";
  const recoveryHint = shared ? `\n\n${recovery}` : "";
  return `${context}Waiting on:\n- ${blockers.join("\n- ")}${recoveryHint}`;
}

function joinPullNumbers(numbers) {
  const labels = numbers.map((number) => `#${number}`);
  if (labels.length < 2) {
    return labels[0] || "";
  }
  if (labels.length === 2) {
    return `${labels[0]} and ${labels[1]}`;
  }
  return `${labels.slice(0, -1).join(", ")}, and ${labels.at(-1)}`;
}

function isReadOnlyForkCheckError(error, context) {
  return (
    error?.status === 403 &&
    /Resource not accessible by integration/i.test(error.message || "") &&
    context.payload.pull_request?.head?.repo?.fork === true
  );
}

function decisionIdentity(prNumber, headSha) {
  const number = Number(prNumber);
  const sha = String(headSha || "").toLowerCase();
  if (!Number.isSafeInteger(number) || number <= 0) {
    throw new Error(`Invalid PR number for Auto Gate decision: ${prNumber}`);
  }
  if (!/^[0-9a-f]{40}$/.test(sha)) {
    throw new Error(`Invalid head SHA for Auto Gate decision: ${headSha}`);
  }
  return {
    checkName: `${AUTO_GATE_DECISION_CHECK} / PR #${number} / ${sha}`,
    externalId: `auto-gate:pr:${number}:head:${sha}`,
    key: `pr-${number}-head-${sha}`,
  };
}

async function merge({ github, context, core, prNumber, expectedHeadSha }) {
  if (!Number.isInteger(prNumber) || prNumber <= 0) {
    throw new Error(`Invalid PR number for merge: ${prNumber}`);
  }

  const gate = await evaluate({ github, context, core, prNumber, setOutputs: false });
  if (evaluationFailed(gate)) {
    throw evaluationFailure(
      `Auto Gate merge evaluation failed for PR #${prNumber}: ${gate.summary}`,
      gate,
    );
  }
  if (!gate.shouldMerge) {
    throw new Error(`Refusing to merge PR #${prNumber}; gate no longer passes: ${gate.summary}`);
  }
  if (!gate.headSha) {
    throw new Error(`Refusing to merge PR #${prNumber}; evaluated head SHA is missing`);
  }
  if (expectedHeadSha && gate.headSha !== expectedHeadSha) {
    throw new Error(
      `Refusing to merge PR #${prNumber}; serialized head ${expectedHeadSha} ` +
        `does not match evaluated head ${gate.headSha}`,
    );
  }
  const aggregate = await evaluateAggregateFresh({
    github,
    context,
    core,
    headSha: gate.headSha,
  });
  if (!aggregate.ok) {
    throw new Error(
      `Refusing to merge PR #${prNumber}; fixed aggregate no longer passes: ${aggregate.summary}`,
    );
  }

  const { owner, repo } = context.repo;
  const response = await github.rest.pulls.merge({
    owner,
    repo,
    pull_number: prNumber,
    merge_method: "squash",
    sha: gate.headSha,
  });

  core.notice(`Squash-merged PR #${prNumber}: ${response.data.sha}`);
  const postMergeErrors = [];
  let invalidated;
  try {
    invalidated = await invalidateAggregateDecision({
      github,
      context,
      core,
      headSha: gate.headSha,
    });
    if (invalidated.writeState === "read-only") {
      throw new Error(`Could not invalidate aggregate ${gate.headSha}`);
    }
    core.notice(`Invalidated the pre-merge aggregate on ${gate.headSha}.`);
  } catch (error) {
    postMergeErrors.push(error);
  }

  if (gate.docsChanged) {
    try {
      await github.rest.actions.createWorkflowDispatch({
        owner,
        repo,
        workflow_id: "docs.yml",
        ref: "master",
        inputs: {
          deploy_docs: "true",
        },
      });
      core.notice(`Dispatched Docs workflow for PR #${prNumber} docs-path merge.`);
    } catch (error) {
      postMergeErrors.push(error);
    }
  }
  if (postMergeErrors.length > 0) {
    const detail = postMergeErrors.map((error) => error.message || String(error)).join("; ");
    throw new AggregateError(
      postMergeErrors,
      `PR #${prNumber} merged, but post-merge operation(s) failed: ${detail}`,
    );
  }
  return { mergeSha: response.data.sha, invalidated };
}

async function resolveTargets({ github, context, core, prNumber }) {
  const numbers = [];
  const payload = context.payload;

  if (prNumber) {
    numbers.push(prNumber);
  } else if (payload.pull_request?.number) {
    numbers.push(payload.pull_request.number);
  } else if (payload.issue?.pull_request && payload.issue.number) {
    numbers.push(payload.issue.number);
  } else {
    const sha = payload.check_suite?.head_sha || payload.workflow_run?.head_sha || payload.sha;
    if (sha) {
      const pulls = await listOpenMasterPullRequestsForHead({ github, context, headSha: sha });
      numbers.push(...pulls.map((pull) => pull.number));
    } else {
      core.info(`Event ${context.eventName} did not identify a PR/head pair to evaluate.`);
    }
  }

  const sourceSha = payload.check_suite?.head_sha || payload.workflow_run?.head_sha || payload.sha;
  const targets = [];
  for (const number of [...new Set(numbers.filter(Boolean))]) {
    const pr = await getPullRequest({ github, context, number });
    if (
      sourceSha &&
      (pr.state !== "OPEN" || pr.merged || pr.baseRefName !== "master" || pr.headRefOid !== sourceSha)
    ) {
      continue;
    }
    targets.push({
      prNumber: Number(pr.number),
      headSha: pr.headRefOid,
      decisionKey: decisionIdentity(pr.number, pr.headRefOid).key,
    });
  }
  return targets;
}

async function listOpenMasterPullRequestsForHead({ github, context, headSha }) {
  const { owner, repo } = context.repo;
  // This association list is a merge-path ownership read. A failed read is not
  // an empty result: retry transient failures, then propagate an explicit
  // failure so the aggregate stays red without claiming the commit has no PRs.
  const pulls = await retryRead(`could not enumerate PRs at commit ${headSha}`, () =>
    github.paginate(
      github.rest.repos.listPullRequestsAssociatedWithCommit,
      {
        owner,
        repo,
        commit_sha: headSha,
        per_page: 100,
      },
    ),
  );
  return pulls
    .filter(
      (pull) =>
        pull.state === "open" &&
        pull.base?.ref === "master" &&
        pull.head?.sha === headSha,
    )
    .sort((left, right) => left.number - right.number);
}

async function findPullRequestNumber({ github, context, core }) {
  const targets = await resolveTargets({
    github,
    context,
    core,
  });
  return targets[0]?.prNumber || null;
}

async function getPullRequest({ github, context, number }) {
  const { owner, repo } = context.repo;
  const query = `
    query($owner: String!, $repo: String!, $number: Int!) {
      repository(owner: $owner, name: $repo) {
        pullRequest(number: $number) {
          id
          number
          title
          url
          baseRefName
          headRefName
          headRefOid
          headRepository {
            nameWithOwner
          }
          isDraft
          state
          merged
          mergeable
          mergeStateStatus
          author {
            login
          }
          labels(first: 100) {
            nodes {
              name
            }
          }
          commits(last: 1) {
            nodes {
              commit {
                committedDate
              }
            }
          }
        }
      }
    }
  `;

  const response = await retryRead(`could not read PR #${number}`, () =>
    github.graphql(query, { owner, repo, number }),
  );
  const pr = response.repository.pullRequest;
  if (!pr) {
    throw new Error(`PR #${number} was not found`);
  }

  return {
    id: pr.id,
    number: pr.number,
    title: pr.title,
    url: pr.url,
    baseRefName: pr.baseRefName,
    headRefOid: pr.headRefOid,
    headRepository: pr.headRepository?.nameWithOwner || "",
    isDraft: pr.isDraft,
    state: pr.state,
    merged: pr.merged,
    mergeable: pr.mergeable,
    mergeStateStatus: pr.mergeStateStatus,
    author: pr.author?.login || "",
    labels: pr.labels.nodes.map((label) => label.name),
    lastCommitDate: pr.commits.nodes[0]?.commit?.committedDate,
  };
}

// Make "GitHub-native auto-merge is off" a PRECONDITION of publishing a
// manual-merge PR's passing decision, rather than a best-effort side effect.
//
// manualMergeRequired publishes a PASSING decision while refusing to auto-merge,
// so if native auto-merge is armed GitHub can merge on that green — a PR the gate
// has just declared maintainer-review-only. The guard used to be keyed on
// nativeAutoMergeEnabled, captured once in getPullRequest, with the PR files,
// required checks, issue comments, reviews and review comments all read in
// between. Auto-merge armed anywhere in that window was invisible, the disable
// was skipped, and the decision published green (#3381).
//
// The freshness fix is a re-read, and the safety fix is confirming the result:
// this returns normally only when a read taken moments before the publish says
// auto-merge is off. Anything else throws, which leaves the aggregate red.
//
// Note what this deliberately does NOT do: attempt the disable unconditionally
// and tolerate the "not in the auto-merge queue" rejection. That would have to
// tell a benign error from a real one by its message, and getting that wrong
// turns the safety mechanism into a silent no-op. Here a read distinguishes
// "was never armed" from "failed to disarm", so no error text is interpreted.
async function ensureNativeAutoMergeDisabled({ github, context, core, results }) {
  const numbers = results.map((result) => Number(result.prNumber));
  const states = await readNativeAutoMergeStates({ github, context, numbers });
  for (const result of results) {
    const prNumber = Number(result.prNumber);
    const state = states.get(prNumber);
    // The state belongs to a head this transaction does not own. Cancelling it
    // would destroy an auto-merge request armed for the NEW head — one this
    // transaction never evaluated and has no claim over. Stop instead; the
    // synchronize event owns that head and will evaluate it.
    if (normalizeHeadSha(state.headSha) !== normalizeHeadSha(result.headSha)) {
      return { state: "head-changed", prNumber, headSha: state.headSha };
    }
    if (!state.armed) {
      continue;
    }
    if (!result.pullRequestId) {
      throw new Error(`Cannot disable native auto-merge for PR #${prNumber}: missing node ID`);
    }
    // Re-read this PR alone, immediately before its mutation. The batched read
    // above is one snapshot for the whole head, and the disables that follow it
    // are sequential: a PR can synchronize to a new head while an earlier PR is
    // being disabled, and the mutation takes a stable node ID, so it would
    // cancel a queue entry armed for a head this transaction never evaluated.
    // Check-then-act cannot be made atomic here, but the window is one call.
    const fresh = (await readNativeAutoMergeStates({ github, context, numbers: [prNumber] })).get(
      prNumber,
    );
    if (normalizeHeadSha(fresh.headSha) !== normalizeHeadSha(result.headSha)) {
      return { state: "head-changed", prNumber, headSha: fresh.headSha };
    }
    if (!fresh.armed) {
      continue;
    }
    const mutation = `
      mutation DisablePullRequestAutoMerge($pullRequestId: ID!) {
        disablePullRequestAutoMerge(input: {pullRequestId: $pullRequestId}) {
          pullRequest {
            number
          }
        }
      }
    `;
    await github.graphql(mutation, { pullRequestId: result.pullRequestId });
    core.notice(
      `Disabled GitHub-native auto-merge for manual-only PR #${prNumber} before ` +
        "publishing its passing decision.",
    );
  }
  return { state: "disarmed" };
}

// The final observation, run immediately before the aggregate write and after
// every read that write depends on. The mutation succeeding is a claim; this is
// the evidence. Anything still armed — or any state that cannot be read — throws,
// so no consumable green is published.
async function confirmNativeAutoMergeDisabled({ github, context, results }) {
  const numbers = results.map((result) => Number(result.prNumber));
  const states = await readNativeAutoMergeStates({ github, context, numbers });
  const armed = [...states.entries()]
    .filter(([, state]) => state.armed)
    .map(([number]) => number);
  if (armed.length > 0) {
    throw new Error(
      `Refusing to publish a passing decision for manual-only PR ${joinPullNumbers(armed)}: ` +
        "GitHub-native auto-merge is still armed after disabling it",
    );
  }
}

// The auto-merge state of every manual-merge PR at this head, in ONE read.
// Batched rather than looped on purpose: the guarantee this guard offers is
// "nothing was armed as of the last observation before the write", and a loop of
// N reads makes that claim N-1 reads stale for the first PR. One query keeps the
// observation simultaneous across the whole head.
//
// headRefOid comes back with it because the state is only meaningful for the head
// this transaction owns: a PR that synchronized to a new head has an auto-merge
// request belonging to THAT head, and cancelling it would destroy a queue entry
// this transaction has no claim over.
//
// Separate from getPullRequest on purpose: the whole defect is that
// getPullRequest's answer is many round trips old by the time the decision acts.
async function readNativeAutoMergeStates({ github, context, numbers }) {
  const { owner, repo } = context.repo;
  if (numbers.length === 0) {
    return new Map();
  }
  const parameters = numbers.map((_, index) => `$n${index}: Int!`).join(", ");
  const fields = numbers
    .map(
      (_, index) => `
          pr${index}: pullRequest(number: $n${index}) {
            number
            headRefOid
            autoMergeRequest {
              enabledAt
            }
          }`,
    )
    .join("");
  const query = `
    query AutoMergeState($owner: String!, $repo: String!, ${parameters}) {
      repository(owner: $owner, name: $repo) {${fields}
      }
    }
  `;
  const variables = { owner, repo };
  numbers.forEach((number, index) => {
    variables[`n${index}`] = number;
  });
  const label = `could not read auto-merge state for PR ${joinPullNumbers(numbers)}`;
  const response = await retryRead(label, () => github.graphql(query, variables));
  const states = new Map();
  numbers.forEach((number, index) => {
    const pr = response?.repository?.[`pr${index}`];
    if (!pr) {
      // An unreadable state is not a disarmed one. Fail closed: the caller turns
      // this into a red aggregate rather than a published green.
      throw new Error(`Could not read auto-merge state for PR #${number}`);
    }
    states.set(number, { armed: Boolean(pr.autoMergeRequest), headSha: pr.headRefOid });
  });
  return states;
}

async function listPullRequestFiles({ github, context, number }) {
  const { owner, repo } = context.repo;
  const files = await retryRead(`could not list files for PR #${number}`, () =>
    github.paginate(github.rest.pulls.listFiles, {
      owner,
      repo,
      pull_number: number,
      per_page: 100,
    }),
  );
  return files.map((file) => file.filename);
}

async function evaluateRequiredChecks({ github, context, branch, sha, core }) {
  const required = await getRequiredCheckSpecs({ github, context, branch, core });
  const syntheticDecisionSpecs = required.specs.filter(
    (spec) =>
      isSyntheticDecisionContext(spec.context) &&
      (!spec.sourceAppId || spec.sourceAppId === GITHUB_ACTIONS_APP_ID),
  );
  const specs = required.specs.filter(
    (spec) =>
      !isSyntheticDecisionContext(spec.context) ||
      (spec.sourceAppId && spec.sourceAppId !== GITHUB_ACTIONS_APP_ID),
  );
  const notes = [];
  const reasons = [...required.errors];

  if (syntheticDecisionSpecs.length > 0) {
    notes.push("Synthetic Auto Gate decisions are excluded from their own prerequisites");
  }

  if (specs.length === 0) {
    notes.push("No required status checks configured for branch");
  } else {
    notes.push(`Required status checks: ${specs.map(formatCheckSpec).join(", ")}`);
  }

  const { owner, repo } = context.repo;
  const checkRuns = await retryRead(`could not read check runs at commit ${sha}`, () =>
    github.paginate(github.rest.checks.listForRef, {
      owner,
      repo,
      ref: sha,
      per_page: 100,
    }),
  );
  const statuses = await retryRead(`could not read commit statuses at ${sha}`, () =>
    github.paginate(github.rest.repos.listCommitStatusesForRef, {
      owner,
      repo,
      ref: sha,
      per_page: 100,
    }),
  );

  for (const spec of specs) {
    const state = latestRequiredState(spec, checkRuns, statuses);
    if (!state) {
      reasons.push(`required check ${formatCheckSpec(spec)} is missing on ${sha}`);
      continue;
    }

    notes.push(`${formatCheckSpec(spec)}: ${state.description}`);
    if (!state.ok) {
      const stateDescription = state.waiting ? "is still settling" : "did not succeed";
      reasons.push(`required check ${formatCheckSpec(spec)} ${stateDescription} (${state.description})`);
    }
  }

  return { ok: reasons.length === 0, reasons, notes };
}

function isSyntheticDecisionContext(contextName) {
  return (
    contextName === AUTO_GATE_DECISION_CHECK ||
    /^Auto Gate decision \/ PR #\d+ \/ [0-9a-f]{40}$/.test(contextName)
  );
}

async function getRequiredCheckSpecs({ github, context, branch, core }) {
  const { owner, repo } = context.repo;
  const specs = new Map();
  const errors = [];

  try {
    const response = await retryRead(`could not read branch rules for ${branch}`, () =>
      github.request("GET /repos/{owner}/{repo}/rules/branches/{branch}", {
        owner,
        repo,
        branch,
      }),
    );

    for (const rule of response.data || []) {
      if (rule.type !== "required_status_checks") {
        continue;
      }
      for (const check of rule.parameters?.required_status_checks || []) {
        if (check.context) {
          addCheckSpec(specs, check.context, check.integration_id);
        }
      }
    }
  } catch (error) {
    if (error.status !== 404) {
      const message = `could not read branch rules for ${branch}: ${formatError(error)}`;
      core.warning(message);
      errors.push(message);
    }
  }

  if (specs.size > 0) {
    return { specs: sortedCheckSpecs(specs), errors: [] };
  }

  try {
    const response = await retryRead(
      `could not read branch protection checks for ${branch}`,
      () =>
        github.request(
          "GET /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks",
          { owner, repo, branch },
        ),
    );

    for (const contextName of response.data.contexts || []) {
      addCheckSpec(specs, contextName, null);
    }
    for (const check of response.data.checks || []) {
      if (check.context) {
        addCheckSpec(specs, check.context, check.app_id);
      }
    }
  } catch (error) {
    if (error.status !== 404) {
      const message = `could not read branch protection checks for ${branch}: ${formatError(error)}`;
      core.warning(message);
      errors.push(message);
    }
  }

  return { specs: sortedCheckSpecs(specs), errors };
}

function addCheckSpec(specs, contextName, sourceAppId) {
  const parsedAppId = Number(sourceAppId);
  const normalizedAppId = Number.isInteger(parsedAppId) && parsedAppId > 0 ? parsedAppId : null;
  const key = `${contextName}\0${normalizedAppId || ""}`;
  specs.set(key, { context: contextName, sourceAppId: normalizedAppId });
}

function sortedCheckSpecs(specs) {
  return [...specs.values()].sort((a, b) => formatCheckSpec(a).localeCompare(formatCheckSpec(b)));
}

function formatCheckSpec(spec) {
  return spec.sourceAppId ? `${spec.context} (app ${spec.sourceAppId})` : spec.context;
}

function latestRequiredState(spec, checkRuns, statuses) {
  const candidates = [];

  for (const run of checkRuns) {
    if (!checkRunMatchesSpec(run, spec)) {
      continue;
    }
    const state = checkRunState(run);
    candidates.push({
      date: parseTimestamp(run.completed_at || run.started_at || run.created_at) || 0,
      ...state,
    });
  }

  if (!spec.sourceAppId) {
    for (const status of statuses) {
      if (status.context !== spec.context) {
        continue;
      }
      candidates.push({
        date: parseTimestamp(status.created_at) || 0,
        ok: status.state === "success",
        waiting: status.state === "pending",
        description: `commit status ${status.state}`,
      });
    }
  }

  candidates.sort((a, b) => b.date - a.date);
  return candidates[0] || null;
}

function checkRunMatchesSpec(run, spec) {
  if (run.name !== spec.context) {
    return false;
  }
  return !spec.sourceAppId || Number(run.app?.id) === spec.sourceAppId;
}

function checkRunState(run) {
  const description = `check run ${run.status}/${run.conclusion || "no conclusion"} from ${formatRunSource(run)}`;
  const successful = run.status === "completed" && run.conclusion === "success";
  const conditionalSkip =
    run.status === "completed" &&
    run.conclusion === "skipped" &&
    ALLOWED_SKIPPED_CHECKS.has(run.name) &&
    run.app?.slug === "github-actions";

  return {
    ok: successful || conditionalSkip,
    // GitHub Advanced Security reports CodeQL neutral while its Analyze jobs run,
    // then updates the same head check to success when analysis settles.
    waiting: run.status !== "completed" || (run.name === "CodeQL" && run.conclusion === "neutral"),
    description,
  };
}

function formatRunSource(run) {
  if (!run.app) {
    return "unknown source";
  }
  const name = run.app.slug || run.app.name || "app";
  return `${name} (${run.app.id || "unknown app id"})`;
}

async function evaluateCodex({ github, context, number, sha, lastCommitDate }) {
  const notes = [];
  const reasons = [];
  // Set only where it is proven: no exact-head verdict AND the reviewer's own
  // usage-limit message on its latest comment. Anything else leaves it false.
  // reviewerUnavailableReason is the one reason a degradation may waive; the
  // caller uses it to tell that reason apart from every independent blocker.
  let reviewerUnavailable = false;
  let reviewerUnavailableReason = "";
  const { owner, repo } = context.repo;
  const lastPushTime = parseTimestamp(lastCommitDate);

  if (lastPushTime == null) {
    reasons.push("last commit timestamp was unavailable, so Codex freshness cannot be verified");
  }

  const comments = await retryRead(`could not read issue comments for PR #${number}`, () =>
    github.paginate(github.rest.issues.listComments, {
      owner,
      repo,
      issue_number: number,
      per_page: 100,
    }),
  );
  const reviews = await retryRead(`could not read reviews for PR #${number}`, () =>
    github.paginate(github.rest.pulls.listReviews, {
      owner,
      repo,
      pull_number: number,
      per_page: 100,
    }),
  );
  const codexComments = comments.filter((comment) => comment.user?.login === CODEX_REVIEWER);
  const codexReviewArtifacts = [...codexComments, ...reviews]
    .filter((comment) => comment.user?.login === CODEX_REVIEWER)
    .sort((a, b) => reviewArtifactTime(b) - reviewArtifactTime(a));
  const matchingReviewArtifacts = codexReviewArtifacts.filter((comment) => {
    const reviewedCommit = parseReviewedCommit(comment.body || "");
    return reviewedCommit != null && reviewedCommitMatchesHead(reviewedCommit, sha);
  });
  const verdict = matchingReviewArtifacts[0];

  if (!verdict) {
    // The reviewer saying it is out of quota is the only accepted evidence, and
    // only on its latest response: an older usage-limit note that a later
    // response superseded proves nothing about now. A read that fails throws out
    // of retryRead rather than reaching here, so an unreadable list can never be
    // mistaken for "no rate limit" — or for a rate limit.
    //
    // Latest across issue comments AND reviews — the list is already sorted
    // newest-first. Reading only issue comments would miss a review posted after
    // a quota message, which proves the reviewer answered again and therefore
    // that the quota message no longer describes the present.
    const latestCodexArtifact = codexReviewArtifacts[0];
    const latestCodexBody = latestCodexArtifact?.body || "";
    // The detector is an unanchored substring match, so a review that merely
    // QUOTES the usage-limit phrase trips it — reviewing this very gate is
    // enough. A body carrying both review markers is a review, not a quota
    // response; the genuine message carries neither (verified against the real
    // one on #3371), so requiring their absence cannot suppress it. Such a body
    // already fails parseReviewedCommit, so it is not a verdict either, and the
    // gate lands on "keep blocking" rather than on a false degradation.
    const looksLikeReviewArtifact =
      CODEX_REVIEW_RE.test(latestCodexBody) && REVIEWED_COMMIT_RE.test(latestCodexBody);
    const rateLimited = CODEX_RATE_LIMIT_RE.test(latestCodexBody) && !looksLikeReviewArtifact;
    // …and it has to be evidence about THIS head, on the same freshness rule the
    // verdict below is held to. A usage-limit answer only proves the reviewer was
    // out of quota when it answered; a head pushed after it may simply not have
    // been reached yet, which is the silence case and must keep blocking. Without
    // this the degradation is sticky: one usage-limit comment would put the PR in
    // manual-merge mode for every later push, forever. Fails closed on an unknown
    // order, like every other timestamp comparison in this file.
    const rateLimitTime = rateLimited ? reviewArtifactTime(latestCodexArtifact) : 0;
    reviewerUnavailable = rateLimited && lastPushTime != null && rateLimitTime > lastPushTime;
    const suffix = !rateLimited
      ? ""
      : reviewerUnavailable
        ? "; the latest Codex response was usage-limited"
        : "; the latest Codex response was usage-limited but predates this head, so it is not " +
          "evidence about this head";
    const missingVerdictReason = `Codex has not reviewed head ${sha} yet${suffix}`;
    if (reviewerUnavailable) {
      reviewerUnavailableReason = missingVerdictReason;
    }
    reasons.push(missingVerdictReason);
  } else {
    const verdictTime = reviewArtifactTime(verdict);
    if (lastPushTime == null || verdictTime === 0 || verdictTime <= lastPushTime) {
      reasons.push("Codex verdict for the head commit is older than the head commit timestamp");
    } else {
      notes.push(`Codex verdict matches head ${sha}`);
    }
  }

  if (verdict && CODEX_BODY_FINDING_RE.test(verdict.body || "")) {
    reasons.push("latest exact-head Codex review body contains a P0-P3 finding");
  }

  const reviewComments = await retryRead(`could not read review comments for PR #${number}`, () =>
    github.paginate(github.rest.pulls.listReviewComments, {
      owner,
      repo,
      pull_number: number,
      per_page: 100,
    }),
  );
  const resolvedByAllowedReply = new Set(
    reviewComments
      .filter((comment) => {
        return (
          comment.in_reply_to_id &&
          ALLOWED_AUTHORS.has(comment.user?.login || "") &&
          hasResolutionMarker(comment.body || "")
        );
      })
      .map((comment) => comment.in_reply_to_id),
  );
  const isLiveFinding = (comment) =>
    comment.user?.login === CODEX_REVIEWER && !comment.in_reply_to_id && comment.line != null;
  const unresolvedFindings = reviewComments.filter(
    (comment) => isLiveFinding(comment) && !resolvedByAllowedReply.has(comment.id),
  );

  if (unresolvedFindings.length > 0) {
    reasons.push(`${unresolvedFindings.length} unresolved live Codex inline finding(s)`);
  } else {
    notes.push("No unresolved live Codex inline findings");
  }

  // A RESOLVED reply is a CLAIM; the commit is the evidence. Clearing a finding
  // fires pull_request_review_comment, which re-runs this gate — so without this
  // check the merge happens the instant the reply lands, on a head that predates
  // the fix. That is how #2799, #2718, #2829 and #2839 all merged with real
  // findings live, the fix commits arriving minutes after the squash (#2878).
  //
  // A fix for a finding filed at T cannot exist in a commit made before T. That
  // is the whole test — necessary, not sufficient, and deliberately not "newer
  // than the reply": the normal order is fix, push, reply, which leaves the head
  // older than the reply and would block a correctly fixed PR.
  const allowedReplies = reviewComments.filter(
    (comment) => comment.in_reply_to_id && ALLOWED_AUTHORS.has(comment.user?.login || ""),
  );
  const claimedFixed = new Set(
    allowedReplies.filter((c) => FIX_CLAIM_RE.test(c.body || "")).map((c) => c.in_reply_to_id),
  );
  const claimedNoChange = new Set(
    allowedReplies
      .filter((c) => NO_CHANGE_CLAIM_RE.test(c.body || "") || (c.body || "").includes("[gate-ack]"))
      .map((c) => c.in_reply_to_id),
  );
  const unpushedFixClaims = reviewComments.filter((comment) => {
    if (!isLiveFinding(comment)) {
      return false;
    }
    // An explicit "no code change is owed" wins: the author asserted the finding
    // does not apply, and no commit can be demanded for that.
    if (!claimedFixed.has(comment.id) || claimedNoChange.has(comment.id)) {
      return false;
    }
    const filedAt = parseTimestamp(comment.created_at);
    // Unparseable timestamps fail closed — an unknown order is not a proven one.
    return filedAt == null || lastPushTime == null || lastPushTime <= filedAt;
  });

  if (unpushedFixClaims.length > 0) {
    reasons.push(
      `${unpushedFixClaims.length} finding(s) marked RESOLVED with no commit pushed after them; ` +
        "the head predates the fix they claim",
    );
  }

  return { ok: reasons.length === 0, reasons, notes, reviewerUnavailable, reviewerUnavailableReason };
}

function parseReviewedCommit(body) {
  if (!CODEX_REVIEW_RE.test(body) || CODEX_RATE_LIMIT_RE.test(body)) {
    return null;
  }
  return body.match(REVIEWED_COMMIT_RE)?.[1]?.toLowerCase() || null;
}

function reviewedCommitMatchesHead(reviewedCommit, headSha) {
  const normalizedHead = String(headSha || "").toLowerCase();
  return /^[0-9a-f]{40}$/.test(normalizedHead) && normalizedHead.startsWith(reviewedCommit);
}

function reviewArtifactTime(artifact) {
  return parseTimestamp(artifact.updated_at || artifact.submitted_at || artifact.created_at) || 0;
}

function hasResolutionMarker(body) {
  return RESOLUTION_MARKER_RE.test(body) || body.includes("[gate-ack]");
}

function latestRunTime(run) {
  return parseTimestamp(run.completed_at || run.started_at || run.created_at) || 0;
}

function newestCheckGeneration(checkRuns) {
  return [...checkRuns].sort((left, right) => {
    const createdDifference =
      (parseTimestamp(right.created_at) || 0) - (parseTimestamp(left.created_at) || 0);
    if (createdDifference !== 0) {
      return createdDifference;
    }
    return Number(right.id || 0) - Number(left.id || 0);
  })[0];
}

function parseTimestamp(value) {
  const parsed = Date.parse(value || "");
  return Number.isFinite(parsed) ? parsed : null;
}

function finish(core, setOutputs, result) {
  let summary;
  if (result.manualMergeRequired) {
    const manual = (result.manualMergeReasons || []).join(" ") || MANUAL_MERGE_AUTHOR_REASON;
    const unmet =
      result.reasons.length === 0
        ? ""
        : `\n\nUnmet automatic-merge requirements:\n- ${result.reasons.join("\n- ")}`;
    summary = `PASS: ${manual}${unmet}`;
  } else {
    summary =
      result.reasons.length === 0
        ? `PASS: ${result.notes.join("; ")}`
        : `BLOCKED: ${result.reasons.join("; ")}`;
  }

  if (result.reasons.length === 0) {
    core.notice(summary);
  } else {
    core.notice(summary);
  }

  for (const note of result.notes || []) {
    core.info(`gate note: ${note}`);
  }
  for (const reason of result.reasons || []) {
    core.info(`gate block: ${reason}`);
  }

  if (setOutputs) {
    core.setOutput("pr_number", result.prNumber);
    core.setOutput("should_merge", result.shouldMerge ? "true" : "false");
    core.setOutput("head_sha", result.headSha || "");
    core.setOutput("docs_changed", result.docsChanged ? "true" : "false");
    core.setOutput("summary", summary);
  }

  return { ...result, summary };
}

function formatError(error) {
  return `${error.status || "error"} ${error.message || error}`;
}

module.exports = {
  beginAggregateDecision,
  evaluate,
  evaluateAggregateDecision,
  evaluateAggregateFresh,
  invalidateAggregateDecision,
  isReadFailure,
  merge,
  processAggregateHead,
  reportAggregateDecision,
  reportDecision,
  resolveAggregateHeads,
  resolveTargets,
  __test: {
    evaluateCodex,
    evaluateRequiredChecks,
    hasResolutionMarker,
    latestRequiredState,
    parseReviewedCommit,
    reviewedCommitMatchesHead,
  },
};
