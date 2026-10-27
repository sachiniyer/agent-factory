"use strict";

// Throughput-mode auto-merger. See .github/auto-merge.md.
//
// Selection is pure (selectMergeable) and fed by ONE paginated GraphQL query per
// run; REST is spent only on the merges themselves, the master dispatches, and
// the (rare) broken-master issue. The Actions GITHUB_TOKEN quota starved the old
// gate, so do not add per-PR reads here.

const BASE = "master";
const HOLD_LABELS = ["hold", "no-auto-merge"];
const REQUIRED_CHECKS = ["Build", "Lint"];
const CODEX_LOGIN = "chatgpt-codex-connector";
const ACK_RE = /RESOLVED|ACCEPTED|\[gate-ack\]/;
const BROKEN_TITLE_PREFIX = "master broken by merge batch";
// Same list auto-gate.js re-raised after a merge (MASTER_PUSH_WORKFLOWS).
const MASTER_WORKFLOWS = [
  "build.yml",
  "docs.yml",
  "lint.yml",
  "tui-driver-selftest.yml",
  "web-selftest.yml",
];
const DOCS_WORKFLOW = "docs.yml";

// Phase 1: cheap, for every open master PR. No review threads here.
const CANDIDATE_QUERY = `
query($owner: String!, $repo: String!, $base: String!, $after: String) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $repo) {
    pullRequests(states: OPEN, baseRefName: $base, first: 50, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        headRefOid
        isDraft
        mergeable
        labels(first: 20) { nodes { name } }
        commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) {
          pageInfo { hasNextPage }
          nodes {
            __typename
            ... on CheckRun { name status conclusion }
          }
        } } } } }
      }
    }
  }
}`;

// Phase 2: review threads, only for PRs that survived phase 1 (one aliased query).
function threadsQuery(numbers) {
  const fields = numbers
    .map(
      (n) => `
    p${n}: pullRequest(number: ${Number(n)}) {
      reviewThreads(first: 50) {
        pageInfo { hasNextPage }
        nodes { comments(first: 20) {
          pageInfo { hasNextPage }
          nodes { author { login } body }
        } }
      }
    }`,
    )
    .join("");
  return `query($owner: String!, $repo: String!) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $repo) {${fields}
  }
}`;
}

const WORKFLOW_SKIP_REASON =
  "workflow files — needs AUTO_MERGE_TOKEN or a maintainer merge";

// One aliased query, only for the PRs that survived phase 1 (and only when
// merging with GITHUB_TOKEN, which cannot touch .github/workflows/).
function filesQuery(numbers) {
  const fields = numbers
    .map(
      (n) => `
    p${n}: pullRequest(number: ${Number(n)}) {
      files(first: 100) { pageInfo { hasNextPage } nodes { path } }
    }`,
    )
    .join("");
  return `query($owner: String!, $repo: String!) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $repo) {${fields}
  }
}`;
}

// A truncated or missing file list fails closed (treated as touching workflows).
function touchesWorkflows(files) {
  if (!files || (files.pageInfo && files.pageInfo.hasNextPage)) return true;
  return (files.nodes || []).some((f) => f.path.startsWith(".github/workflows/"));
}

function isCodex(login) {
  return (login || "").replace(/\[bot\]$/, "") === CODEX_LOGIN;
}

// A P1 Codex thread with no RESOLVED/ACCEPTED/[gate-ack] reply. Truncated
// pagination fails closed (treated as unanswered).
function hasUnansweredP1(pr) {
  const threads = pr.reviewThreads || { nodes: [] };
  if (threads.pageInfo && threads.pageInfo.hasNextPage) return true;
  for (const thread of threads.nodes || []) {
    const comments = thread.comments || { nodes: [] };
    const [first, ...replies] = comments.nodes || [];
    if (!first || !isCodex(first.author && first.author.login)) continue;
    if (!(first.body || "").includes("P1 Badge")) continue;
    if (comments.pageInfo && comments.pageInfo.hasNextPage) return true;
    if (!replies.some((c) => ACK_RE.test(c.body || ""))) return true;
  }
  return false;
}

// Build and Lint must each have run and every run of that name succeeded.
function checksGreen(pr) {
  const commit = pr.commits && pr.commits.nodes && pr.commits.nodes[0];
  const rollup = commit && commit.commit && commit.commit.statusCheckRollup;
  if (!rollup) return false;
  const nodes = (rollup.contexts && rollup.contexts.nodes) || [];
  return REQUIRED_CHECKS.every((name) => {
    const runs = nodes.filter((n) => n.__typename === "CheckRun" && n.name === name);
    return (
      runs.length > 0 &&
      runs.every((r) => r.status === "COMPLETED" && r.conclusion === "SUCCESS")
    );
  });
}

