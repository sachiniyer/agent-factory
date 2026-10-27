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

const QUERY = `
query($owner: String!, $repo: String!, $base: String!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequests(states: OPEN, baseRefName: $base, first: 25, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        headRefOid
        isDraft
        mergeable
        labels(first: 50) { nodes { name } }
        commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) {
          pageInfo { hasNextPage }
          nodes {
            __typename
            ... on CheckRun { name status conclusion }
          }
        } } } } }
        reviewThreads(first: 50) {
          pageInfo { hasNextPage }
          nodes { comments(first: 20) {
            pageInfo { hasNextPage }
            nodes { author { login } body }
          } }
        }
      }
    }
  }
}`;

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
  if (hasUnansweredP1(pr)) return "unanswered P1 Codex finding";
  return null;
}

function selectMergeable(prs) {
  return prs
    .map((pr) => ({ pr, reason: evaluate(pr) }))
    .sort((a, b) => a.pr.number - b.pr.number);
}

async function fetchOpenPrs(github, owner, repo) {
  const prs = [];
  let after = null;
  for (;;) {
    const data = await github.graphql(QUERY, { owner, repo, base: BASE, after });
    const page = data.repository.pullRequests;
    prs.push(...page.nodes);
    if (!page.pageInfo.hasNextPage) return prs;
    after = page.pageInfo.endCursor;
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
  const prs = await fetchOpenPrs(github, owner, repo);
  const merged = [];
  let lastSha = "";
  for (const { pr, reason } of selectMergeable(prs)) {
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
  selectMergeable, hasUnansweredP1, checksGreen, evaluate,
  MASTER_WORKFLOWS, BROKEN_TITLE_PREFIX,
};
