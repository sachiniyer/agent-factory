// The latest COMPLETED verdict of master's `Test (macOS)` job — the only place
// the darwin test suite runs. Neither Build nor Lint sees it on master: a master
// pr.yml run's Build is cancelled with the run, so a red macOS job merged
// unnoticed (#5818, #5947). Used by the auto-merger (halt non-fix-master merges
// while it is red) and by the release gates (do not cut a release on a known
// red verdict).
//
// Walks master's recent pr.yml runs newest-first and returns the first one whose
// macOS job reached success/failure. A run that is still in progress counts when
// its macOS job is already done; a cancelled job says nothing and is skipped.
const WORKFLOW = "pr.yml";
const JOB_NAME = "Test (macOS)";
const MAX_RUNS = 8;

async function latestMacosVerdict({ github, owner, repo, branch = "master", maxRuns = MAX_RUNS }) {
  const res = await github.rest.actions.listWorkflowRuns({
    owner, repo, workflow_id: WORKFLOW, branch, per_page: maxRuns,
  });
  for (const run of res.data.workflow_runs.slice(0, maxRuns)) {
    const jobs = await github.rest.actions.listJobsForWorkflowRun({
      owner, repo, run_id: run.id, filter: "latest", per_page: 100,
    });
    const job = jobs.data.jobs.find((j) => j.name === JOB_NAME);
    if (job && job.status === "completed" && (job.conclusion === "success" || job.conclusion === "failure" || job.conclusion === "timed_out")) {
      return {
        verdict: job.conclusion === "success" ? "success" : "failure",
        sha: run.head_sha,
        runId: run.id,
        url: job.html_url || run.html_url,
        completedAt: job.completed_at,
      };
    }
  }
  return null;
}

// Release gate. A red latest verdict blocks the release; no verdict (the job was
// cancelled or never ran in the recent window) warns and proceeds, because the
// gate must not wait 15 minutes on a 10x-billed runner and the Linux preflight
// plus the darwin cross-build still run. Test (macOS) is NOT a job of the release
// workflows: it already ran on master's own commits, and re-running it here would
// triple the macOS bill for the same commits.
async function gateRelease({ github, core, context }) {
  const { owner, repo } = context.repo;
  const v = await latestMacosVerdict({ github, owner, repo });
  if (!v) {
    core.warning(`No completed Test (macOS) verdict in master's last ${MAX_RUNS} pr.yml runs; releasing on the Linux preflight and the darwin build.`);
    return;
  }
  const age = `${v.sha.slice(0, 10)} (${v.completedAt})`;
  if (v.verdict === "failure") {
    core.setFailed(`Latest Test (macOS) verdict on master is RED at ${age}: ${v.url}. Fix forward on master (fix-master) before releasing.`);
    return;
  }
  core.notice(`Latest Test (macOS) verdict on master is green at ${age}.`);
}

module.exports = { gateRelease, latestMacosVerdict, WORKFLOW, JOB_NAME };
