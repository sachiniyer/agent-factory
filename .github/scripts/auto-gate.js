const { randomUUID } = require("node:crypto");

const ALLOWED_AUTHORS = new Set(["sachiniyer", "app-detail-app", "app-detail-app[bot]"]);
const TUI_PATH_PREFIXES = ["app/", "ui/", "session/tmux/"];
// Every workflow whose master-side run is triggered by `push: branches:
// [master]`. A push made with GITHUB_TOKEN does not trigger further workflow
// runs — documented Actions behavior that exists to prevent recursion — so an
// auto-gate merge lands a commit none of these ever see, while the identical
// merge performed by a maintainer runs all four (#3435). A squash lands a tree
// neither the PR head nor the previous master ever had, so "the PR was green"
// is not the same claim.
//
// workflow_dispatch is the documented exception to that suppression: an event
// the gate raises with GITHUB_TOKEN *does* start a run. So the gate re-raises
// each of these by hand after it merges.
//
// This list is a literal copy of a trigger that cannot be computed, exactly
// like SELFTEST_PATHS in web-selftest-scope.js. auto-gate.test.js reads
// .github/workflows and fails if any workflow carries that push trigger without
// appearing here, or appears here without declaring workflow_dispatch — so the
// copy cannot rot silently.
const MASTER_PUSH_WORKFLOWS = ["build.yml", "docs.yml", "lint.yml", "web-selftest.yml"];
// docs.yml is the one entry that also *publishes*, and it decides WHETHER to
// publish for itself: the dispatch names the commit, never the paths. A copy of
// its deploy-path list here would be a second source of truth, and the kind that
// rots silently — it already omitted README.md, commands/docs_gen.go,
// requirements-docs.txt and scripts/gen-docs.sh, so an auto-gate merge touching
// any of those left Pages stale.
const DOCS_WORKFLOW = "docs.yml";
// GitHub check runs live on commits, but the underlying gate evidence is
// PR-scoped. The full (PR, head) pair is therefore part of every composite
// decision identifier. AUTO_GATE_DECISION_CHECK is the fixed-name aggregate
// required by the master ruleset: it passes only when every open master PR at
// the commit has its own passing composite decision.
const AUTO_GATE_DECISION_CHECK = "Auto Gate decision";
const AUTO_GATE_AGGREGATE_EXTERNAL_ID_PREFIX = "auto-gate:aggregate:head:";
const GITHUB_ACTIONS_APP_ID = 15368;
// The title the aggregate carries while a transaction is refreshing it. Also the
// marker resolveMergeRefusal matches to recognize a newer transaction that has
// taken ownership of the same head, so it is a constant rather than two copies.
const AGGREGATE_WAITING_TITLE = "WAITING: refreshing every PR/head decision at this commit";
// Every merge refusal Auto Gate concedes instead of failing on. Each one means
// "another actor won this head", not "this head cannot merge": the losing
// evaluation would otherwise paint a red Auto Gate run on master, the repo's
// highest-priority alarm, for an outcome that converged correctly.
//
// THIS LIST IS THE ONE PLACE the conceded shapes are named. Matching a shape is
// never sufficient on its own — resolveMergeRefusal concedes only against a
// second read proving another actor actually won — so a genuinely unmergeable
// head still fails loudly.
const CONCEDED_MERGE_REFUSALS = [
  // #3324: the ruleset refuses the write because a competing merge advanced
  // master past the required up-to-date check between evaluation and merge.
  { status: 405, pattern: /Repository rule violations found/i },
  { status: 405, pattern: /not mergeable/i },
  // #3434: a merge for this PR is already in flight. #3379 made the maintainer
  // merge path a normal outcome rather than a rarity, so "a human merges while
  // the gate is mid-evaluation on the same head" is the designed flow now.
  //
  // `settling` because this refusal names a merge that has STARTED, not one that
  // has finished: the confirming read can race ahead of the winner and see a PR
  // that is still open. It is the one shape that re-reads before concluding.
  { status: 405, pattern: /Merge already in progress/i, settling: true },
];
const CODEX_REVIEWER = "chatgpt-codex-connector[bot]";
const CODEX_REVIEW_RE = /\bCodex Review\b/i;
const CODEX_RATE_LIMIT_RE = /reached your Codex usage limits for code reviews/i;
const CODEX_BODY_FINDING_RE = /\bP[0-3]\b/i;
const REVIEWED_COMMIT_RE = /(?:\*\*Reviewed commit:\*\*|Reviewed commit:)\s*`([0-9a-f]{7,40})`/i;
// The second artifact shape. Codex emits the prose line above when a review is
// REQUESTED; when it reviews automatically on a push it only edits its summary
// comment, whose table names the commit in a cell (#3606). Both are the same
// reviewer saying the same thing about the same head, so both count — a PR whose
// final head was reviewed automatically otherwise blocks forever on a review
// that already ran, which is how every final head came to need a manual
// `@codex review`.
//
// A row is a verdict only when it is Completed AND names a commit AND carries a
// parseable time. `Running` is progress, not a verdict.
// The marker GitHub's Codex integration writes into its own persistent summary
// comment. Requiring it is what stops any body that merely CONTAINS a
// table-looking line — a review quoting this very format, which reviewing this
// gate does — from being read as a verdict artifact.
const CODEX_SUMMARY_MARKER = "<!-- codex-pull-request-review-summary -->";
const CODEX_SUMMARY_ROW_LABEL_RE = /\bCode Review\b/i;
const CODEX_SUMMARY_ROW_COMPLETED_RE = /\bCompleted\b/i;
const CODEX_SUMMARY_ROW_COMMIT_RE = /`([0-9a-f]{7,40})`/i;
const CODEX_SUMMARY_ROW_TIME_RE = /<relative-time[^>]*\bdatetime="([^"]+)"/i;
// The third spelling of "this artifact is about commit X": the SHAs in the
// artifact's own body. A finding posted as an ISSUE COMMENT carries no
// `commit_id` — that field exists only on reviews — and the finding shape Codex
// emits for lines outside the diff hunks carries no `Reviewed commit:` line
// either, so #3656 auto-merged with eight live P2s that the gate never
// inspected (#3670). Every one of those findings linked
// `blob/<40-hex>/docs/daemon-memory.md#L145`, naming the merged head outright;
// only the spelling was one no branch of the binding rule read.
//
// URL-form, 40-hex, deliberately. A bare backticked short SHA would also match
// the summary table's Commit CELL, and binding the summary comment to a head is
// precisely what #3606's rule forbids — see codexArtifactBindsToHead. The
// lookahead keeps a longer hex run from matching its first 40 characters.
const CODEX_BODY_COMMIT_RE =
  /\/(?:blob|blame|commit|commits|raw|tree)\/([0-9a-f]{40})(?![0-9a-f])/gi;
// What GitHub will serve from pulls.listCommits, however you paginate it.
// Reaching it means the list is a prefix of the PR's history, not the history.
const PR_COMMIT_LIST_CAP = 250;
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
// A merge that has already STARTED needs longer than a read retry to land.
// Reusing RETRY_DELAYS_MS gave the winner 1.25s total, and a slower merge then
// read back as "nobody merged" — refusing the concession this exists to grant.
const MERGE_SETTLE_DELAYS_MS = [1000, 2000, 4000];
const MAX_RATE_LIMIT_DELAY_MS = 10000;