// Returns {pr, reason} entries: reason is null for a PR to merge.
function evaluate(pr) {
  const labels = ((pr.labels && pr.labels.nodes) || []).map((l) => l.name);
  if (pr.isDraft) return "draft";
  const held = HOLD_LABELS.find((l) => labels.includes(l));
  if (held) return `label ${held}`;
  if (pr.mergeable !== "MERGEABLE") return `mergeable=${pr.mergeable}`;
  if (!checksGreen(pr)) return "Build/Lint not green on current head";
  return null;
}

// Phase-1 selection (no review data needed). Reasons are null for candidates.
function selectCandidates(prs) {
  return prs
    .map((pr) => ({ pr, reason: evaluate(pr) }))
    .sort((a, b) => a.pr.number - b.pr.number);
}

// Full selection once reviewThreads are attached (tests use this directly).
function selectMergeable(prs) {
  return selectCandidates(prs).map((e) =>
    e.reason === null && hasUnansweredP1(e.pr)
      ? { pr: e.pr, reason: "unanswered P1 Codex finding" }
      : e,
  );
}

function noteRateLimit(core, label, rl) {
  core.notice(`GraphQL ${label}: cost ${rl.cost}, remaining ${rl.remaining}, resets ${rl.resetAt}`);
}

async function fetchOpenPrs(github, core, owner, repo) {
  const prs = [];
  let after = null;
  for (;;) {
    const data = await github.graphql(CANDIDATE_QUERY, { owner, repo, base: BASE, after });
    noteRateLimit(core, "candidates", data.rateLimit);
    const page = data.repository.pullRequests;
    prs.push(...page.nodes);
    if (!page.pageInfo.hasNextPage) return prs;
    after = page.pageInfo.endCursor;
  }
}

async function attachThreads(github, core, owner, repo, prs) {
  if (!prs.length) return;
  const data = await github.graphql(threadsQuery(prs.map((p) => p.number)), { owner, repo });
  noteRateLimit(core, "threads", data.rateLimit);
  for (const pr of prs) {
    const node = data.repository[`p${pr.number}`];
    // Missing data fails closed: hasUnansweredP1 treats hasNextPage as unanswered.
    pr.reviewThreads = node ? node.reviewThreads : { pageInfo: { hasNextPage: true }, nodes: [] };
  }
}

async function findBrokenIssue(github, owner, repo) {
  const res = await github.rest.search.issuesAndPullRequests({
    q: `repo:${owner}/${repo} is:issue is:open in:title "${BROKEN_TITLE_PREFIX}"`,
    per_page: 5,
  });
  return res.data.items.find((i) => i.title.startsWith(BROKEN_TITLE_PREFIX)) || null;
}

// Step 1: is merging halted by an open broken-master issue?
async function checkHalt({ github, core, context }) {
  const { owner, repo } = context.repo;
  const issue = await findBrokenIssue(github, owner, repo);
  core.setOutput("broken", issue ? "true" : "false");
  core.setOutput("broken_number", issue ? String(issue.number) : "");
}

// Step 2 (only when broken): close the issue if master now passes.
async function settleBroken({ github, core, context, passed, number }) {
  const { owner, repo } = context.repo;
  if (!passed) {
    core.warning(`master still fails the composition check (#${number}); merging stays halted.`);
    core.setOutput("halted", "true");
    return;
  }
  await github.rest.issues.createComment({
    owner, repo, issue_number: Number(number),
    body: "Master passes the composition check again; auto-merge resumed.\n\n— Captain Claude (throughput-infra)",
  });
  await github.rest.issues.update({ owner, repo, issue_number: Number(number), state: "closed" });
  core.setOutput("halted", "false");
}

