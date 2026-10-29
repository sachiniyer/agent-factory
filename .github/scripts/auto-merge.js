"use strict";

// Throughput-mode auto-merger. See .github/auto-merge.md.
//
// Selection is pure (selectMergeable) and fed by ONE paginated GraphQL query per
// run; REST is spent only on the merges themselves, the master dispatches, and
// the (rare) broken-master issue. The Actions GITHUB_TOKEN quota starved the old
// gate, so do not add per-PR reads here.

const BASE = "master";
const FIX_LABEL = "fix-master";
const HOLD_LABELS = ["hold", "no-auto-merge"];
const REQUIRED_CHECKS = ["Build", "Lint"];
const CODEX_LOGIN = "chatgpt-codex-connector";
// A reply is an acknowledgment only when it STARTS with the marker (optionally
// after whitespace / markdown emphasis): a substring match would let "UNRESOLVED"
// or "NOT ACCEPTED" clear a P1 finding (#5676).
const ACK_RE = /^[\s*_>]*(?:RESOLVED|ACCEPTED|\[gate-ack\])(?![A-Za-z])/;
const BROKEN_TITLE_PREFIX = "master broken by merge batch";
// Same list auto-gate.js re-raised after a merge (MASTER_PUSH_WORKFLOWS).
const MASTER_WORKFLOWS = [
  "build.yml",
  "docs.yml",
  "lint.yml",
  // Since slim PR CI (#5615) the heavy jobs (macOS, systemd, perf) run only on
  // master's pr.yml, and a GITHUB_TOKEN merge fires no push to run them (#5674).
  "pr.yml",
  // Advisory like the heavy suites (not in VERIFY_WORKFLOWS), but master must
  // still exercise the sandbox path after every batch: a GITHUB_TOKEN merge
  // fires no push, so without this entry it never ran again.
  "sandbox-e2e.yml",
  "tui-driver-selftest.yml",
  "web-selftest.yml",
];
const DOCS_WORKFLOW = "docs.yml";
const { latestMacosVerdict } = require("./macos-verdict.js");

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
        headRefName
        isCrossRepository
        isDraft
        mergeable
        labels(first: 20) { pageInfo { hasNextPage } nodes { name } }
        commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) {
          totalCount
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

// BATCH-THEN-VERIFY. Under continuous merging every master Build is cancelled by
// the next push, so "latest completed run" reads a stale success. Instead the
// merger only merges onto a master HEAD whose OWN build.yml and lint.yml runs
// (queried by head_sha) have concluded success, then merges at most
// AUTO_MERGE_BATCH_SIZE PRs before the head moves and must be verified again.
const VERIFY_WORKFLOWS = ["build.yml", "lint.yml"];
const DEFAULT_BATCH_SIZE = 8;
const RED_CONCLUSIONS = new Set(["failure", "timed_out", "startup_failure"]);

// Rows are the runs of ONE workflow for the master head.
//   ok       any run succeeded
//   red      (none succeeded) any run failed
//   wait     any run is queued / in progress
//   dispatch no live run at all (all cancelled, or a push that never ran it)
function workflowState(rows) {
  if (rows.some((r) => r.status === "completed" && r.conclusion === "success")) return "ok";
  if (rows.some((r) => r.status === "completed" && RED_CONCLUSIONS.has(r.conclusion))) return "red";
  if (rows.some((r) => r.status !== "completed")) return "wait";
  return "dispatch";
}

// states: {workflow: state}. A failure halts even if the other workflow is still
// running; then waiting beats dispatching; only all-ok verifies the head.
function combineVerification(states) {
  const vals = Object.values(states);
  if (vals.includes("red")) return "red";
  if (vals.includes("wait") || vals.includes("dispatch")) return "wait";
  return "ok";
}

// Returns {status: ok|red|wait, head, states, runs: {workflow: failing/blocking run}}.
// Dispatches workflows that have no live run. A failed read throws: no merge.
// build.yml / lint.yml end with a `Kick auto-merge` job that dispatches the merger
// from INSIDE the run it announces, so that run cannot be `completed` yet when the
// kicked merger looks. Classify an incomplete run by its other jobs: once every
// non-kick job has concluded, the run's verdict is theirs, however fast the
// merger started. Any real job still running keeps the run incomplete (wait).
const KICK_JOB_NAME = "Kick auto-merge";

async function effectiveRun(github, core, owner, repo, run) {
  if (run.status === "completed") return run;
  try {
    const res = await github.rest.actions.listJobsForWorkflowRun({
      owner, repo, run_id: run.id, filter: "latest", per_page: 100,
    });
    const real = res.data.jobs.filter((j) => j.name !== KICK_JOB_NAME);
    if (!real.length || real.some((j) => j.status !== "completed")) return run;
    const conclusions = real.map((j) => j.conclusion);
    const conclusion = conclusions.some((c) => RED_CONCLUSIONS.has(c))
      ? "failure"
      : conclusions.includes("cancelled") ? "cancelled" : "success";
    return { ...run, status: "completed", conclusion };
  } catch (error) {
    // Unreadable jobs: stay conservative (the run reads as still incomplete).
    core.warning(`Could not read jobs of run ${run.id}: ${error.message}`);
    return run;
  }
}

async function verifyMasterHead(github, core, owner, repo) {
  const branch = await github.rest.repos.getBranch({ owner, repo, branch: BASE });
  const head = branch.data.commit.sha;
  const states = {};
  const runs = {};
  for (const workflow_id of VERIFY_WORKFLOWS) {
    const res = await github.rest.actions.listWorkflowRuns({
      owner, repo, workflow_id, head_sha: head, per_page: 20,
    });
    let rows = res.data.workflow_runs.filter((r) => !r.head_sha || r.head_sha === head);
    // Only worth a jobs read when a success is not already in hand.
    if (workflowState(rows) === "wait") {
      rows = await Promise.all(rows.map((r) => effectiveRun(github, core, owner, repo, r)));
    }
    states[workflow_id] = workflowState(rows);
    runs[workflow_id] = rows.find((r) => r.status !== "completed" || RED_CONCLUSIONS.has(r.conclusion)) || null;
  }
  const status = combineVerification(states);
  if (status !== "red") {
    for (const workflow_id of VERIFY_WORKFLOWS) {
      if (states[workflow_id] !== "dispatch") continue;
      await github.rest.actions.createWorkflowDispatch({ owner, repo, workflow_id, ref: BASE });
      core.notice(`Dispatched ${workflow_id} on master: no live run for ${head}.`);
    }
  }
  return { status, head, states, runs };
}

// A connection read with `first: N` that has more than it returned.
function isTruncated(conn) {
  if (!conn) return false;
  if (conn.pageInfo && conn.pageInfo.hasNextPage) return true;
  return typeof conn.totalCount === "number" && conn.totalCount > (conn.nodes || []).length;
}

// Head-branch cleanup. GitHub applies delete_branch_on_merge only to merges made
// by a user token, not GITHUB_TOKEN, so the merger deletes the branch itself.
// The tip re-read and the open-PR checks are ported from deleteMergedHeadRef in
// auto-gate.js. Returns {delete: true} or {delete: false, reason}.
//   branch          repos.getBranch payload (null when already gone)
//   blockingOpenPrs open PRs based ON the branch (deleting closes them) or whose
//                   HEAD is the branch (deleting leaves them headless)
//   tip             branch tip re-read immediately before the delete; undefined
//                   until read. It must equal the merged head: a push after the
//                   merge is work master does not have, and that ref may be its
//                   only copy.
function branchDeletionDecision({ pr, defaultBranch, branch, blockingOpenPrs, stackedOpenPrs, tip }) {
  if (pr.isCrossRepository) return { delete: false, reason: "fork head — never touched" };
  const name = pr.headRefName;
  if (!name) return { delete: false, reason: "no head ref name" };
  if (name === defaultBranch || name === BASE) return { delete: false, reason: "default branch" };
  if (!branch) return { delete: false, reason: "branch already gone" };
  if (branch.protected) return { delete: false, reason: "protected branch" };
  const blocking = blockingOpenPrs ?? stackedOpenPrs ?? 0;
  if (blocking > 0) {
    return { delete: false, reason: `${blocking} open PR(s) use it as their base or head` };
  }
  if (tip !== undefined && String(tip).toLowerCase() !== String(pr.headRefOid).toLowerCase()) {
    return {
      delete: false,
      reason: `tip moved after merge (${tip || "unreadable"}, merged ${pr.headRefOid}) — carries work this merge did not take`,
    };
  }
  return { delete: true };
}

async function deleteMergedBranch({ github, core, owner, repo, pr, defaultBranch }) {
  try {
    let branch = null;
    if (!pr.isCrossRepository && pr.headRefName && pr.headRefName !== defaultBranch) {
      try {
        branch = (await github.rest.repos.getBranch({ owner, repo, branch: pr.headRefName })).data;
      } catch (error) {
        if (error.status !== 404) throw error;
      }
    }
    let blocking = 0;
    if (branch) {
      const [dependents, siblings] = await Promise.all([
        github.rest.pulls.list({ owner, repo, state: "open", base: pr.headRefName, per_page: 1 }),
        // The merged PR is closed by now, so an open head-PR here is a different one.
        github.rest.pulls.list({ owner, repo, state: "open", head: `${owner}:${pr.headRefName}`, per_page: 1 }),
      ]);
      blocking = dependents.data.length + siblings.data.length;
    }
    const first = branchDeletionDecision({ pr, defaultBranch, branch, blockingOpenPrs: blocking });
    if (!first.delete) {
      core.info(`#${pr.number}: keeping branch ${pr.headRefName} (${first.reason})`);
      return;
    }
    // LAST read before the delete: the REST delete takes no expected OID, so
    // ordering is the only lever against a push landing in between.
    const ref = await github.rest.git.getRef({ owner, repo, ref: `heads/${pr.headRefName}` });
    const tip = String((ref && ref.data && ref.data.object && ref.data.object.sha) || "");
    const decision = branchDeletionDecision({ pr, defaultBranch, branch, blockingOpenPrs: blocking, tip });
    if (!decision.delete) {
      core.notice(`Keeping ${pr.headRefName} (#${pr.number}): ${decision.reason}.`);
      return;
    }
    await github.rest.git.deleteRef({ owner, repo, ref: `heads/${pr.headRefName}` });
    core.notice(`Deleted merged branch ${pr.headRefName} (#${pr.number}).`);
  } catch (error) {
    // Best-effort. 404/422: already gone or the ref moved under us — nothing to prune.
    const status = Number(error && (error.status || (error.response && error.response.status)));
    const msg = status === 404 || status === 422 ? "nothing to prune" : "could not delete";
    core.warning(`#${pr.number}: ${msg} branch ${pr.headRefName}: ${error.message}`);
  }
}

const UNKNOWN_RETRY_ATTEMPTS = 3;
const UNKNOWN_RETRY_DELAY_MS = 12000;

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function mergeableQuery(numbers) {
  const fields = numbers
    .map((n) => `\n    p${n}: pullRequest(number: ${Number(n)}) { mergeable }`)
    .join("");
  return `query($owner: String!, $repo: String!) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $repo) {${fields}
  }
}`;
}

// GitHub computes mergeability lazily: the first read starts the computation and
// returns UNKNOWN. Re-read ONLY the UNKNOWN PRs that would otherwise be eligible,
// after a short delay, a bounded number of times; mutates pr.mergeable in place.
// A PR still UNKNOWN afterwards stays a skip, tagged pr.unknownRetries.
async function resolveUnknownMergeable({ github, core, owner, repo, prs, attempts, delayMs }) {
  const maxAttempts = attempts ?? UNKNOWN_RETRY_ATTEMPTS;
  const wait = delayMs ?? UNKNOWN_RETRY_DELAY_MS;
  let pending = prs.filter(
    (pr) => pr.mergeable === "UNKNOWN" && evaluate({ ...pr, mergeable: "MERGEABLE" }) === null,
  );
  for (let attempt = 1; pending.length && attempt <= maxAttempts; attempt++) {
    core.info(`mergeable=UNKNOWN for ${pending.map((p) => "#" + p.number).join(", ")}; re-reading in ${wait}ms (attempt ${attempt}/${maxAttempts})`);
    if (wait > 0) await sleep(wait);
    const data = await github.graphql(mergeableQuery(pending.map((p) => p.number)), { owner, repo });
    noteRateLimit(core, "mergeable", data.rateLimit);
    for (const pr of pending) {
      const node = data.repository[`p${pr.number}`];
      pr.unknownRetries = attempt;
      if (node && node.mergeable) pr.mergeable = node.mergeable;
    }
    pending = pending.filter((pr) => pr.mergeable === "UNKNOWN");
  }
}

function isCodex(login) {
  return (login || "").replace(/\[bot\]$/, "") === CODEX_LOGIN;
}

// A Codex thread of one of `levels` with no RESOLVED/ACCEPTED/[gate-ack] reply.
// Truncated pagination fails closed (treated as unanswered).
function hasUnansweredFinding(pr, levels) {
  const threads = pr.reviewThreads || { nodes: [] };
  if (threads.pageInfo && threads.pageInfo.hasNextPage) return true;
  for (const thread of threads.nodes || []) {
    const comments = thread.comments || { nodes: [] };
    const [first, ...replies] = comments.nodes || [];
    if (!first || !isCodex(first.author && first.author.login)) continue;
    if (!levels.some((level) => (first.body || "").includes(`${level} Badge`))) continue;
    if (comments.pageInfo && comments.pageInfo.hasNextPage) return true;
    if (!replies.some((c) => ACK_RE.test(c.body || ""))) return true;
  }
  return false;
}

const hasUnansweredP1 = (pr) => hasUnansweredFinding(pr, ["P1"]);

// Detail (the detail-app integration) authors its PRs on `detail/...` branches
// and answers its own Codex findings. Those PRs are cheap to hold, and an
// unanswered P2 accuracy finding had left eight docs on master describing
// behaviour that was never shipped (#6006), so they also block on P2. Every
// other PR keeps the throughput-mode rule: only P1 blocks.
const DETAIL_BRANCH_PREFIX = "detail/";
const isDetailPr = (pr) => typeof pr.headRefName === "string" && pr.headRefName.startsWith(DETAIL_BRANCH_PREFIX);
const UNANSWERED_P1_REASON = "unanswered P1 Codex finding";
const UNANSWERED_DETAIL_P2_REASON = "unanswered Codex P2 on a Detail PR";

// The review-thread skip reason for a PR, or null. P1 is reported first.
function reviewBlockReason(pr) {
  if (hasUnansweredP1(pr)) return UNANSWERED_P1_REASON;
  if (isDetailPr(pr) && hasUnansweredFinding(pr, ["P2"])) return UNANSWERED_DETAIL_P2_REASON;
  return null;
}

// Build and Lint must each have run and every run of that name succeeded.
function checksGreen(pr) {
  const commit = pr.commits && pr.commits.nodes && pr.commits.nodes[0];
  const rollup = commit && commit.commit && commit.commit.statusCheckRollup;
  if (!rollup) return false;
  const nodes = (rollup.contexts && rollup.contexts.nodes) || [];
  // A truncated read could hide a second (failing) Build/Lint run: fail closed (#5678).
  if (isTruncated(rollup.contexts)) return false;
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
  // A hold label could sit past the labels we read: fail closed (#5678).
  if (isTruncated(pr.labels)) return "labels truncated";
  const labels = ((pr.labels && pr.labels.nodes) || []).map((l) => l.name);
  if (pr.isDraft) return "draft";
  const held = HOLD_LABELS.find((l) => labels.includes(l));
  if (held) return `label ${held}`;
  if (pr.mergeable !== "MERGEABLE") {
    const retried = pr.unknownRetries ? ` (still UNKNOWN after ${pr.unknownRetries} re-reads)` : "";
    return `mergeable=${pr.mergeable}${retried}`;
  }
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
  return selectCandidates(prs).map((e) => {
    const reason = e.reason === null ? reviewBlockReason(e.pr) : null;
    return reason ? { pr: e.pr, reason } : e;
  });
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

// workflow_run debounce. The run evaluating this is itself running, and the
// `auto-merge` concurrency group allows one running run, so any other run it finds
// is OLDER: it read state from before the event that triggered this one. Skipping
// on "something ran recently" therefore throws away new state (a master Build that
// completed seconds after a merger run said "not yet verified"). A trigger may be
// skipped only when the run it defers to is guaranteed to read state at least as
// new as the event:
//   - another merger run STARTED AFTER the triggering run completed
//     (`workflow_run.updated_at`), or
//   - a newer merger run is already queued (it has not read anything yet).
// Anything else proceeds. `runs` is the newest-first listWorkflowRuns result.
const PENDING_STATUSES = new Set(["queued", "waiting", "pending", "requested"]);

function shouldDebounce({ eventName, triggerUpdatedAt, runs, runId }) {
  if (eventName !== "workflow_run") return { skip: false, reason: "not a workflow_run trigger" };
  const triggered = Date.parse(triggerUpdatedAt);
  for (const r of runs) {
    if (r.id === runId || r.conclusion === "cancelled") continue;
    if (PENDING_STATUSES.has(r.status)) {
      return { skip: true, reason: `run ${r.id} is already queued and will read newer state` };
    }
    // A run that was itself debounced (or halted) completed "success" having
    // read nothing, so it cannot vouch for this trigger (#5786).
    if (r.readState !== true) continue;
    const started = Date.parse(r.run_started_at || r.created_at);
    if (Number.isFinite(triggered) && Number.isFinite(started) && started > triggered) {
      return { skip: true, reason: `run ${r.id} started ${r.run_started_at || r.created_at}, after the triggering run completed (${triggerUpdatedAt})` };
    }
  }
  return { skip: false, reason: "no merger run started after the triggering run completed" };
}

// Whether a finished merger run reached the "Merge ready PRs" step, i.e. read
// merge state and acted on it. The run's own conclusion cannot say: a debounced
// run ends "success" with every later step skipped. A failed jobs read answers
// false — the caller then proceeds, never skips, so a read failure costs at
// most one redundant run (#5786).
const MERGE_STEP_NAME = "Merge ready PRs";
async function runReadState(github, owner, repo, run) {
  try {
    const jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRun, {
      owner, repo, run_id: run.id, per_page: 100,
    });
    return jobs.some((job) => (job.steps || []).some((step) =>
      step.name === MERGE_STEP_NAME && (step.conclusion === "success" || step.conclusion === "failure")));
  } catch (error) {
    return false;
  }
}

async function checkDebounce({ github, core, context }) {
  if (context.eventName !== "workflow_run") return core.setOutput("skip", "false");
  const { owner, repo } = context.repo;
  const res = await github.rest.actions.listWorkflowRuns({
    owner, repo, workflow_id: "auto-merge.yml", per_page: 10,
  });
  const triggerUpdatedAt = context.payload.workflow_run && context.payload.workflow_run.updated_at;
  const triggered = Date.parse(triggerUpdatedAt);
  // Only runs that could make us defer need their jobs read: completed ones
  // that started after the trigger. Pending ones are not read yet by definition.
  const runs = await Promise.all(res.data.workflow_runs.map(async (r) => {
    const started = Date.parse(r.run_started_at || r.created_at);
    const candidate = r.id !== context.runId && r.status === "completed" && r.conclusion !== "cancelled" &&
      Number.isFinite(triggered) && Number.isFinite(started) && started > triggered;
    return candidate ? { ...r, readState: await runReadState(github, owner, repo, r) } : r;
  }));
  const verdict = shouldDebounce({
    eventName: context.eventName,
    triggerUpdatedAt,
    runs,
    runId: context.runId,
  });
  core.notice(`${verdict.skip ? "Debounced" : "Proceeding"}: ${verdict.reason}.`);
  core.setOutput("skip", verdict.skip ? "true" : "false");
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
  // Only merge onto a verified master head. A failed read throws: no merge.
  const verdict = await verifyMasterHead(github, core, owner, repo);
  let red = verdict.status === "red";
  // Test (macOS) runs only on master's pr.yml and its run (hence Build) is often
  // cancelled by the next merge, so verifyMasterHead cannot see it. Read its
  // latest completed verdict directly; a red one halts like a red Build. An
  // unreadable verdict warns and proceeds — it must not freeze all merging.
  let macos = null;
  if (!red && verdict.status === "ok") {
    try {
      macos = await latestMacosVerdict({ github, owner, repo, branch: BASE });
    } catch (error) {
      core.warning(`Could not read the Test (macOS) verdict: ${error.message}`);
    }
    if (macos && macos.verdict === "failure") {
      red = true;
      verdict.status = "red";
      verdict.states["pr.yml (Test (macOS))"] = "red";
      verdict.runs["pr.yml (Test (macOS))"] = { id: macos.runId, html_url: macos.url };
    }
  }
  const lines = Object.entries(verdict.states).map(([wf, st]) => {
    const run = verdict.runs[wf];
    return `- \`${wf}\`: ${st}${run ? ` ([run ${run.id}](${run.html_url}))` : ""}`;
  });
  if (verdict.status !== "ok") {
    await core.summary
      .addHeading(red ? "Merging halted: master is red" : "Waiting for master verification", 3)
      .addRaw(`Master head ${verdict.head.slice(0, 10)}:\n\n${lines.join("\n")}\n\n`)
      .write();
  }
  if (verdict.status === "wait") {
    core.notice(`Master ${verdict.head} not yet verified; exiting without merging.`);
    core.setOutput("merged", "false");
    return;
  }
  if (red) {
    // Not a deadlock: a fix-forward PR labelled `fix-master` (Build + Lint still
    // required on its head) may land, since nothing else could turn master green.
    core.warning(`Master ${verdict.head} failed verification; only ${FIX_LABEL} PRs may merge.`);
  }
  const prs = await fetchOpenPrs(github, core, owner, repo);
  await resolveUnknownMergeable({
    github, core, owner, repo, prs,
    delayMs: process.env.AUTO_MERGE_UNKNOWN_DELAY_MS !== undefined
      ? Number(process.env.AUTO_MERGE_UNKNOWN_DELAY_MS) : undefined,
  });
  const phase1 = selectCandidates(prs);
  await attachThreads(github, core, owner, repo, phase1.filter((e) => !e.reason).map((e) => e.pr));
  let selected = phase1.map((e) => {
    const reason = e.reason === null ? reviewBlockReason(e.pr) : null;
    return reason ? { pr: e.pr, reason } : e;
  });
  if (red) {
    selected = selected.map((e) =>
      e.reason === null && !e.pr.labels.nodes.some((l) => l.name === FIX_LABEL)
        ? { pr: e.pr, reason: "master is red" }
        : e,
    );
  }
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
  const batchSize = Number(process.env.AUTO_MERGE_BATCH_SIZE) > 0
    ? Math.floor(Number(process.env.AUTO_MERGE_BATCH_SIZE))
    : DEFAULT_BATCH_SIZE;
  const merged = [];
  let lastSha = "";
  for (const { pr, reason } of selected) {
    if (merged.length >= batchSize) {
      core.info(`#${pr.number}: skip (batch of ${batchSize} reached; master head must be verified again)`);
      continue;
    }
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
      await deleteMergedBranch({ github, core, owner, repo, pr, defaultBranch: BASE });
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
  runReadState,
  checkHalt, checkDebounce, shouldDebounce, settleBroken, mergeAll, reportBroken,
  workflowState, combineVerification, touchesWorkflows, filesQuery, WORKFLOW_SKIP_REASON, selectCandidates, selectMergeable, hasUnansweredP1, hasUnansweredFinding, isDetailPr, reviewBlockReason, checksGreen, evaluate,
  MASTER_WORKFLOWS, BROKEN_TITLE_PREFIX,
  resolveUnknownMergeable, isTruncated, ACK_RE, branchDeletionDecision,
};