// GitHub reads are side-effect-free, and check-run updates are idempotent when
// replayed by check_run_id. Check-run creates are different: external_id is
// correlation metadata, not a uniqueness key, so an ambiguous create is issued
// once and then reconciled by a per-attempt marker instead of being replayed.
// This is especially important for aggregate invalidation, which closes a
// stale-green safety window. Squash merge, workflow dispatch, and GraphQL
// mutations are also non-idempotent; keep those calls single-shot and never
// route them through these helpers.
async function retryRead(label, operation, subject = null) {
  return retryTransient(label, operation, {
    failureName: "AutoGateReadError",
    readFailure: true,
    subject,
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

async function retryTransient(label, operation, { failureName, readFailure, subject = null }) {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await operation();
    } catch (error) {
      // A NOT_FOUND for a node id this run resolved itself contradicts a fact
      // this run established, so it is answered rather than believed: confirm
      // the subject on a different code path before retrying (#3396).
      const selfContradictory = isSelfContradictoryNotFound(error, subject?.nodeId);
      if (selfContradictory && !(await subject.exists())) {
        throw subjectGone(subject, error);
      }
      if (!selfContradictory && !isRetryableGitHubError(error)) {
        throw error;
      }
      if (attempt >= RETRY_DELAYS_MS.length) {
        throw retryFailure(label, attempt + 1, error, failureName, readFailure);
      }
      await delay(retryDelayMilliseconds(error, RETRY_DELAYS_MS[attempt]));
    }
  }
}

// A NOT_FOUND naming a node id THIS RUN already resolved. GitHub answered with
// that id moments earlier, so the second answer cannot also be true; during the
// 2026-08-17 degradation it arrived on a REST read (`pulls/:n/reviews` returned
// HTTP 404 carrying a GraphQL NOT_FOUND body for the PR's own node id), which is
// not retryable by status and so escaped unhandled and reddened master.
//
// Deliberately narrow. NOT_FOUND stays a hard, loud error for every id the gate
// did not just resolve itself — the property #3346 established on purpose — and
// the id has to be named verbatim, so a NOT_FOUND about anything else is
// untouched by this.
function isSelfContradictoryNotFound(error, nodeId) {
  if (!nodeId || !error) {
    return false;
  }
  // The id must be the WHOLE id, not a prefix of a longer one: node ids are
  // opaque base64url, so a bare substring test would accept a NOT_FOUND about a
  // different object whose id merely starts with ours.
  const namesSubject = new RegExp(
    `(?<![A-Za-z0-9_-])${nodeId.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?![A-Za-z0-9_-])`,
  );
  // …and the id has to be named by the SAME error record that says NOT_FOUND.
  // Checking the whole envelope would pair a NOT_FOUND about one object with our
  // id appearing in a different error of a multi-error response, which proves
  // nothing about the object this run resolved.
  const notFoundNamingSubject = (record) =>
    String(record?.type || "").toUpperCase() === "NOT_FOUND" &&
    namesSubject.test(String(record?.message || ""));

  const body = error.response?.data;
  const records = [
    ...graphQLResponseErrors(error),
    ...(Array.isArray(body?.errors) ? body.errors : []),
    ...(body && typeof body === "object" && !Array.isArray(body) ? [body] : []),
  ];
  if (records.length > 0) {
    // Structured records exist, so they ARE the evidence — the transport message
    // is only their serialization, and consulting it as a fallback would re-admit
    // through the back door the very split pairing the per-record test rejects.
    return records.some(notFoundNamingSubject);
  }
  // No structured record at all: the transport message is the only record there
  // is, so it must both be a 404 and name the id itself.
  const status = Number(error.status ?? error.response?.status);
  return status === 404 && namesSubject.test(String(error.message || ""));
}

// The cross-check said the subject really is gone. That is a conclusion, not a
// read failure: a PR deleted or transferred mid-run cannot be evaluated, and
// evaluate() finishes cleanly on it rather than reporting an evaluation error.
function subjectGone(subject, error) {
  const gone = new Error(`PR #${subject.number} no longer exists`);
  gone.autoGatePullRequestGone = true;
  gone.prNumber = subject.number;
  gone.cause = error;
  return gone;
}

// The PR this run already resolved, carried by every read performed afterwards.
// exists() deliberately uses REST while getPullRequest used GraphQL: a
// cross-check on the same code path that just lied proves nothing.
function resolvedPullRequest({ github, context, pr }) {
  const { owner, repo } = context.repo;
  return {
    nodeId: pr.id,
    number: pr.number,
    exists: async () => {
      try {
        await github.rest.pulls.get({ owner, repo, pull_number: pr.number });
        return true;
      } catch (error) {
        // Only a definite 404 is evidence of absence. Any other answer — a 500,
        // a rate limit, a network failure — is an unknown, and an unknown must
        // not be read as "the PR is gone".
        return Number(error?.status ?? error?.response?.status) !== 404;
      }
    },
  };
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
  // A publish precondition that failed is not a failed write. Retrying it under
  // the write's schedule multiplies its own retries and, worse, relabels it as a
  // write error — losing the read-failure marker the caller needs to publish a
  // clean BLOCKED aggregate instead of failing the job.
  if (error?.autoGateGuardFailure) {
    return false;
  }
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
    // A PR that no longer exists is a conclusion, not an evaluation failure: it
    // cannot be evaluated and there is nothing to report on it. isOpen false
    // leaves any existing decision untouched (#3396).
    if (error?.autoGatePullRequestGone) {
      core.notice(error.message);
      return finish(core, setOutputs, {
        prNumber: String(error.prNumber || prNumber || ""),
        shouldMerge: false,
        isOpen: false,
        docsChanged: false,
        reasons: [error.message],
        notes: [],
      });
    }
    const message = formatError(error);
    const warning = isReadFailure(error) ? message : error?.stack || message;
    core.warning(warning);
    return finish(core, setOutputs, {
      prNumber: prNumber ? String(prNumber) : "",
      shouldMerge: false,
      isOpen: false,
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

  // Everything below reads about a PR this run has now resolved, so each read
  // carries that identity and can tell a self-contradictory NOT_FOUND from a
  // real one (#3396).
  const subject = resolvedPullRequest({ github, context, pr });
  const files = await listPullRequestFiles({ github, context, number: pr.number, subject });
  // The dispatch set this run will use comes from master's copy of this file, so
  // a merge that CHANGES workflow definitions may be re-raising a stale set —
  // most sharply when it adds a push-gated workflow, which cannot be in the list
  // the running copy holds.
  const workflowsChanged = files.some((path) => path.startsWith(".github/workflows/"));
  // A `_test.go` file is not compiled into the shipped binary, so it cannot
  // change what a user sees — and the label this gate demands is a claim that
  // someone drove the TUI and looked. #3601's only file under these prefixes was
  // `ui/config_pane_test.go`, and the lane had to run a play-test to satisfy a
  // gate for a diff with nothing to look at. The subtraction is per FILE, not
  // per PR: a production file under any prefix still requires the label, and so
  // does a diff that changes a test and a production file together.
  const touchesTui = files.some(
    (path) => !path.endsWith("_test.go") && TUI_PATH_PREFIXES.some((prefix) => path.startsWith(prefix)),
  );
  const labels = new Set(pr.labels.map((label) => label.toLowerCase()));

  if (touchesTui && !labels.has("play-tested")) {
    reasons.push("PR touches visible TUI/pane paths and is missing the play-tested label");
  } else if (touchesTui) {
    notes.push("TUI path gate passed with play-tested label");
  } else {
    notes.push("TUI path gate not required");
  }

  const requiredChecks = await evaluateRequiredChecks({
    github,
    context,
    branch: pr.baseRefName,
    sha: pr.headRefOid,
    core,
    subject,
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
    subject,
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
  // The manual path exists so branch protection does not sit red on a PR this
  // gate will never merge itself. It was passing the required check for EVERY
  // blocker, which made "the author is external" waive a live Codex finding and
  // let three hand merges ship one (#3534, #3545, #3546 → #3555, #3557, #3553).
  //
  // A finding is a claim about the CODE. It does not depend on who opened the
  // PR, and the degradation above already refuses to let "the reviewer is down"
  // waive one — this is the same rule, applied to the branch that skipped it.
  //
  // Findings ONLY. The other unmet requirements stay notes here, deliberately: a
  // live finding is cleared per-thread by a RESOLVED / ACCEPTED / [gate-ack]
  // reply the maintainer already posts, so blocking on one leaves an exit. A
  // missing play-tested label or an absent verdict has no such per-item answer
  // on a PR whose author does not iterate, and blocking on those would turn the
  // manual path into a stop with no way out — the failure mode the reviewer
  // degradation was written to avoid.
  const manualMergeBlockers = manualMergeRequired ? codex.findingBlockers ?? [] : [];

  return finish(core, setOutputs, {
    prNumber: String(pr.number),
    pullRequestId: pr.id,
    shouldMerge: !manualMergeRequired && reasons.length === 0,
    manualMergeRequired,
    manualMergeReasons,
    manualMergeBlockers,
    degradedForUnavailableReviewer,
    isOpen: pr.state === "OPEN" && !pr.merged,
    baseRefName: pr.baseRefName,
    headRefName: pr.headRefName,
    headRepository: pr.headRepository,
    headSha: pr.headRefOid,
    workflowsChanged,
    reasons,
    notes,
  });
}

async function reportDecision({ github, context, core, result, manual = false }) {
  // This runs after the PR was resolved, so its reads carry that identity for the
  // same reason evaluatePullRequest's do: without it a self-contradictory
  // NOT_FOUND arriving here is non-retryable and rethrows unhandled, reddening
  // the run over a PR the gate resolved seconds earlier (#3396).
  const subject =
    result?.pullRequestId && Number(result.prNumber) > 0
      ? resolvedPullRequest({
          github,
          context,
          pr: { id: result.pullRequestId, number: Number(result.prNumber) },
        })
      : null;
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
    subject,
  );
  const priorDecision = checkRuns
    .filter(
      (run) =>
        run.name === identity.checkName &&
        run.external_id === identity.externalId &&
        run.app?.id === GITHUB_ACTIONS_APP_ID,
    )
    .sort((left, right) => latestRunTime(right) - latestRunTime(left))[0];
  // A manual-merge PR passes the required check only while nothing the
  // maintainer must answer first is outstanding (#3558).
  const manualMergeBlockers = result.manualMergeBlockers || [];
  const manualMergePasses = result.manualMergeRequired && manualMergeBlockers.length === 0;
  const decisionPasses = result.shouldMerge || manualMergePasses;
  const state =
    manual && !priorDecision
      ? "never-ran"
      : result.manualMergeRequired
        ? manualMergePasses
          ? "manual"
          : "manual-blocked"
        : result.shouldMerge
          ? "pass"
          : "waiting";
  const title =
    state === "never-ran"
      ? `NEVER_RAN: no prior decision; recovery ${decisionPasses ? "passed" : "is waiting"}`
      : result.manualMergeRequired
        ? manualMergePasses
          ? result.degradedForUnavailableReviewer
            ? "PASS: reviewer usage-limited; maintainer review and manual merge required"
            : "PASS: maintainer review and manual merge required"
          : "BLOCKED: a manual merge still requires every live Codex finding to be answered"
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
      title: AGGREGATE_WAITING_TITLE,
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

// Establish several publish preconditions as ONE observation.
//
// Issuing them concurrently is not enough: each performs a GitHub read, and if
// one of those retries, the other's answer is already stale by that backoff when
// Promise.all finally resolves. So the reads are single-shot here, and a
// transient failure discards the WHOLE round and re-reads everything — the
// answers that count all come from the same round, and none can be older than
// one request's latency, which is the floor.
//
// A precondition that is genuinely violated — ownership lost, auto-merge still
// armed, an unevaluated PR in the set — is not retryable and stops immediately.
async function establishPublishPreconditions(label, preconditions) {
  for (let attempt = 0; ; attempt += 1) {
    const settled = await Promise.allSettled(preconditions.map((precondition) => precondition()));
    const rejected = settled.filter((outcome) => outcome.status === "rejected");
    if (rejected.length === 0) {
      return;
    }
    const violated = rejected.find((outcome) => !isRetryableGitHubError(outcome.reason));
    if (violated) {
      throw violated.reason;
    }
    if (attempt >= RETRY_DELAYS_MS.length) {
      throw retryFailure(label, attempt + 1, rejected[0].reason, "AutoGateReadError", true);
    }
    // The longest delay any rejection asked for, not the first one's. Every
    // rejected precondition has to succeed in the next round, so honouring only
    // rejected[0] can retry the whole round inside another's throttle window and
    // burn all three attempts on a condition that was merely rate-limited.
    await delay(
      Math.max(
        ...rejected.map((outcome) =>
          retryDelayMilliseconds(outcome.reason, RETRY_DELAYS_MS[attempt]),
        ),
      ),
    );
  }
}

// Re-establish that this transaction still owns the aggregate on this head.
// Aggregate invalidation runs outside the head-keyed serialized lane, so a newer
// event can take ownership at any moment — including during a write's backoff.
async function assertStillOwnsAggregate({ github, context, headSha, checkRunId, retry = true }) {
  const { owner, repo } = context.repo;
  const identity = aggregateIdentity(headSha);
  const read = () =>
    github.paginate(github.rest.checks.listForRef, { owner, repo, ref: headSha, per_page: 100 });
  const checkRuns = retry
    ? await retryRead(`could not re-read aggregate checks at ${headSha}`, read)
    : await read();
  const latestGeneration = newestCheckGeneration(
    checkRuns.filter(
      (run) =>
        run.name === identity.checkName &&
        run.external_id === identity.externalId &&
        run.app?.id === GITHUB_ACTIONS_APP_ID,
    ),
  );
  if (latestGeneration?.id !== checkRunId) {
    const superseded = new Error(
      `A newer Auto Gate event invalidated ${headSha} during the write; this older ` +
        "transaction will not publish PASS.",
    );
    superseded.autoGateAssociationChanged = true;
    throw superseded;
  }
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
    beforePublish: aggregate.ok
      ? async () => {
          // Both preconditions, CONCURRENTLY, on every attempt.
          //
          // Concurrent because ordering them makes whichever runs first stale by
          // the other's duration — and each performs its own retrying read, so
          // that duration can include a backoff. Sequenced either way, an event
          // landing inside the second read's window is invisible to the first.
          // Issued together, neither observation is stale relative to the other,
          // and the only interval left is the write itself.
          //
          // On every attempt because a retried write sleeps and reissues: the
          // generation check at the top of reportAggregateDecision covers the
          // moment the decision was built, not the moment each attempt writes.
          await establishPublishPreconditions(
            `could not establish the publish preconditions for ${aggregate.headSha}`,
            [
              ...(checkRunId
                ? [
                    () =>
                      assertStillOwnsAggregate({
                        github,
                        context,
                        headSha: aggregate.headSha,
                        checkRunId,
                        retry: false,
                      }),
                  ]
                : []),
              ...(typeof beforePublish === "function"
                ? [() => beforePublish({ pullNumbers: aggregate.pullNumbers, retry: false })]
                : []),
            ],
          );
        }
      : undefined,
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
      // The PR vanished between its evaluation and its decision write. evaluate()
      // converts this marker into a clean conclusion, but reportDecision's reads
      // are outside it, so without this the same deletion reddens the run from a
      // few lines further down (#3396).
      if (error?.autoGatePullRequestGone) {
        core.notice(
          `Keeping aggregate ${pending.headSha} non-green because PR #${prNumber} ` +
            "no longer exists.",
        );
        return { state: "association-changed", pending };
      }
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
      beforePublish: ({ pullNumbers, retry }) =>
        confirmNativeAutoMergeDisabled({
          github,
          context,
          results: manualMergeResults,
          evaluated: pending.pullNumbers.map(Number),
          pullNumbers,
          retry,
        }),
    });
  } catch (error) {
    // The head changed under this transaction — a newer generation took
    // ownership, or a PR joined the head after it was evaluated. Neither is a
    // failure: the aggregate stays non-green and the event that caused the
    // change owns the next evaluation.
    if (error?.autoGateAssociationChanged) {
      core.notice(`Keeping aggregate ${pending.headSha} non-green: ${error.message}`);
      return { state: "association-changed", pending };
    }
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
      // A newer-owner concession is the ONE case that must not write. It is
      // reached only because a live transaction already made this aggregate
      // non-green, so invalidating again would create a WAITING generation newer
      // than the winner's: the winner then sees itself superseded and refuses to
      // publish, while nothing is left to finish the generation this run just
      // created, and the head stays red and unmerged until an unrelated event.
      //
      // A "merged" concession is NOT that case. Nothing has invalidated the head,
      // and the PASS this transaction published a moment ago is still standing —
      // which any OTHER PR sharing this head inherits as authorization built on
      // a master that has since advanced. Worse, a token-authenticated winning
      // merge raises no event, so nothing would come along to repair it. That
      // one falls through to the ordinary invalidation below, exactly as a
      // successful merge does, and for the same reason.
      // Both reasons mean the same thing to this catch: another actor's outcome
      // stands and this run must not write to the aggregate — one because a live
      // transaction owns it, one because the read that would have proved
      // otherwise failed.
      if (
        error?.autoGateConcessionReason === "newer-owner" ||
        error?.autoGateConcessionReason === "merged-owner-unknown"
      ) {
        core.notice(message);
        return { state: "conceded", pending, aggregate };
      }
      // Ownership could not be read. Skip the write — a blind invalidation would
      // supersede whichever transaction owns this head — but do NOT swallow the
      // failure: nothing proved another actor won, the PR is still open, and the
      // merge did not happen. Conceding here would leave the aggregate's PASS
      // standing for a merge that never occurred.
      if (error?.autoGateOwnershipUnknown) {
        core.warning(
          `Not invalidating ${pending.headSha}: ownership could not be determined ` +
            `(${error.autoGateOwnershipUnknown}), so this run will not overwrite whichever ` +
            "transaction owns it. The merge refusal below stands.",
        );
        throw error;
      }
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
    // after the first — long enough for the world to change during the backoff
    // and be consumed by the green the retry then publishes. Each attempt
    // re-establishes it.
    let attempt = 0;
    const guard = async () => {
      if (typeof beforePublish !== "function") {
        attempt += 1;
        return;
      }
      try {
        await beforePublish({ attempt });
      } catch (error) {
        // Re-thrown as a guard failure so the write's retry classifier lets it
        // out immediately, with the read-failure marker preserved for the caller.
        const guardFailure = new Error(error?.message || String(error));
        guardFailure.name = "AutoGateGuardError";
        guardFailure.autoGateGuardFailure = true;
        guardFailure.autoGateReadFailure = isReadFailure(error);
        guardFailure.autoGateAssociationChanged = error?.autoGateAssociationChanged === true;
        guardFailure.cause = error;
        throw guardFailure;
      }
      attempt += 1;
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

  // Re-raise every gate the merge suppressed. This runs unconditionally rather
  // than replicating each workflow's `paths:` filter: a dispatch ignores those
  // filters anyway, and a second copy of four path lists is precisely the thing
  // that rots. The cost is a few runner-minutes on a merge that could not have
  // broken them; the alternative cost is master carrying an unverified commit.
  for (const workflowId of MASTER_PUSH_WORKFLOWS) {
    try {
      // Single-shot, deliberately: a dispatch is a write, and retrying one that
      // may already have been accepted starts a duplicate run — for docs.yml, a
      // duplicate Pages deploy. A failure is recorded and fails the job instead.
      await github.rest.actions.createWorkflowDispatch({
        owner,
        repo,
        workflow_id: workflowId,
        // A branch ref, because workflow_dispatch takes no SHA. master is the
        // commit this merge just created; if a later merge has already advanced
        // it, that newer commit is the one worth verifying — verification is
        // monotonic, since the newer tree contains this one.
        ref: "master",
        // Publishing is NOT monotonic, and docs.yml publishes. A run raised for
        // this merge but started after a later one would diff that later
        // commit's paths, find no docs change, and never publish these docs —
        // while the later merge's own run does the same. So the deploy decision
        // is pinned to the commit that raised the run. The gate names the
        // commit; docs.yml still owns which paths mean "deploy".
        ...(workflowId === DOCS_WORKFLOW
          ? { inputs: { verify_sha: response.data.sha } }
          : {}),
      });
      // Name the commit this merge produced. A dispatch takes a branch ref and
      // master can in principle advance first, so the notice is what lets a
      // human correlate the run with the merge it was raised for.
      core.notice(
        `Dispatched ${workflowId} on master to verify the PR #${prNumber} merge ` +
          `${response.data.sha}.`,
      );
    } catch (error) {
      // Keep going: one unavailable workflow must not cost master the other three.
      postMergeErrors.push(error);
    }
  }
  // auto-gate.yml pins every checkout to the default branch, so the loop above
  // ran from master's PRE-merge copy of MASTER_PUSH_WORKFLOWS. A merge that adds
  // a push-gated workflow therefore cannot re-raise it for its own landing
  // commit; every later merge picks it up. Nothing here can close that — deriving
  // the set from the merged tree means re-parsing workflow triggers at merge
  // time, where a misparse either skips a dispatch silently or reds a merge that
  // has already landed. What it can do is stop the gap being invisible.
  if (gate.workflowsChanged) {
    core.warning(
      `PR #${prNumber} changed workflow definitions, and this run re-raised the set known to ` +
        "master's pre-merge copy of the gate. If it added or renamed a workflow gated on a " +
        "master push, that one did not run for this commit; dispatch it by hand.",
    );
  }

  try {
    await deleteMergedHeadRef({ github, context, core, gate, prNumber });
  } catch (error) {
    postMergeErrors.push(error);
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

// Decide whether a refused merge is a lost race the gate concedes, or a real
// refusal it must fail on. Returns { reason, message } to concede with, or null
// to rethrow.
//
// The reason is load-bearing, not a label. A "newer-owner" concession means a
// live transaction already owns this head and this run must not write to the
// aggregate at all; a "merged" concession means the PR is gone and any OTHER PR
// still sharing the head must not keep the PASS this transaction published.
//
// This lived inline in auto-gate.yml, where it could not be tested — which is
// how "Merge already in progress" reached the generic unhandled path and reddened
// a master run for a merge that had converged correctly (#3434). It is here so
// the conceded shapes and the evidence each one demands are one tested unit.
// A read whose failure means "no evidence", never "a new error". Every caller
// here is looking for proof that another actor won; without proof the original
// refusal is rethrown, which is the loud path and the correct default.
async function readOrNull(operation) {
  try {
    return await operation();
  } catch {
    return null;
  }
}

async function resolveMergeRefusal({ github, error, options, ownedAggregateCheck }) {
  const status = Number(
    error?.status ?? error?.response?.status ?? error?.response?.data?.status,
  );
  const message = error?.response?.data?.message || error?.message || "";
  const refusal = CONCEDED_MERGE_REFUSALS.find(
    (shape) => status === shape.status && shape.pattern.test(message),
  );
  if (!refusal) {
    return null;
  }

  const { owner, repo, pull_number: prNumber, sha } = options;
  const expectedHeadSha = String(sha || "").toLowerCase();

  // A live transaction owning this head OUTRANKS merged evidence, and the two
  // are not mutually exclusive: a shared head can have this PR merged AND a
  // newer transaction mid-flight. Labelling that "merged" would send the caller
  // down the invalidation path and supersede the active winner, which is the
  // ownership theft the newer-owner branch exists to avoid. So the merged branch
  // asks this question before it answers.
  const newerOwnerConcession = async () => {
    // Both halves of this run's own generation must be known first. Without them
    // there is no "newer" to establish, and treating the unknown as generation
    // zero makes every check on the head — including the ones this very
    // transaction created — look like a later owner, i.e. concedes everything.
    // An unknown generation is not a proven loss.
    const ownedCreatedAt = Date.parse(ownedAggregateCheck?.created_at || "");
    if (!Number.isFinite(ownedCreatedAt) || !Number.isFinite(Number(ownedAggregateCheck?.id))) {
      return null;
    }
    const aggregateExternalId = `${AUTO_GATE_AGGREGATE_EXTERNAL_ID_PREFIX}${expectedHeadSha}`;
    // NOT readOrNull: an empty list here reads as "nobody owns this head", and
    // the caller acts on that by creating another invalidation — which supersedes
    // the very transaction this read failed to see. "No answer" and "no owner"
    // must not be the same value, so this retries and then reports the unknown.
    let aggregateChecks;
    try {
      aggregateChecks = await retryRead(`could not read aggregate checks at ${expectedHeadSha}`, () =>
        github.paginate(github.rest.checks.listForRef, {
          owner,
          repo,
          ref: expectedHeadSha,
          filter: "all",
          per_page: 100,
        }),
      );
    } catch (readError) {
      // NOT a concession. Nothing here proved another actor won: the PR is still
      // open and the merge did not happen, so exiting successfully would leave
      // the aggregate's PASS standing for a merge that never occurred. The
      // refusal must stay loud.
      //
      // What the unknown DOES change is the invalidation: creating a newer
      // generation blind would supersede whichever transaction owns this head,
      // which is the damage the ownership read exists to avoid. So the original
      // error is marked and rethrown, and the caller skips the write only.
      error.autoGateOwnershipUnknown = formatError(readError);
      return null;
    }
    const newerOwner = aggregateChecks
      .filter((check) => {
        const createdAt = Date.parse(check.created_at || "") || 0;
        const newerGeneration =
          createdAt > ownedCreatedAt ||
          (createdAt === ownedCreatedAt && Number(check.id) > Number(ownedAggregateCheck.id));
        return (
          newerGeneration &&
          check.name === AUTO_GATE_DECISION_CHECK &&
          check.external_id === aggregateExternalId &&
          check.app?.id === GITHUB_ACTIONS_APP_ID &&
          check.conclusion === "failure" &&
          check.output?.title?.startsWith(AGGREGATE_WAITING_TITLE)
        );
      })
      .sort((left, right) => {
        const createdDifference =
          (Date.parse(right.created_at || "") || 0) - (Date.parse(left.created_at || "") || 0);
        return createdDifference || Number(right.id) - Number(left.id);
      })[0];
    if (!newerOwner) {
      return null;
    }
    return {
      reason: "newer-owner",
      message:
        `Conceding merge-refused race for PR #${prNumber}: the winning outcome ` +
        `is newer Auto Gate check ${newerOwner.html_url || `#${newerOwner.id}`} ` +
        `owning ${expectedHeadSha} (${newerOwner.output.title}).`,
    };
  };

  // The PR is merged, so the outcome this run wanted already happened. A settling
  // refusal gets a bounded re-read because the winning merge may still be in
  // flight; exhausting it concludes "nobody merged", which is a refusal to
  // concede rather than a reason to wait longer.
  //
  // A failed confirmation is "no evidence yet", not a new error to raise. If it
  // threw, a 500 on the first read would replace the merge refusal with a read
  // error AND abandon the remaining settlement window — so the winning merge
  // could land inside the window this loop exists to wait through, and the run
  // would still fail with the red master alarm this is meant to prevent.
  const settleDelays = refusal.settling ? MERGE_SETTLE_DELAYS_MS : [];
  for (let attempt = 0; attempt <= settleDelays.length; attempt += 1) {
    if (attempt > 0) {
      await delay(settleDelays[attempt - 1]);
    }
    const pull = await readOrNull(() =>
      github.rest.pulls.get({ owner, repo, pull_number: prNumber }),
    );
    if (pull?.data?.merged) {
      const owned = await newerOwnerConcession();
      if (owned) {
        return owned;
      }
      const winner =
        `Conceding merge-refused race for PR #${prNumber}: the winning outcome ` +
        `already merged ${pull.data.merge_commit_sha || expectedHeadSha}.`;
      // Merged, but the ownership read failed — and a `||` fallback here would
      // discard that, answering "merged" and sending the caller into the generic
      // invalidation. Both facts have to survive: the race IS conceded (the PR is
      // merged, so the outcome converged), and the aggregate must NOT be written,
      // because a newer generation created blind would supersede whichever
      // transaction owns this shared head.
      if (error.autoGateOwnershipUnknown) {
        return {
          reason: "merged-owner-unknown",
          message:
            `${winner} Not invalidating ${expectedHeadSha}: ownership could not be determined ` +
            `(${error.autoGateOwnershipUnknown}), so this run will not overwrite whichever ` +
            "transaction owns it.",
        };
      }
      return { reason: "merged", message: winner };
    }
  }

  // Not merged. A newer transaction may still own the head.
  const newerOwner = await newerOwnerConcession();
  if (newerOwner) {
    return newerOwner;
  }

  // No winner. The head is genuinely unmergeable and the run must stay loud.
  return null;
}

// Delete the merged head branch. The repository's delete_branch_on_merge setting
// does not fire for a GITHUB_TOKEN merge, so every auto-gate merge left its
// branch behind and origin regrew to 201 (#3603).
//
// Three conditions, each protecting something different, and every one of them
// no-ops rather than failing: this runs after the merge has landed, and nothing
// here is worth reddening a merge that already succeeded.
async function deleteMergedHeadRef({ github, context, core, gate, prNumber }) {
  const { owner, repo } = context.repo;
  const branch = gate.headRefName;
  // (a) A fork's branch is not ours to delete, and the token cannot anyway.
  if (!branch || gate.headRepository !== `${owner}/${repo}`) {
    return;
  }
  try {
    // (c) first, because it is the only condition whose answer cannot change in
    // a way that makes deleting safe — and because every read placed between the
    // SHA check and the delete widens the window in which the branch can move.
    //
    // Two shapes: a PR based ON this branch, which deleting would close, and a
    // PR whose HEAD is this branch targeting some other base, which deleting
    // would leave headless. The merged PR itself is closed by now, so an open
    // head-PR here is genuinely a different one.
    const [dependents, siblings] = await Promise.all([
      github.paginate(github.rest.pulls.list, {
        owner,
        repo,
        base: branch,
        state: "open",
        per_page: 100,
      }),
      github.paginate(github.rest.pulls.list, {
        owner,
        repo,
        head: `${owner}:${branch}`,
        state: "open",
        per_page: 100,
      }),
    ]);
    const blocking = [...dependents, ...siblings].map((pull) => pull.number);
    if (blocking.length > 0) {
      core.notice(
        `Keeping ${branch}: open PR ${joinPullNumbers([...new Set(blocking)])} still uses it.`,
      );
      return;
    }

    // (b) The ref must still point at the commit that was merged. A lane that
    // pushed after the merge keeps its branch — that work is not in master, and
    // the pushed ref may be the only copy of it anywhere.
    //
    // This is the LAST read before the delete, deliberately, because the delete
    // itself cannot carry the condition: neither the REST ref API nor GraphQL's
    // deleteRef accepts an expected OID (introspected), so the pair cannot be
    // made atomic from here. Ordering is the only lever this side of pushing a
    // lease with git, and it is used.
    const ref = await github.rest.git.getRef({ owner, repo, ref: `heads/${branch}` });
    const tip = String(ref?.data?.object?.sha || "").toLowerCase();
    if (tip !== String(gate.headSha || "").toLowerCase()) {
      core.notice(
        `Keeping ${branch}: it now points at ${tip || "an unreadable commit"}, not the merged ` +
          `${gate.headSha}, so it carries work this merge did not take.`,
      );
      return;
    }

    await github.rest.git.deleteRef({ owner, repo, ref: `heads/${branch}` });
    core.notice(`Deleted merged head branch ${branch} for PR #${prNumber}.`);
  } catch (error) {
    // Already gone, or the ref moved under us between the read and the delete.
    // Both mean there is nothing left to prune, which is the desired end state.
    const status = Number(error?.status ?? error?.response?.status);
    if (status === 404 || status === 422) {
      core.notice(`Nothing to prune for ${branch}: ${formatError(error)}.`);
      return;
    }
    throw error;
  }
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
    headRefName: pr.headRefName,
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
async function confirmNativeAutoMergeDisabled({ github, context, results, evaluated, pullNumbers, retry = true }) {
  // The set this transaction guarded was frozen from its own association
  // snapshot, but the aggregate re-reads associations before publishing and can
  // pass for a LARGER set — a PR reopened or retargeted onto this head, carrying
  // a passing decision from an earlier transaction, is in the green this write is
  // about to publish and was never guarded here. Refuse rather than guess: this
  // transaction did not evaluate it and cannot speak for its auto-merge state.
  if (Array.isArray(pullNumbers)) {
    const unevaluated = pullNumbers.filter((number) => !evaluated.includes(Number(number)));
    if (unevaluated.length > 0) {
      const changed = new Error(
        `Refusing to publish: PR ${joinPullNumbers(unevaluated)} joined this head after it was ` +
          "evaluated, so this transaction cannot vouch for its auto-merge state",
      );
      changed.autoGateAssociationChanged = true;
      throw changed;
    }
  }
  const numbers = results.map((result) => Number(result.prNumber));
  const states = await readNativeAutoMergeStates({ github, context, numbers, retry });
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
async function readNativeAutoMergeStates({ github, context, numbers, retry = true }) {
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
  // Single-shot when it is a publish precondition: retrying inside one
  // observation makes the other stale by this backoff (see
  // establishPublishPreconditions).
  const response = retry
    ? await retryRead(label, () => github.graphql(query, variables))
    : await github.graphql(query, variables);
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

async function listPullRequestFiles({ github, context, number, subject = null }) {
  const { owner, repo } = context.repo;
  const files = await retryRead(
    `could not list files for PR #${number}`,
    () =>
      github.paginate(github.rest.pulls.listFiles, {
        owner,
        repo,
        pull_number: number,
        per_page: 100,
      }),
    subject,
  );
  // A rename touches BOTH paths, and the API reports the old one only as
  // `previous_filename`. Keeping just `filename` loses the fact that a file was
  // REMOVED from where it used to be, which every path predicate below reads as
  // "nothing there changed" — sharpest for the TUI gate, where renaming
  // `ui/pane.go` to `ui/pane_test.go` takes a production file out of the shipped
  // binary while leaving one path that ends in `_test.go`.
  return files.flatMap((file) =>
    file.previous_filename && file.previous_filename !== file.filename
      ? [file.filename, file.previous_filename]
      : [file.filename],
  );
}

async function evaluateRequiredChecks({ github, context, branch, sha, core, subject = null }) {
  const required = await getRequiredCheckSpecs({ github, context, branch, core, subject });
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
  const checkRuns = await retryRead(
    `could not read check runs at commit ${sha}`,
    () =>
      github.paginate(github.rest.checks.listForRef, {
        owner,
        repo,
        ref: sha,
        per_page: 100,
      }),
    subject,
  );
  const statuses = await retryRead(
    `could not read commit statuses at ${sha}`,
    () =>
      github.paginate(github.rest.repos.listCommitStatusesForRef, {
        owner,
        repo,
        ref: sha,
        per_page: 100,
      }),
    subject,
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

async function getRequiredCheckSpecs({ github, context, branch, core, subject = null }) {
  const { owner, repo } = context.repo;
  const specs = new Map();
  const errors = [];

  try {
    const response = await retryRead(
      `could not read branch rules for ${branch}`,
      () =>
        github.request("GET /repos/{owner}/{repo}/rules/branches/{branch}", {
          owner,
          repo,
          branch,
        }),
      subject,
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
    // A 404 here normally means "this repository does not use that mechanism",
    // which is why it is swallowed. A retry-exhausted read failure carries status
    // 404 too, and swallowing THAT is a fail-open: with neither mechanism
    // readable the gate reports no required checks at all, and a PR with nothing
    // green can pass. An unreadable ruleset is not an absent one.
    if (isReadFailure(error) || error?.autoGatePullRequestGone) {
      throw error;
    }
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
      subject,
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
    // A 404 here normally means "this repository does not use that mechanism",
    // which is why it is swallowed. A retry-exhausted read failure carries status
    // 404 too, and swallowing THAT is a fail-open: with neither mechanism
    // readable the gate reports no required checks at all, and a PR with nothing
    // green can pass. An unreadable ruleset is not an absent one.
    if (isReadFailure(error) || error?.autoGatePullRequestGone) {
      throw error;
    }
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

async function evaluateCodex({ github, context, number, sha, lastCommitDate, subject = null }) {
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

  const comments = await retryRead(
    `could not read issue comments for PR #${number}`,
    () =>
      github.paginate(github.rest.issues.listComments, {
        owner,
        repo,
        issue_number: number,
        per_page: 100,
      }),
    subject,
  );
  const reviews = await retryRead(
    `could not read reviews for PR #${number}`,
    () =>
      github.paginate(github.rest.pulls.listReviews, {
        owner,
        repo,
        pull_number: number,
        per_page: 100,
      }),
    subject,
  );
  const codexComments = comments.filter((comment) => comment.user?.login === CODEX_REVIEWER);
  const codexReviewArtifacts = [...codexComments, ...reviews]
    .filter((comment) => comment.user?.login === CODEX_REVIEWER)
    .sort((a, b) => reviewArtifactTime(b) - reviewArtifactTime(a));
  // Both artifact shapes, each carrying its OWN time: the prose line's is its
  // comment's, the summary row's is the row's. Sorted by that rather than by
  // comment order, because the summary comment is edited on every review
  // activity and its comment time says nothing about when this review completed.
  const matchingReviewArtifacts = codexReviewArtifacts
    .map((artifact) => parseVerdictArtifact(artifact, sha))
    .filter(Boolean)
    .sort((left, right) => right.time - left.time);
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
    // Split, because the two states need different actions from a reader: one
    // says wait for or request a review, the other says a review ran and this
    // gate could not read it — go look at the artifact, not at Codex.
    const missingVerdictReason = summaryNamesHead(codexReviewArtifacts, sha)
      ? `a Codex review exists for head ${sha} but carried no parseable verdict${suffix}`
      : `Codex has not reviewed head ${sha} yet${suffix}`;
    if (reviewerUnavailable) {
      reviewerUnavailableReason = missingVerdictReason;
    }
    reasons.push(missingVerdictReason);
  } else {
    // The artifact's own time: the comment's for a prose line, the row's for a
    // summary row.
    const verdictTime = verdict.time;
    if (lastPushTime == null || verdictTime === 0 || verdictTime <= lastPushTime) {
      reasons.push("Codex verdict for the head commit is older than the head commit timestamp");
    } else {
      notes.push(`Codex verdict matches head ${sha}`);
    }
  }

  // Findings are read from artifacts BOUND to this head, which is a wider set
  // than the verdict artifacts: an automatic review can carry a P0-P3 in its body
  // with no `Reviewed commit:` line at all, and it is bound to the head by its
  // own commit_id. Leaving those out let a Completed summary row pass while the
  // finding beside it was never inspected.
  //
  // A summary row is deliberately NOT in this set. It records that a review
  // completed, not that it was clean, and its body never carries findings — so
  // letting it be the newest finding-capable artifact would clear a finding by
  // the mere fact that Codex rewrote the table afterwards, which it does on every
  // review activity including posting that finding.
  //
  // Newest-wins is preserved among the bound artifacts, so a newer clean verdict
  // still supersedes an older body-only finding.
  // Declared here because a body finding is a finding: the manual-merge path
  // consumes findingBlockers, not reasons, so a finding recorded only in reasons
  // publishes a PASSING manual decision and a maintainer merges with it live —
  // exactly what #3591 closed for inline findings.
  const findingBlockers = [];
  const headBoundArtifacts = codexReviewArtifacts.filter((artifact) =>
    codexArtifactBindsToHead(artifact, sha),
  );
  const latestBoundArtifact = headBoundArtifacts[0];
  if (latestBoundArtifact && CODEX_BODY_FINDING_RE.test(latestBoundArtifact.body || "")) {
    const bodyFindingReason = "latest exact-head Codex review body contains a P0-P3 finding";
    reasons.push(bodyFindingReason);
    // The remedy has to be one that terminates. A body finding has no thread, so
    // no RESOLVED reply can clear it — it clears when a NEWER head-bound artifact
    // for this head is clean, which is what a fresh review produces. Advertising
    // a reply here would send the maintainer round a loop with no exit, the same
    // trap the unpushed-fix-claim remedy avoids.
    findingBlockers.push({
      reason: bodyFindingReason,
      remedy:
        "request a fresh `@codex review` so a newer clean verdict for this head supersedes it, " +
        "or push the fix — no reply can clear a finding that is not on a thread",
    });
  }

  // …and the artifacts that name NO commit at all block too (#3670).
  //
  // The rule above binds an artifact to a head by fields that only some artifact
  // shapes carry, and every shape it cannot bind was silently DROPPED: not
  // inspected, not reported, not counted against the merge. That is fail-open by
  // construction — the newest unanticipated artifact shape reopens the hole the
  // moment Codex invents one, which is exactly how the issue-comment finding
  // shape got past a rule written for reviews. Unknown is not clean.
  //
  // Scoped to artifacts that name no commit ANYWHERE, not to "not bound to this
  // head": an artifact naming some other commit is stale evidence about an older
  // head, which the head-bound rule already handles correctly and which must not
  // start blocking every PR that was ever reviewed twice.
  //
  // The remedy has to terminate, and for this blocker nothing mechanical can end
  // it: no push changes the fact that the artifact names no commit, and there is
  // no thread to reply on. So the exit is the explicit one this repo already
  // uses for a finding no commit can answer — an allowed author says, on the PR,
  // that they read it. That is a human acknowledgement, not a heuristic: the
  // gate never decides for itself that an artifact it could not classify is
  // clean.
  //
  // …and the answer must NAME the artifact, by its comment URL or its
  // `#issuecomment-<id>` anchor. A marker alone is not specific enough to be an
  // answer: lanes routinely post top-level round comments like "head moved to
  // <sha>, findings RESOLVED in-thread — @codex review", written about that
  // round's INLINE findings, and one of those would silently clear an unbound
  // artifact nobody had read — reopening this very hole through its own exit.
  // Requiring the reference makes an acknowledgement per-artifact and impossible
  // to trip in passing: it can only be written by someone who went and looked at
  // the thing the gate could not classify.
  const acknowledgements = [...comments, ...reviews].filter(
    (artifact) =>
      ALLOWED_AUTHORS.has(artifact.user?.login || "") && hasResolutionMarker(artifact.body || ""),
  );
  //
  // "Names a commit" means STATES THE REVISION IT REVIEWED, which a link alone
  // does not (Codex P1 on #3676). A finding whose prose happens to cite some
  // other commit — an earlier fix, another repository — would otherwise count as
  // placed, bind to no head, and fall out of both sets: dropped, exactly the way
  // #3656's did. Two spellings state it outright (`Reviewed commit:`, and a
  // review's commit_id); a permalink states it only when the commit is one of
  // THIS PR's own, because that is a revision this PR actually had and so a
  // revision that could have been reviewed. A link to anything else says nothing
  // about which head the finding is about, and unknown is not clean.
  const findingCandidates = codexReviewArtifacts.filter(
    (artifact) =>
      CODEX_BODY_FINDING_RE.test(artifact.body || "") &&
      !isCodexSummaryArtifact(artifact) &&
      !codexArtifactStatesItsCommit(artifact),
  );
  // Read only when it can change an answer. Nearly every evaluation has no
  // candidate at all, and this is the one question that needs the PR's history
  // rather than its head. A failed read throws out of retryRead and blocks, like
  // every other read here — an unreadable commit list is not an empty one.
  //
  // KNOWN and ACCEPTED: a force-push drops the old head from this list, so a
  // finding linking a rebased-away commit stops reading as stale and starts
  // blocking, clearable only by an acknowledgement naming it (Codex P2 on
  // #3676). That is the fail-closed side of an unavoidable trade, and it is the
  // side this file takes everywhere else: after a rebase nobody can say whether
  // the finding survived, and "unknown" is what the whole rule treats as
  // not-clean. The alternative asks GitHub whether the linked commit is
  // associated with THIS PR (repos.listPullRequestsAssociatedWithCommit, already
  // used elsewhere here), which would cover a force-pushed-away commit IF that
  // association outlives the rewrite — unverified, and a guess about someone
  // else's retention policy is not a thing to gate merges on. Measure it first;
  // until then one acknowledgement is the cost, on a PR that has both been
  // rebased and carries a finding no head-bound artifact answers.
  const prCommits =
    findingCandidates.length === 0
      ? []
      : await retryRead(
          `could not read commits for PR #${number}`,
          () =>
            github.paginate(github.rest.pulls.listCommits, {
              owner,
              repo,
              pull_number: number,
              per_page: 100,
            }),
          subject,
        );
  const prCommitShas = new Set(
    prCommits.map((commit) => String(commit.sha || "").toLowerCase()).filter(Boolean),
  );
  // GitHub serves at most 250 commits from this endpoint however you paginate
  // it, so on a longer PR the list is a prefix of the history rather than the
  // history (Codex P2 on #3676). The classification is unchanged — a link it
  // cannot confirm still blocks, which is the fail-closed side — but the reason
  // has to SAY so, or the maintainer reads "names no commit" about an artifact
  // that plainly names one and has no way to tell why. A truncated list needs a
  // PR longer than this repo's conventions allow; if that stops being true, the
  // fix is range traversal from the merge base, not ancestry, which would let a
  // link to a pre-branch master commit place the artifact and reopen the hole
  // closed one commit earlier.
  const prCommitsTruncated = prCommits.length >= PR_COMMIT_LIST_CAP;
  const unboundFindingArtifacts = findingCandidates.filter((artifact) => {
    if ([...parseBodyCommits(artifact.body)].some((commit) => prCommitShas.has(commit))) {
      return false;
    }
    // Fails closed on an unknown order, like every other timestamp comparison in
    // this file: an artifact whose own time will not parse is never acknowledged.
    // An edit that adds the reference moves the acknowledging comment's own time
    // with it, so this stays satisfiable; an edit to the FINDING moves the
    // artifact past its answer, which is right — the answer was to other text.
    const artifactTime = reviewArtifactTime(artifact);
    if (artifactTime === 0) {
      return true;
    }
    const references = artifactReferences(artifact);
    return !acknowledgements.some(
      (ack) =>
        // Equal seconds count (Codex P2 on #3676). GitHub serialises two events
        // into the same whole second often enough that a bot answering
        // immediately was rejected, leaving the finding stuck until someone
        // reposted the answer a second later. Equality cannot smuggle in an
        // ordinary earlier comment here, because an answer has to name the
        // artifact's server-generated id — which does not exist until the
        // artifact does.
        reviewArtifactTime(ack) >= artifactTime &&
        references.some((reference) => bodyNamesReference(ack.body || "", reference)) &&
        acknowledgementIsAnswerable(ack, artifactTime, lastPushTime),
    );
  });
  if (unboundFindingArtifacts.length > 0) {
    // Named, not just counted. The remedy tells the reader to answer the artifact
    // by its link; a blocker that does not say WHICH artifact sends them hunting
    // through a comment list that may be dozens long, on the one PR shape where
    // the whole point is that a human goes and reads the thing.
    const named = unboundFindingArtifacts
      .map((artifact) => artifactReferences(artifact)[0])
      .filter(Boolean);
    const unboundReason =
      `${unboundFindingArtifacts.length} Codex artifact(s) carrying a P0-P3 finding name no ` +
      "commit, so the gate cannot tell which head they are about" +
      (named.length > 0 ? `: ${named.join(", ")}` : "") +
      (prCommitsTruncated
        ? `; this PR has more than ${PR_COMMIT_LIST_CAP} commits, so its commit list is truncated ` +
          "and a permalink to an earlier commit could not be checked"
        : "");
    reasons.push(unboundReason);
    findingBlockers.push({
      reason: unboundReason,
      remedy:
        "read the finding and answer it in a PR comment that LINKS it (its comment URL or " +
        "`#issuecomment-<id>`) and carries RESOLVED, ACCEPTED or [gate-ack] — no push can clear " +
        "an artifact that names no commit, and a marker that names no artifact is not an answer",
    });
  }

  const reviewComments = await retryRead(
    `could not read review comments for PR #${number}`,
    () =>
      github.paginate(github.rest.pulls.listReviewComments, {
        owner,
        repo,
        pull_number: number,
        per_page: 100,
      }),
    subject,
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

  // Collected as their own list, not recognised later by matching this string.
  // The manual-merge path has to block on findings specifically (#3558), and a
  // gate that identifies another gate's blocker by its message drifts the moment
  // someone rewords the message.
  //
  // Each carries the REMEDY that clears it, because the two do not clear the same
  // way and a blanket instruction is wrong for one of them (#3591 review). The
  // remedy travels with the reason for the same purpose as the reason itself: so
  // the summary renders what the gate knows rather than inferring it back out of
  // the message.
  if (unresolvedFindings.length > 0) {
    const unresolvedReason = `${unresolvedFindings.length} unresolved live Codex inline finding(s)`;
    reasons.push(unresolvedReason);
    findingBlockers.push({
      reason: unresolvedReason,
      remedy: "reply RESOLVED, ACCEPTED or [gate-ack] on each thread",
    });
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
    const unpushedReason =
      `${unpushedFixClaims.length} finding(s) marked RESOLVED with no commit pushed after them; ` +
      "the head predates the fix they claim";
    reasons.push(unpushedReason);
    // A finding whose only answer is a claim the head cannot contain is still a
    // live finding, so it belongs in the same list as the unanswered kind.
    //
    // Its remedy is NOT "reply RESOLVED". The predicate above already requires a
    // RESOLVED reply to exist, and then turns on lastPushTime — which another
    // reply does not move. Only a commit newer than the finding clears it, or
    // withdrawing the claim with ACCEPTED / [gate-ack], which short-circuits the
    // filter. Advertising RESOLVED here would send the maintainer round a loop
    // that cannot terminate.
    findingBlockers.push({
      reason: unpushedReason,
      remedy:
        "push the commit that fixes them, or reply ACCEPTED / [gate-ack] to withdraw the fix " +
        "claim — another RESOLVED reply cannot clear this one",
    });
  }

  return {
    ok: reasons.length === 0,
    reasons,
    notes,
    reviewerUnavailable,
    reviewerUnavailableReason,
    findingBlockers,
  };
}

function parseReviewedCommit(body) {
  if (!CODEX_REVIEW_RE.test(body) || CODEX_RATE_LIMIT_RE.test(body)) {
    return null;
  }
  return body.match(REVIEWED_COMMIT_RE)?.[1]?.toLowerCase() || null;
}

// The Code Review rows of a Codex summary table. Split on cells rather than
// matched as one regex so a row is read positionally — status in its own cell,
// commit in its own cell — and the header and separator rows fall out for free
// because neither names a Code Review.
function parseSummaryRows(body) {
  const text = String(body || "");
  // Authenticated first. Without this, a finding-bearing review body that quotes
  // the table format parses as a `summary-row` artifact, and the P0-P3 check —
  // which deliberately reads only prose — skips its finding.
  if (!text.includes(CODEX_SUMMARY_MARKER)) {
    return [];
  }
  const rows = [];
  for (const line of text.split("\n")) {
    if (!line.trim().startsWith("|")) {
      continue;
    }
    const cells = line.split("|").slice(1, -1).map((cell) => cell.trim());
    if (cells.length < 3 || !CODEX_SUMMARY_ROW_LABEL_RE.test(cells[0])) {
      continue;
    }
    rows.push({
      completed: CODEX_SUMMARY_ROW_COMPLETED_RE.test(cells[1]),
      commit: CODEX_SUMMARY_ROW_COMMIT_RE.exec(cells[2])?.[1]?.toLowerCase() || null,
      // The ROW's timestamp, not the comment's. The summary comment is edited on
      // every review activity, so its own updated_at says when Codex last
      // touched anything — not when this review completed, which is what the
      // freshness rule is comparing against the head.
      time: parseTimestamp(CODEX_SUMMARY_ROW_TIME_RE.exec(cells[1])?.[1]),
    });
  }
  return rows;
}

// A Codex artifact's verdict for this head, or null. Prose first: it is the
// explicit form and carries the artifact's own timestamp, exactly as before.
//
// The commit is matched as a PREFIX of the head, which needs no API lookup: the
// head SHA is already known here, so there is nothing to resolve and no
// check-then-use window to be raced. A stale row for an older head has to clear
// two independent bars — its seven-character cell must equal this head's prefix,
// and its own timestamp must post-date the head — and freshness is the one doing
// the work.
function parseVerdictArtifact(artifact, headSha) {
  const body = artifact.body || "";
  const reviewedCommit = parseReviewedCommit(body);
  if (reviewedCommit != null && reviewedCommitMatchesHead(reviewedCommit, headSha)) {
    return { kind: "prose", time: reviewArtifactTime(artifact), body };
  }
  const row = parseSummaryRows(body).find(
    (candidate) =>
      candidate.completed &&
      candidate.commit != null &&
      candidate.time != null &&
      reviewedCommitMatchesHead(candidate.commit, headSha),
  );
  return row ? { kind: "summary-row", time: row.time, body } : null;
}

// Whether any artifact's summary table names this head at all, whatever its
// status. This is what separates "no review has run" from "a review ran and this
// gate could not read its verdict" — the distinction that cost an hour of
// looking for a missing review that had already completed.
function summaryNamesHead(artifacts, headSha) {
  return artifacts.some((artifact) =>
    parseSummaryRows(artifact.body || "").some(
      (row) => row.commit != null && reviewedCommitMatchesHead(row.commit, headSha),
    ),
  );
}

// Every 40-hex commit an artifact's body names through a GitHub URL. A Set
// because the only questions asked of it are "does it name this head" and "does
// it name anything".
//
// matchAll needs the /g flag and does not disturb the shared regex's lastIndex —
// it iterates over its own clone — so the module-level constant is safe to reuse
// here without the reset a manual exec loop would need.
function parseBodyCommits(body) {
  return new Set(
    [...String(body || "").matchAll(CODEX_BODY_COMMIT_RE)].map((match) => match[1].toLowerCase()),
  );
}

// Codex's own persistent summary comment, as opposed to a body that merely
// quotes one. It records that a review COMPLETED, never what the review found,
// so it is neither a finding carrier nor evidence about a head — the invariant
// #3606 established and the reason a summary row must never supersede a finding.
//
// Anchored at the start of the body, where the real comment carries it. `bodies
// that CONTAIN the marker` is the wrong test: this repository's own gate script
// holds that marker as a string literal, so a Codex review OF this file can
// quote it, and an exemption keyed on `includes` would drop that review's
// findings — the exact fail-open this function exists to help prevent. A body
// that quotes the marker mid-text is therefore still inspected in full.
function isCodexSummaryArtifact(artifact) {
  const body = String(artifact?.body || "");
  return body.trimStart().startsWith(CODEX_SUMMARY_MARKER) && parseSummaryRows(body).length > 0;
}

// The three spellings of "this artifact is about THIS head", in the order they
// were added: the `Reviewed commit:` prose line, the review's own commit_id, and
// the 40-hex SHAs the body links (#3670).
function codexArtifactBindsToHead(artifact, headSha) {
  // A summary comment names the head in its table on every review activity,
  // including the activity of posting a finding. Letting it bind would make it
  // the newest finding-capable artifact and clear the finding beside it — so it
  // binds to nothing, which is what its lack of a commit_id and of a `Reviewed
  // commit:` line achieved implicitly before body SHAs became a binding rule.
  if (isCodexSummaryArtifact(artifact)) {
    return false;
  }
  const reviewedCommit = parseReviewedCommit(artifact.body || "");
  if (reviewedCommit != null && reviewedCommitMatchesHead(reviewedCommit, headSha)) {
    return true;
  }
  if (String(artifact.commit_id || "").toLowerCase() === String(headSha || "").toLowerCase()) {
    return true;
  }
  return parseBodyCommits(artifact.body).has(String(headSha || "").toLowerCase());
}

// The strings that count as naming THIS artifact: its own permalink, and the
// anchor that permalink ends in — which is what someone pastes when they link a
// comment, and what GitHub's own "Copy link" produces.
//
// The anchor is read off html_url when the API supplied one rather than being
// assembled from a guessed artifact kind, because the two kinds spell it
// differently (`#issuecomment-` for a comment, `#pullrequestreview-` for a
// review) and getting that wrong would make a real answer unrecognisable. Only
// when html_url is absent does it fall back to both spellings of the id: a
// reference is a substring test, so offering both costs nothing that matters —
// no body carries a bare `#issuecomment-<n>` it does not mean. Every artifact
// the REST API returns carries an id, so the empty case below is not a shape
// this reads back from GitHub — it is what an artifact with no identity would
// be, and it stays blocked because nothing can be written that answers it.
function artifactReferences(artifact) {
  const references = [];
  const htmlUrl = String(artifact?.html_url || "");
  if (htmlUrl !== "") {
    references.push(htmlUrl);
    const hash = htmlUrl.indexOf("#");
    if (hash !== -1 && hash < htmlUrl.length - 1) {
      references.push(htmlUrl.slice(hash));
    }
    return references;
  }
  const id = artifact?.id;
  if (id != null && String(id) !== "") {
    references.push(`#issuecomment-${id}`, `#pullrequestreview-${id}`);
  }
  return references;
}

// Whether an acknowledgement can answer an artifact filed at this time.
//
// ACCEPTED and [gate-ack] assert that no code change is owed, so nothing has to
// have been pushed. A bare RESOLVED claims the opposite — that a change WAS made
// — and a fix for a finding filed at T cannot exist in a commit made before T
// (#2878). Without this, "RESOLVED <link>" posted before the fix merges the
// unchanged head, which is exactly the premature merge unpushedFixClaims already
// prevents for inline findings (Codex P1 on #3676); the two paths now hold the
// same claim to the same evidence.
//
// Strict `>` on the push, unlike the acknowledgement's own comparison above: a
// commit made in the same second as the finding cannot contain its fix, while an
// answer written in the same second can name it.
function acknowledgementIsAnswerable(ack, artifactTime, lastPushTime) {
  const body = ack.body || "";
  if (NO_CHANGE_CLAIM_RE.test(body) || body.includes("[gate-ack]")) {
    return true;
  }
  return lastPushTime != null && lastPushTime > artifactTime;
}

// Whether an acknowledgement's body names this reference. Both reference forms
// END in the artifact's numeric id, so a plain substring test would let an
// answer to `#issuecomment-1234` also clear `#issuecomment-123` — a fail-open in
// the exit of a rule that exists to close one. The digit boundary is the whole
// difference; the escape is because a permalink is full of regex metacharacters.
function bodyNamesReference(body, reference) {
  const escaped = reference.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(`${escaped}(?![0-9])`).test(String(body || ""));
}

// Whether the artifact STATES the revision it reviewed, in one of the two
// spellings that are GitHub's or Codex's own assertion rather than prose: the
// `Reviewed commit:` line, and a review's commit_id.
//
// Body permalinks are deliberately not read here. They are prose, and prose can
// cite a commit for any reason (Codex P1 on #3676) — the caller decides whether
// a linked commit places the artifact, by asking whether it is one this PR
// actually had. Nor are a summary table's cells read, by either caller: a body
// that merely QUOTES this script's table, which reviewing this very file
// produces, would otherwise classify itself. Genuine summary comments never
// reach here — they are excluded first, on the LEADING marker, which is the
// check that can tell a real summary from a quoted one.
function codexArtifactStatesItsCommit(artifact) {
  return (
    parseReviewedCommit(artifact?.body || "") != null || String(artifact?.commit_id || "") !== ""
  );
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
    // Blockers are lifted out of the unmet list and named first. The unmet list
    // is advisory — things this gate would have wanted before merging itself —
    // and burying a hard blocker inside it is how one gets read as another
    // "maintainer's call" line on the way to a hand merge (#3558).
    const blockers = result.manualMergeBlockers || [];
    const blockerReasons = new Set(blockers.map((blocker) => blocker.reason));
    const unmet = result.reasons.filter((reason) => !blockerReasons.has(reason));
    const unmetSuffix =
      unmet.length === 0 ? "" : `\n\nUnmet automatic-merge requirements:\n- ${unmet.join("\n- ")}`;
    // Each blocker is rendered with ITS OWN remedy. One blanket instruction was
    // wrong for the unpushed-fix-claim blocker, which no further reply can clear.
    const blocked = blockers
      .map((blocker) => `${blocker.reason} — ${blocker.remedy}`)
      .join("\n- ");
    summary =
      blockers.length === 0
        ? `PASS: ${manual}${unmetSuffix}`
        : `BLOCKED: ${manual} A manual merge still requires every live Codex finding to be ` +
          `answered:\n- ${blocked}${unmetSuffix}`;
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
  resolveMergeRefusal,
  resolveTargets,
  __test: {
    CONCEDED_MERGE_REFUSALS,
    deleteMergedHeadRef,
    parseSummaryRows,
    parseVerdictArtifact,
    MASTER_PUSH_WORKFLOWS,
    isSelfContradictoryNotFound,
    evaluateCodex,
    evaluateRequiredChecks,
    hasResolutionMarker,
    latestRequiredState,
    parseReviewedCommit,
    parseBodyCommits,
    artifactReferences,
    acknowledgementIsAnswerable,
    bodyNamesReference,
    codexArtifactBindsToHead,
    codexArtifactStatesItsCommit,
    isCodexSummaryArtifact,
    reviewedCommitMatchesHead,
  },
};
