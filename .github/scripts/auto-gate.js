const ALLOWED_AUTHORS = new Set(["sachiniyer", "app-detail-app", "app-detail-app[bot]"]);
const TUI_PATH_PREFIXES = ["app/", "ui/", "session/tmux/"];
const DOCS_DEPLOY_PATHS = ["docs/", "mkdocs.yml"];
const GREPTILE_RE = /greptile/i;

async function evaluate({ github, context, core, prNumber, setOutputs = true }) {
  const number = prNumber || (await findPullRequestNumber({ github, context, core }));

  if (!number) {
    return finish(core, setOutputs, {
      prNumber: "",
      shouldMerge: false,
      reasons: ["No open pull request found for this event."],
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
  } else if (pr.mergeable === "UNKNOWN") {
    reasons.push("mergeability is still UNKNOWN");
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

  const greptile = await evaluateGreptile({
    github,
    context,
    number: pr.number,
    sha: pr.headRefOid,
    lastCommitDate: pr.lastCommitDate,
    core,
  });
  if (!greptile.ok) {
    reasons.push(...greptile.reasons);
  }
  notes.push(...greptile.notes);

  return finish(core, setOutputs, {
    prNumber: String(pr.number),
    shouldMerge: reasons.length === 0,
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

  const { owner, repo } = context.repo;
  const response = await github.rest.pulls.merge({
    owner,
    repo,
    pull_number: prNumber,
    merge_method: "squash",
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
  const required = await getRequiredCheckContexts({ github, context, branch, core });
  const contexts = required.contexts;
  const notes = [];
  const reasons = [...required.errors];

  if (contexts.length === 0) {
    if (reasons.length > 0) {
      return { ok: false, reasons, notes };
    }
    notes.push("No required status checks configured for branch");
    return { ok: true, reasons, notes };
  }

  notes.push(`Required status checks: ${contexts.join(", ")}`);

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

  for (const contextName of contexts) {
    const state = latestRequiredState(contextName, checkRuns, statuses);
    if (!state) {
      reasons.push(`required check ${contextName} is missing on ${sha}`);
      continue;
    }

    notes.push(`${contextName}: ${state.description}`);
    if (!state.ok) {
      reasons.push(`required check ${contextName} is not completed successfully (${state.description})`);
    }
  }

  return { ok: reasons.length === 0, reasons, notes };
}

async function getRequiredCheckContexts({ github, context, branch, core }) {
  const { owner, repo } = context.repo;
  const contexts = new Set();
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
          contexts.add(check.context);
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

  if (contexts.size > 0) {
    return { contexts: [...contexts].sort(), errors: [] };
  }

  try {
    const response = await github.request(
      "GET /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks",
      { owner, repo, branch },
    );

    for (const contextName of response.data.contexts || []) {
      contexts.add(contextName);
    }
    for (const check of response.data.checks || []) {
      if (check.context) {
        contexts.add(check.context);
      }
    }
  } catch (error) {
    if (error.status !== 404) {
      const message = `could not read branch protection checks for ${branch}: ${formatError(error)}`;
      core.warning(message);
      errors.push(message);
    }
  }

  return { contexts: [...contexts].sort(), errors };
}

function latestRequiredState(contextName, checkRuns, statuses) {
  const candidates = [];

  for (const run of checkRuns) {
    if (run.name !== contextName) {
      continue;
    }
    candidates.push({
      date: Date.parse(run.completed_at || run.started_at || run.created_at || 0),
      ok: run.status === "completed" && run.conclusion === "success",
      description: `check run ${run.status}/${run.conclusion || "no conclusion"}`,
    });
  }

  for (const status of statuses) {
    if (status.context !== contextName) {
      continue;
    }
    candidates.push({
      date: Date.parse(status.created_at || 0),
      ok: status.state === "success",
      description: `commit status ${status.state}`,
    });
  }

  candidates.sort((a, b) => b.date - a.date);
  return candidates[0] || null;
}

async function evaluateGreptile({ github, context, number, sha, lastCommitDate, core }) {
  const notes = [];
  const reasons = [];
  const { owner, repo } = context.repo;

  const checkRuns = await github.paginate(github.rest.checks.listForRef, {
    owner,
    repo,
    ref: sha,
    per_page: 100,
  });
  const greptileRuns = checkRuns
    .filter((run) => GREPTILE_RE.test(run.name) || GREPTILE_RE.test(run.app?.slug || run.app?.name || ""))
    .sort((a, b) => latestRunTime(b) - latestRunTime(a));
  const latestRun = greptileRuns[0];

  if (!latestRun) {
    reasons.push("Greptile Review check run was not found");
  } else {
    notes.push(`Greptile check: ${latestRun.name} ${latestRun.status}/${latestRun.conclusion || "no conclusion"}`);
    if (latestRun.status !== "completed" || latestRun.conclusion !== "success") {
      reasons.push(`Greptile Review is not completed successfully (${latestRun.status}/${latestRun.conclusion || "no conclusion"})`);
    }
  }

  const comments = await github.paginate(github.rest.issues.listComments, {
    owner,
    repo,
    issue_number: number,
    per_page: 100,
  });
  const latestGreptileComment = comments
    .filter((comment) => GREPTILE_RE.test(comment.user?.login || ""))
    .sort((a, b) => Date.parse(b.updated_at || b.created_at || 0) - Date.parse(a.updated_at || a.created_at || 0))[0];

  if (!latestGreptileComment) {
    reasons.push("latest greptile-apps summary comment was not found");
  } else {
    const score = parseConfidenceScore(latestGreptileComment.body || "");
    if (score == null) {
      reasons.push("latest greptile-apps summary comment did not include a Confidence Score");
    } else if (score < 4) {
      reasons.push(`Greptile Confidence Score is ${score}/5, below 4/5`);
    } else {
      notes.push(`Greptile Confidence Score: ${score}/5`);
    }
  }

  const reviewComments = await github.paginate(github.rest.pulls.listReviewComments, {
    owner,
    repo,
    pull_number: number,
    per_page: 100,
  });
  const lastPushTime = Date.parse(lastCommitDate || 0);
  const repliedTo = new Set(
    reviewComments.filter((comment) => comment.in_reply_to_id).map((comment) => comment.in_reply_to_id),
  );
  const unresolvedFindings = reviewComments.filter((comment) => {
    if (!GREPTILE_RE.test(comment.user?.login || "")) {
      return false;
    }
    if (comment.in_reply_to_id) {
      return false;
    }
    if (Date.parse(comment.created_at || 0) <= lastPushTime) {
      return false;
    }
    return /\bP[12]\b/i.test(comment.body || "") && !repliedTo.has(comment.id);
  });

  if (unresolvedFindings.length > 0) {
    reasons.push(`${unresolvedFindings.length} unresolved Greptile P1/P2 inline finding(s) after the last push`);
  } else {
    notes.push("No unresolved Greptile P1/P2 inline findings after the last push");
  }

  return { ok: reasons.length === 0, reasons, notes };
}

function parseConfidenceScore(body) {
  const normalized = body.replace(/\s+/g, " ");
  const patterns = [
    /confidence\s*score\D{0,40}([0-5](?:\.\d+)?)\s*\/\s*5/i,
    /confidence\D{0,40}([0-5](?:\.\d+)?)\s*\/\s*5/i,
    /([0-5](?:\.\d+)?)\s*\/\s*5\D{0,40}confidence/i,
  ];

  for (const pattern of patterns) {
    const match = normalized.match(pattern);
    if (match) {
      return Number.parseFloat(match[1]);
    }
  }

  return null;
}

function latestRunTime(run) {
  return Date.parse(run.completed_at || run.started_at || run.created_at || 0);
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
    core.setOutput("docs_changed", result.docsChanged ? "true" : "false");
    core.setOutput("summary", summary);
  }

  return { ...result, summary };
}

function formatError(error) {
  return `${error.status || "error"} ${error.message || error}`;
}

module.exports = { evaluate, merge };