// Step 3: merge everything selectable, then re-raise master verification once.
async function mergeAll({ github, core, context }) {
  const { owner, repo } = context.repo;
  const prs = await fetchOpenPrs(github, core, owner, repo);
  const phase1 = selectCandidates(prs);
  await attachThreads(github, core, owner, repo, phase1.filter((e) => !e.reason).map((e) => e.pr));
  let selected = phase1.map((e) =>
    e.reason === null && hasUnansweredP1(e.pr) ? { pr: e.pr, reason: "unanswered P1 Codex finding" } : e,
  );
  // AUTO_MERGE_TOKEN (a PAT with the workflows permission) can merge workflow
  // PRs; GITHUB_TOKEN cannot, and GitHub refuses the merge. Detect them up front.
  const usingPat = process.env.USING_PAT === "true";
  const workflowSkipped = [];
  if (!usingPat) {
    const cands = selected.filter((e) => !e.reason).map((e) => e.pr);
    if (cands.length) {
      const data = await github.graphql(filesQuery(cands.map((c) => c.number)), { owner, repo });
      noteRateLimit(core, "files", data.rateLimit);
      selected = selected.map((e) => {
        if (e.reason) return e;
        const node = data.repository[`p${e.pr.number}`];
        if (!touchesWorkflows(node && node.files)) return e;
        workflowSkipped.push(e.pr.number);
        return { pr: e.pr, reason: WORKFLOW_SKIP_REASON };
      });
    }
  }
  if (workflowSkipped.length) {
    await core.summary
      .addHeading("Skipped: workflow-touching PRs", 3)
      .addRaw(`${WORKFLOW_SKIP_REASON}\n\n`)
      .addList(workflowSkipped.map((n) => `#${n}`))
      .write();
  }
  const merged = [];
  let lastSha = "";
  for (const { pr, reason } of selected) {
    if (reason) {
      core.info(`#${pr.number}: skip (${reason})`);
      continue;
    }
    try {
      const res = await github.rest.pulls.merge({
        owner, repo, pull_number: pr.number,
        merge_method: "squash",
        sha: pr.headRefOid,
      });
      merged.push(pr.number);
      lastSha = res.data.sha;
      core.notice(`Merged #${pr.number} at ${pr.headRefOid} -> ${lastSha}`);
    } catch (error) {
      // 405/409: head moved, conflict from an earlier merge in this batch, or a
      // protection rule. The next run re-evaluates from fresh state.
      core.warning(`#${pr.number}: merge refused: ${error.message}`);
    }
  }
  core.setOutput("merged", merged.length ? "true" : "false");
  core.setOutput("merged_prs", merged.join(","));
  if (!merged.length) return;

  // GITHUB_TOKEN merges fire no push workflows: dispatch once for the new head.
  // A PAT merge fires them itself, so dispatching would duplicate every run.
  if (usingPat) return;
  for (const workflow_id of MASTER_WORKFLOWS) {
    try {
      await github.rest.actions.createWorkflowDispatch({
        owner, repo, workflow_id, ref: BASE,
        ...(workflow_id === DOCS_WORKFLOW ? { inputs: { verify_sha: lastSha } } : {}),
      });
      core.notice(`Dispatched ${workflow_id} on master for ${lastSha}.`);
    } catch (error) {
      core.warning(`Could not dispatch ${workflow_id}: ${error.message}`);
    }
  }
}

// Step 4: composition check failed after a batch — open/update ONE issue.
async function reportBroken({ github, core, context, merged }) {
  const { owner, repo } = context.repo;
  const sha = (process.env.MASTER_SHA || "").slice(0, 10);
  const title = `${BROKEN_TITLE_PREFIX} ${sha}`;
  const body =
    `The post-merge composition check (\`go build ./...\`, \`go vet ./...\`, ` +
    `\`scripts/lint-file-length.sh\`) failed on master ${sha} after auto-merging ` +
    `${merged ? "#" + merged.split(",").join(", #") : "a batch"}.\n\n` +
    `Auto-merge is halted until master passes again; fix forward on master. The next ` +
    `scheduled run re-checks and closes this issue automatically.\n\n` +
    `Run: ${context.serverUrl}/${owner}/${repo}/actions/runs/${context.runId}\n\n— Captain Claude (throughput-infra)`;
  const existing = await findBrokenIssue(github, owner, repo);
  if (existing) {
    await github.rest.issues.createComment({ owner, repo, issue_number: existing.number, body });
    return;
  }
  await github.rest.issues.create({ owner, repo, title, body });
}

module.exports = {
  checkHalt, settleBroken, mergeAll, reportBroken,
  touchesWorkflows, filesQuery, WORKFLOW_SKIP_REASON, selectCandidates, selectMergeable, hasUnansweredP1, checksGreen, evaluate,
  MASTER_WORKFLOWS, BROKEN_TITLE_PREFIX,
};
