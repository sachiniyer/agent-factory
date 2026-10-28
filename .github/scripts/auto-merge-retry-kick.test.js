const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const { resolveUnknownMergeable, evaluate, mergeAll, MASTER_WORKFLOWS } = require('./auto-merge.js');

process.env.AUTO_MERGE_UNKNOWN_DELAY_MS = '0';
const HEAD = 'c'.repeat(40);
const green = [
  { __typename: 'CheckRun', name: 'Build', status: 'COMPLETED', conclusion: 'SUCCESS' },
  { __typename: 'CheckRun', name: 'Lint', status: 'COMPLETED', conclusion: 'SUCCESS' },
];
const open = (number, over = {}) => ({
  number, headRefName: `b${number}`, isCrossRepository: false, headRefOid: 'a'.repeat(40), isDraft: false,
  mergeable: 'MERGEABLE', labels: { nodes: [] },
  commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes: green } } } }] }, ...over,
});
function core() {
  const c = { outputs: {}, infos: [] };
  c.notice = () => {}; c.warning = () => {}; c.info = (m) => c.infos.push(m); c.setOutput = (k, v) => { c.outputs[k] = v; };
  c.summary = { addHeading() { return this; }, addRaw() { return this; }, addList() { return this; }, async write() {} };
  return c;
}
// `later` maps PR number -> list of mergeable values returned by successive re-reads.
function world(prs, { later = {}, runs, jobs = {} } = {}) {
  const w = { merged: [], dispatched: [], rereads: [], cursor: {} };
  const verdictRuns = runs || { 'build.yml': [{ status: 'completed', conclusion: 'success' }], 'lint.yml': [{ status: 'completed', conclusion: 'success' }] };
  w.graphql = async (q) => {
    const rateLimit = { cost: 1, remaining: 1, resetAt: 'x' };
    if (q.includes('pullRequests(')) return { rateLimit, repository: { pullRequests: { pageInfo: { hasNextPage: false }, nodes: prs } } };
    const repository = {};
    if (q.includes('mergeable }')) {
      w.rereads.push([...q.matchAll(/p(\d+):/g)].map((m) => Number(m[1])));
      for (const m of q.matchAll(/p(\d+):/g)) {
        const n = Number(m[1]); const i = w.cursor[n] || 0; w.cursor[n] = i + 1;
        repository[`p${n}`] = { mergeable: (later[n] || ['UNKNOWN'])[Math.min(i, (later[n] || ['UNKNOWN']).length - 1)] };
      }
    } else {
      for (const p of prs) repository[`p${p.number}`] = q.includes('files(')
        ? { files: { pageInfo: { hasNextPage: false }, nodes: [{ path: 'a.go' }] } }
        : { reviewThreads: { pageInfo: { hasNextPage: false }, nodes: [] } };
    }
    return { rateLimit, repository };
  };
  w.rest = {
    repos: { getBranch: async ({ branch }) => (branch === 'master' ? { data: { commit: { sha: HEAD } } } : { data: { protected: true } }) },
    actions: {
      listWorkflowRuns: async ({ workflow_id }) => ({ data: { workflow_runs: (verdictRuns[workflow_id] || []).map((r) => ({ ...r, head_sha: HEAD })) } }),
      listJobsForWorkflowRun: async ({ run_id }) => ({ data: { jobs: jobs[run_id] || [] } }),
      createWorkflowDispatch: async ({ workflow_id }) => { w.dispatched.push(workflow_id); },
    },
    pulls: { merge: async ({ pull_number }) => { w.merged.push(pull_number); return { data: { sha: 'f'.repeat(40) } }; }, list: async () => ({ data: [] }) },
    git: { getRef: async () => ({ data: { object: { sha: 'a'.repeat(40) } } }), deleteRef: async () => {} },
  };
  return w;
}
const go = (w, pat = true) => { if (pat) process.env.USING_PAT = 'true'; else delete process.env.USING_PAT; return mergeAll({ github: w, core: core(), context: { repo: { owner: 'o', repo: 'r' } } }).finally(() => { delete process.env.USING_PAT; }); };

// ---- Defect 1: UNKNOWN mergeability is retried -------------------------------

test('UNKNOWN that resolves to MERGEABLE on a re-read is merged', async () => {
  const w = world([open(1, { mergeable: 'UNKNOWN' }), open(2)], { later: { 1: ['UNKNOWN', 'MERGEABLE'] } });
  await go(w);
  assert.deepEqual(w.rereads, [[1], [1]]); // only the UNKNOWN PR, twice
  assert.deepEqual(w.merged, [1, 2]);
});

test('UNKNOWN that becomes CONFLICTING stays skipped', async () => {
  const w = world([open(1, { mergeable: 'UNKNOWN' })], { later: { 1: ['CONFLICTING'] } });
  await go(w);
  assert.deepEqual(w.rereads, [[1]]);
  assert.deepEqual(w.merged, []);
});

test('still UNKNOWN after 3 re-reads is a logged skip', async () => {
  const w = world([open(1, { mergeable: 'UNKNOWN' })]);
  await go(w);
  assert.equal(w.rereads.length, 3);
  assert.deepEqual(w.merged, []);
  const pr = open(1, { mergeable: 'UNKNOWN', unknownRetries: 3 });
  assert.match(evaluate(pr), /still UNKNOWN after 3 re-reads/);
});

test('ineligible UNKNOWN PRs (draft, hold) are not re-read', async () => {
  const prs = [open(1, { mergeable: 'UNKNOWN', isDraft: true }), open(2, { mergeable: 'UNKNOWN', labels: { nodes: [{ name: 'hold' }] } })];
  const w = world(prs);
  await go(w);
  assert.deepEqual(w.rereads, []);
});

