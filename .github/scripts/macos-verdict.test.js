const assert = require('node:assert/strict');
const test = require('node:test');
const { latestMacosVerdict, gateRelease } = require('./macos-verdict.js');

// runs: newest first; each {id, head_sha, jobs: [{name,status,conclusion}]}
const fakeGithub = (runs, { failJobs = false } = {}) => ({
  rest: { actions: {
    listWorkflowRuns: async () => ({ data: { workflow_runs: runs.map(({ jobs, ...r }) => r) } }),
    listJobsForWorkflowRun: async ({ run_id }) => {
      if (failJobs) throw new Error('502');
      return { data: { jobs: runs.find((r) => r.id === run_id).jobs } };
    },
  } },
});
const mac = (status, conclusion) => ({ name: 'Test (macOS)', status, conclusion, html_url: 'u', completed_at: 't' });
const run = (id, ...jobs) => ({ id, head_sha: String(id).repeat(10), jobs });
const ask = (runs, o) => latestMacosVerdict({ github: fakeGithub(runs, o), owner: 'o', repo: 'r' });

test('the newest run whose macOS job completed decides; cancelled/in-progress jobs are skipped', async () => {
  const v = await ask([
    run(5, mac('in_progress', null)),
    run(4, mac('completed', 'cancelled')),
    run(3, mac('completed', 'failure')),
    run(2, mac('completed', 'success')),
  ]);
  assert.equal(v.verdict, 'failure');
  assert.equal(v.runId, 3);
});

test('a run still going counts once its macOS job is done; no verdict is null', async () => {
  const v = await ask([run(9, { name: 'Build', status: 'queued' }, mac('completed', 'success'))]);
  assert.equal(v.verdict, 'success');
  assert.equal(await ask([run(1, mac('completed', 'cancelled')), run(2)]), null);
});

test('release gate: red fails, green notices, no verdict warns and proceeds', async () => {
  const mk = () => { const out = { failed: [], warn: [], notice: [] };
    return { out, core: { setFailed: (m) => out.failed.push(m), warning: (m) => out.warn.push(m), notice: (m) => out.notice.push(m) } }; };
  const ctx = { repo: { owner: 'o', repo: 'r' } };
  for (const [runs, expect] of [
    [[run(3, mac('completed', 'failure'))], 'failed'],
    [[run(3, mac('completed', 'success'))], 'notice'],
    [[run(3, mac('completed', 'cancelled'))], 'warn'],
  ]) {
    const { out, core } = mk();
    await gateRelease({ github: fakeGithub(runs), core, context: ctx });
    assert.equal(out[expect].length, 1, expect);
    for (const other of ['failed', 'warn', 'notice'].filter((k) => k !== expect)) assert.equal(out[other].length, 0, `${expect}: ${other}`);
  }
});
