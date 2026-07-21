const ALLOWED_AUTHORS = new Set(["sachiniyer", "app-detail-app", "app-detail-app[bot]"]);
const TUI_PATH_PREFIXES = ["app/", "ui/", "session/tmux/"];
const DOCS_DEPLOY_PATHS = ["docs/", "mkdocs.yml"];
const CODEX_REVIEWER = "chatgpt-codex-connector[bot]";
const CODEX_REVIEW_RE = /\bCodex Review\b/i;
const CODEX_RATE_LIMIT_RE = /reached your Codex usage limits for code reviews/i;
const REVIEWED_COMMIT_RE = /\*\*Reviewed commit:\*\*\s*`([0-9a-f]{7,40})`/i;
// Docs/Deploy is deliberately conditional and is skipped on pull_request runs.
const ALLOWED_SKIPPED_CHECKS = new Set(["Deploy"]);
const AUTO_GATE_CHECK_NAME = "Evaluate auto-merge gate";
const RESOLUTION_MARKER_RE = /\b(?:RESOLVED|ACCEPTED)\b/;

async function evaluate({ github, context, core, prNumber, setOutputs = true }) {
  try {
    return await evaluatePullRequest({ github, context, core, prNumber, setOutputs });
  } catch (error) {
    const message = formatError(error);
    core.warning(error && error.stack ? error.stack : message);
    return finish(core, setOutputs, {
      prNumber: prNumber ? String(prNumber) : "",
      shouldMerge: false,
      docsChanged: false,
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
      docsChanged: false,
      reasons: ["No open pull request found for this event."],
      notes: [],
    });
  }

  const pr = await getPullRequest({ github, context, number });
  const reasons = [];
  const notes = [];

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

  if (!ALLOWED_AUTHORS.has(pr.author)) {
    reasons.push(`author ${pr.author || "(unknown)"} is not an allowed maintainer/app`);
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

  return finish(core, setOutputs, {
    prNumber: String(pr.number),
    shouldMerge: reasons.length === 0,
    headSha: pr.headRefOid,
    docsChanged,
    reasons,
    notes,
  });
}

async function merge({ github, context, core, prNumber }) {
  if (!Number.isInteger(prNumber) || prNumber <= 0) {
    throw new Error(`Invalid PR number for merge: ${prNumber}`);
  }

  const gate = await evaluate({ github, context, core, prNumber, setOutputs: false });
  if (!gate.shouldMerge) {
    throw new Error(`Refusing to merge PR #${prNumber}; gate no longer passes: ${gate.summary}`);
  }
  if (!gate.headSha) {
    throw new Error(`Refusing to merge PR #${prNumber}; evaluated head SHA is missing`);
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

  if (gate.docsChanged) {
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
  }
}

async function findPullRequestNumber({ github, context, core }) {
  const payload = context.payload;

  if (payload.pull_request?.number) {
    return payload.pull_request.number;
  }

  if (payload.issue?.pull_request && payload.issue.number) {
    return payload.issue.number;
  }

  const checkSuitePrs = payload.check_suite?.pull_requests || [];
  const checkSuitePr = checkSuitePrs.find((pr) => pr.base?.ref === "master") || checkSuitePrs[0];
  if (checkSuitePr?.number) {
    return checkSuitePr.number;
  }

  const sha = payload.check_suite?.head_sha || payload.sha;
  if (!sha) {
    core.info(`Event ${context.eventName} did not include a PR or SHA to evaluate.`);
    return null;
  }

  const { owner, repo } = context.repo;
  const pulls = await github.paginate(github.rest.repos.listPullRequestsAssociatedWithCommit, {
    owner,
    repo,
    commit_sha: sha,
    per_page: 100,
  });

  const openMasterPulls = pulls.filter((pr) => pr.state === "open" && pr.base?.ref === "master");
  if (openMasterPulls.length > 1) {
    core.warning(`Found multiple open master PRs for ${sha}; evaluating PR #${openMasterPulls[0].number}.`);
  }

  return openMasterPulls[0]?.number || null;
}

async function getPullRequest({ github, context, number }) {
  const { owner, repo } = context.repo;
  const query = `
    query($owner: String!, $repo: String!, $number: Int!) {
      repository(owner: $owner, name: $repo) {
        pullRequest(number: $number) {
          number
          title
          url
          baseRefName
          headRefName
          headRefOid
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

  const response = await github.graphql(query, { owner, repo, number });
  const pr = response.repository.pullRequest;
  if (!pr) {
    throw new Error(`PR #${number} was not found`);
  }

  return {
    number: pr.number,
    title: pr.title,
    url: pr.url,
    baseRefName: pr.baseRefName,
    headRefOid: pr.headRefOid,
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

async function listPullRequestFiles({ github, context, number }) {
  const { owner, repo } = context.repo;
  const files = await github.paginate(github.rest.pulls.listFiles, {
    owner,
    repo,
    pull_number: number,
    per_page: 100,
  });
  return files.map((file) => file.filename);
}

async function evaluateRequiredChecks({ github, context, branch, sha, core }) {
  const required = await getRequiredCheckSpecs({ github, context, branch, core });
  const specs = required.specs;
  const notes = [];
  const reasons = [...required.errors];

  if (specs.length === 0) {
    notes.push("No required status checks configured for branch");
  } else {
    notes.push(`Required status checks: ${specs.map(formatCheckSpec).join(", ")}`);
  }

  const { owner, repo } = context.repo;
  const checkRuns = await github.paginate(github.rest.checks.listForRef, {
    owner,
    repo,
    ref: sha,
    per_page: 100,
  });
  const statuses = await github.paginate(github.rest.repos.listCommitStatusesForRef, {
    owner,
    repo,
    ref: sha,
    per_page: 100,
  });

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

  for (const run of latestCheckRuns(checkRuns)) {
    if (run.name === AUTO_GATE_CHECK_NAME || specs.some((spec) => checkRunMatchesSpec(run, spec))) {
      continue;
    }

    const state = checkRunState(run);
    notes.push(`Head check ${run.name}: ${state.description}`);
    if (!state.ok) {
      const stateDescription = state.waiting ? "is still settling" : "did not succeed";
      reasons.push(`head check ${run.name} ${stateDescription} (${state.description})`);
    }
  }

  return { ok: reasons.length === 0, reasons, notes };
}

async function getRequiredCheckSpecs({ github, context, branch, core }) {
  const { owner, repo } = context.repo;
  const specs = new Map();
  const errors = [];

  try {
    const response = await github.request("GET /repos/{owner}/{repo}/rules/branches/{branch}", {
      owner,
      repo,
      branch,
    });

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
    const response = await github.request(
      "GET /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks",
      { owner, repo, branch },
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

function latestCheckRuns(checkRuns) {
  const latest = new Map();
  for (const run of checkRuns) {
    const key = `${run.name}\0${run.app?.id || ""}`;
    const current = latest.get(key);
    if (!current || latestRunTime(run) > latestRunTime(current)) {
      latest.set(key, run);
    }
  }
  return [...latest.values()].sort((a, b) => a.name.localeCompare(b.name));
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
  const { owner, repo } = context.repo;
  const lastPushTime = parseTimestamp(lastCommitDate);

  if (lastPushTime == null) {
    reasons.push("last commit timestamp was unavailable, so Codex freshness cannot be verified");
  }

  const comments = await github.paginate(github.rest.issues.listComments, {
    owner,
    repo,
    issue_number: number,
    per_page: 100,
  });
  const codexComments = comments
    .filter((comment) => comment.user?.login === CODEX_REVIEWER)
    .sort((a, b) => commentTime(b) - commentTime(a));
  const verdict = codexComments.find((comment) => {
    const reviewedCommit = parseReviewedCommit(comment.body || "");
    return reviewedCommit != null && reviewedCommitMatchesHead(reviewedCommit, sha);
  });

  if (!verdict) {
    const rateLimited = CODEX_RATE_LIMIT_RE.test(codexComments[0]?.body || "");
    const suffix = rateLimited ? "; the latest Codex response was usage-limited" : "";
    reasons.push(`Codex has not reviewed head ${sha} yet${suffix}`);
  } else {
    const verdictTime = commentTime(verdict);
    if (lastPushTime == null || verdictTime === 0 || verdictTime <= lastPushTime) {
      reasons.push("Codex verdict for the head commit is older than the head commit timestamp");
    } else {
      notes.push(`Codex verdict matches head ${sha}`);
    }
  }

  const reviewComments = await github.paginate(github.rest.pulls.listReviewComments, {
    owner,
    repo,
    pull_number: number,
    per_page: 100,
  });
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
  const unresolvedFindings = reviewComments.filter((comment) => {
    if (comment.user?.login !== CODEX_REVIEWER) {
      return false;
    }
    if (comment.in_reply_to_id) {
      return false;
    }
    if (comment.line == null) {
      return false;
    }
    return !resolvedByAllowedReply.has(comment.id);
  });

  if (unresolvedFindings.length > 0) {
    reasons.push(`${unresolvedFindings.length} unresolved live Codex inline finding(s)`);
  } else {
    notes.push("No unresolved live Codex inline findings");
  }

  return { ok: reasons.length === 0, reasons, notes };
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

function commentTime(comment) {
  return parseTimestamp(comment.updated_at || comment.created_at) || 0;
}

function hasResolutionMarker(body) {
  return RESOLUTION_MARKER_RE.test(body) || body.includes("[gate-ack]");
}

function latestRunTime(run) {
  return parseTimestamp(run.completed_at || run.started_at || run.created_at) || 0;
}

function parseTimestamp(value) {
  const parsed = Date.parse(value || "");
  return Number.isFinite(parsed) ? parsed : null;
}

function finish(core, setOutputs, result) {
  const summary =
    result.reasons.length === 0
      ? `PASS: ${result.notes.join("; ")}`
      : `BLOCKED: ${result.reasons.join("; ")}`;

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
  evaluate,
  merge,
  __test: {
    evaluateCodex,
    evaluateRequiredChecks,
    hasResolutionMarker,
    latestRequiredState,
    parseReviewedCommit,
    reviewedCommitMatchesHead,
  },
};