test('resolveUnknownMergeable makes no call when nothing is UNKNOWN', async () => {
  const w = world([open(1)]);
  await resolveUnknownMergeable({ github: w, core: core(), owner: 'o', repo: 'r', prs: [open(1)], delayMs: 0 });
  assert.deepEqual(w.rereads, []);
});

// ---- Defect 2: the chain terminates --------------------------------------------
// A kicked run re-evaluates master. It dispatches verification only when (a) it
// merged something (the head moved) or (b) a verification workflow has no live
// run. A verified master with nothing to merge, a red master, and a master still
// being verified dispatch nothing, so kick -> run -> kick cannot loop.

test('kicked run, master verified, nothing to merge: dispatches NOTHING', async () => {
  const w = world([]);
  await go(w);
  assert.deepEqual(w.dispatched, []);
});

test('kicked run, master red: dispatches nothing (fix-master PRs may still merge)', async () => {
  const w = world([open(1)], { runs: { 'build.yml': [{ status: 'completed', conclusion: 'failure' }], 'lint.yml': [{ status: 'completed', conclusion: 'success' }] } });
  await go(w);
  assert.deepEqual(w.dispatched, []);
  assert.deepEqual(w.merged, []);
});

test('kicked run, verification still running: waits, dispatches nothing', async () => {
  const w = world([open(1)], { runs: { 'build.yml': [{ status: 'in_progress', conclusion: null }], 'lint.yml': [{ status: 'completed', conclusion: 'success' }] } });
  await go(w);
  assert.deepEqual(w.dispatched, []);
});

test('latest run cancelled (no live run): re-dispatches only the cancelled workflow(s), merges nothing', async () => {
  const w = world([open(1)], { runs: { 'build.yml': [{ status: 'completed', conclusion: 'cancelled' }], 'lint.yml': [{ status: 'completed', conclusion: 'success' }] } });
  await go(w);
  assert.deepEqual(w.dispatched, ['build.yml']);
  assert.deepEqual(w.merged, []);
});

test('merged something: dispatches the post-merge list exactly once', async () => {
  const w = world([open(1), open(2)]);
  await go(w, false); // GITHUB_TOKEN merges fire no push, so the merger dispatches
  assert.deepEqual(w.merged, [1, 2]);
  assert.deepEqual(w.dispatched, MASTER_WORKFLOWS);
});

// ---- Workflow wiring -----------------------------------------------------------

for (const [file, job] of [['build.yml', 'build'], ['lint.yml', 'golangci']]) {
  test(`${file} ends with a gated kick-auto-merge job`, () => {
    const text = fs.readFileSync(path.join(__dirname, '..', 'workflows', file), 'utf8');
    const i = text.indexOf('  kick-auto-merge:');
    assert.ok(i > 0, 'kick job present');
    const body = text.slice(i);
    assert.match(body, new RegExp(`needs: \\[${job}\\]`));
    assert.match(body, /!cancelled\(\)/);
    assert.match(body, /github\.event_name == 'workflow_dispatch'/);
    assert.match(body, /github\.ref == 'refs\/heads\/master'/);
    assert.match(body, /actions: write/);
    assert.match(body, /gh workflow run auto-merge\.yml --ref master/);
  });
}

// ---- The kick job runs inside the run it announces ------------------------------
const job = (name, status, conclusion = null) => ({ name, status, conclusion });
const lintOk = { 'lint.yml': [{ id: 2, status: 'completed', conclusion: 'success' }] };

test('only the kick job is incomplete: verdict is ok, not wait, and the merger proceeds', async () => {
  const w = world([open(1)], {
    runs: { ...lintOk, 'build.yml': [{ id: 1, status: 'in_progress', conclusion: null }] },
    jobs: { 1: [job('Build Binary and Test (linux, amd64)', 'completed', 'success'), job('Kick auto-merge', 'in_progress')] },
  });
  await go(w);
  assert.deepEqual(w.merged, [1]);
});

test('only the kick job is incomplete but a real job failed: verdict is red, not wait', async () => {
  const w = world([open(1)], {
    runs: { ...lintOk, 'build.yml': [{ id: 1, status: 'in_progress', conclusion: null }] },
    jobs: { 1: [job('Build Binary and Test (linux, amd64)', 'completed', 'failure'), job('Kick auto-merge', 'queued')] },
  });
  await go(w);
  assert.deepEqual(w.merged, []); // red: non-fix-master PRs held
  assert.deepEqual(w.dispatched, []);
});

test('a real job still running keeps the verdict at wait: nothing merged, nothing dispatched', async () => {
  const w = world([open(1)], {
    runs: { ...lintOk, 'build.yml': [{ id: 1, status: 'in_progress', conclusion: null }] },
    jobs: { 1: [job('Build Binary and Test (linux, amd64)', 'completed', 'success'), job('Build Binary and Test (darwin, arm64)', 'in_progress'), job('Kick auto-merge', 'queued')] },
  });
  await go(w);
  assert.deepEqual(w.merged, []);
  assert.deepEqual(w.dispatched, []);
});

test('an unreadable jobs list stays at wait (conservative)', async () => {
  const w = world([open(1)], { runs: { ...lintOk, 'build.yml': [{ id: 1, status: 'in_progress', conclusion: null }] } });
  w.rest.actions.listJobsForWorkflowRun = async () => { throw new Error('boom'); };
  await go(w);
  assert.deepEqual(w.merged, []);
});
