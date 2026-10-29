const assert = require('node:assert/strict');
const test = require('node:test');
const { selectMergeable, hasUnansweredP1, checksGreen, touchesWorkflows, workflowState, combineVerification, MASTER_WORKFLOWS, filesQuery, WORKFLOW_SKIP_REASON, mergeAll } = require('./auto-merge.js');

const run = (name, conclusion = 'SUCCESS', status = 'COMPLETED') => ({ __typename: 'CheckRun', name, status, conclusion });
const green = [run('Build'), run('Lint'), run('Test', 'FAILURE')];
const p1 = (replies = []) => ({
  comments: { pageInfo: { hasNextPage: false }, nodes: [
    { author: { login: 'chatgpt-codex-connector' }, body: '![P1 Badge](x) bad' },
    ...replies.map((body) => ({ author: { login: 'someone' }, body })),
  ] },
});
const pr = (over = {}) => ({
  number: 1, headRefOid: 'a'.repeat(40), isDraft: false, mergeable: 'MERGEABLE',
  labels: { nodes: [] },
  commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes: green } } } }] },
  reviewThreads: { pageInfo: { hasNextPage: false }, nodes: [] },
  ...over,
});
const reason = (p) => selectMergeable([p])[0].reason;

test('a clean PR is selected (Test failure is irrelevant)', () => assert.equal(reason(pr()), null));
test('draft, hold labels, and non-MERGEABLE are skipped', () => {
  assert.ok(reason(pr({ isDraft: true })));
  assert.ok(reason(pr({ labels: { nodes: [{ name: 'hold' }] } })));
  assert.ok(reason(pr({ labels: { nodes: [{ name: 'no-auto-merge' }] } })));
  assert.ok(reason(pr({ mergeable: 'CONFLICTING' })));
  assert.ok(reason(pr({ mergeable: 'UNKNOWN' })));
});
test('Build and Lint must both be completed successes', () => {
  const rollup = (nodes) => ({ commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes } } } }] } });
  assert.equal(checksGreen(pr(rollup([run('Build')]))), false);
  assert.equal(checksGreen(pr(rollup([run('Build'), run('Lint', null, 'IN_PROGRESS')]))), false);
  assert.equal(checksGreen(pr(rollup([run('Build'), run('Lint', 'FAILURE')]))), false);
  assert.equal(checksGreen(pr(rollup([run('Build (skipped by probe)'), run('Lint')]))), false);
  assert.equal(checksGreen(pr({ commits: { nodes: [{ commit: { statusCheckRollup: null } }] } })), false);
});
test('unanswered P1 blocks; RESOLVED/ACCEPTED/[gate-ack] reply clears; P2 ignored', () => {
  const threads = (...nodes) => ({ reviewThreads: { pageInfo: { hasNextPage: false }, nodes } });
  assert.equal(hasUnansweredP1(pr(threads(p1()))), true);
  assert.equal(hasUnansweredP1(pr(threads(p1(['thanks', 'RESOLVED in abc'])))), false);
  assert.equal(hasUnansweredP1(pr(threads(p1(['ACCEPTED: fine'])))), false);
  assert.equal(hasUnansweredP1(pr(threads(p1(['[gate-ack]'])))), false);
  const p2 = p1(); p2.comments.nodes[0].body = '![P2 Badge](x)';
  assert.equal(hasUnansweredP1(pr(threads(p2))), false);
  const human = p1(); human.comments.nodes[0].author.login = 'human';
  assert.equal(hasUnansweredP1(pr(threads(human))), false);
});
test('truncated thread pages fail closed', () => {
  assert.equal(hasUnansweredP1(pr({ reviewThreads: { pageInfo: { hasNextPage: true }, nodes: [] } })), true);
});
test('selection is ordered by PR number', () => {
  assert.deepEqual(selectMergeable([pr({ number: 9 }), pr({ number: 3 })]).map((e) => e.pr.number), [3, 9]);
});

test('touchesWorkflows detects .github/workflows paths and fails closed on truncation', () => {
  const f = (...paths) => ({ pageInfo: { hasNextPage: false }, nodes: paths.map((path) => ({ path })) });
  assert.equal(touchesWorkflows(f('a.go', '.github/scripts/x.js')), false);
  assert.equal(touchesWorkflows(f('a.go', '.github/workflows/build.yml')), true);
  assert.equal(touchesWorkflows({ pageInfo: { hasNextPage: true }, nodes: [] }), true);
  assert.equal(touchesWorkflows(null), true);
  assert.match(filesQuery([7]), /p7: pullRequest\(number: 7\)/);
});

function fakeCore() {
  const c = { notices: [], warnings: [], outputs: {}, listed: [] };
  c.notice = (m) => c.notices.push(m); c.warning = (m) => c.warnings.push(m); c.info = () => {};
  c.setOutput = (k, v) => { c.outputs[k] = v; };
  c.summary = { addHeading() { return this; }, addRaw() { return this; },
    addList(l) { c.listed.push(...l); return this; }, async write() {} };
  return c;
}
const HEAD = 'c'.repeat(40);
const vr = (conclusion, status = 'completed', id = 1) => ({ id, status, conclusion, head_sha: 'c'.repeat(40), html_url: 'u' });
const github_runs = { byWf: {}, dispatched: [] };
function fakeGithub(files) {
  github_runs.byWf = { 'build.yml': [vr('success')], 'lint.yml': [vr('success')] };
  github_runs.dispatched = [];
  const merged = [];
  return { merged, graphql: async (q) => {
    const rateLimit = { cost: 1, remaining: 1, resetAt: 'x' };
    if (q.includes('pullRequests(')) return { rateLimit, repository: { pullRequests: { pageInfo: { hasNextPage: false }, nodes: [pr({ number: 1 }), pr({ number: 2 })] } } };
    const repository = {};
    for (const n of [1, 2]) if (q.includes(`p${n}:`)) repository[`p${n}`] = q.includes('files(')
      ? { files: { pageInfo: { hasNextPage: false }, nodes: files[n].map((path) => ({ path })) } }
      : { reviewThreads: { pageInfo: { hasNextPage: false }, nodes: [] } };
    return { rateLimit, repository };
  }, rest: { repos: { getBranch: async () => ({ data: { commit: { sha: HEAD } } }) }, actions: { listWorkflowRuns: async ({ workflow_id }) => ({ data: { workflow_runs: github_runs.byWf[workflow_id] } }), createWorkflowDispatch: async ({ workflow_id }) => { github_runs.dispatched.push(workflow_id); } }, pulls: { merge: async ({ pull_number }) => { merged.push(pull_number); return { data: { sha: 'f'.repeat(40) } }; } },
  } };
}

test('on GITHUB_TOKEN a workflow-touching PR is skipped (not merged, run does not fail)', async () => {
  delete process.env.USING_PAT;
  const core = fakeCore(); const github = fakeGithub({ 1: ['a.go'], 2: ['.github/workflows/x.yml'] });
  await mergeAll({ github, core, context: { repo: { owner: 'o', repo: 'r' } } });
  assert.deepEqual(github.merged, [1]);
  assert.deepEqual(core.listed, ['#2']);
  assert.equal(WORKFLOW_SKIP_REASON.includes('AUTO_MERGE_TOKEN'), true);
});

test('with AUTO_MERGE_TOKEN workflow PRs are merged and the files query is skipped', async () => {
  process.env.USING_PAT = 'true';
  const core = fakeCore(); const github = fakeGithub({ 1: ['a.go'], 2: ['.github/workflows/x.yml'] });
  await mergeAll({ github, core, context: { repo: { owner: 'o', repo: 'r' } } });
  delete process.env.USING_PAT;
  assert.deepEqual(github.merged, [1, 2]);
});

test('workflowState: success -> ok; failure -> red; in progress -> wait; none live -> dispatch', () => {
  assert.equal(workflowState([vr('success')]), 'ok');
  assert.equal(workflowState([vr('failure'), vr('success')]), 'ok');
  assert.equal(workflowState([vr('cancelled'), vr('failure')]), 'red');
  assert.equal(workflowState([vr('timed_out')]), 'red');
  assert.equal(workflowState([vr(null, 'in_progress')]), 'wait');
  assert.equal(workflowState([vr(null, 'queued'), vr('cancelled')]), 'wait');
  assert.equal(workflowState([vr('cancelled')]), 'dispatch');
  assert.equal(workflowState([]), 'dispatch');
});

test('combineVerification: red beats wait beats ok', () => {
  assert.equal(combineVerification({ a: 'ok', b: 'ok' }), 'ok');
  assert.equal(combineVerification({ a: 'ok', b: 'wait' }), 'wait');
  assert.equal(combineVerification({ a: 'dispatch', b: 'ok' }), 'wait');
  assert.equal(combineVerification({ a: 'red', b: 'wait' }), 'red');
});

const ctx = { repo: { owner: 'o', repo: 'r' } };
async function runWith(runs, files = { 1: ['a.go'], 2: ['a.go'] }, mutate) {
  delete process.env.USING_PAT;
  const core = fakeCore(); const github = fakeGithub(files);
  Object.assign(github_runs.byWf, runs);
  if (mutate) mutate(github);
  await mergeAll({ github, core, context: ctx });
  return { github, core };
}

test('verified head (both success) merges', async () => {
  const { github } = await runWith({});
  assert.deepEqual(github.merged, [1, 2]);
  assert.deepEqual(github_runs.dispatched.filter((w) => w !== 'build.yml' && w !== 'lint.yml'), ['docs.yml', 'pr.yml', 'sandbox-e2e.yml', 'tui-driver-selftest.yml', 'web-selftest.yml']);
});

test('a failed Build or Lint on the head halts merging', async () => {
  for (const wf of ['build.yml', 'lint.yml']) {
    const { github, core } = await runWith({ [wf]: [vr('failure')] });
    assert.deepEqual(github.merged, [], wf);
    assert.equal(github_runs.dispatched.length, 0);
    assert.equal(core.outputs.merged, 'false');
  }
});

test('red master still lets a fix-master PR land', async () => {
  const { github } = await runWith({ 'build.yml': [vr('failure')] }, undefined, (g) => {
    const orig = g.graphql;
    g.graphql = async (q) => { const r = await orig(q); const n = r.repository.pullRequests && r.repository.pullRequests.nodes[0];
      if (n) n.labels = { nodes: [{ name: 'fix-master' }] }; return r; };
  });
  assert.deepEqual(github.merged, [1]);
});

test('queued or in-progress verification waits: nothing merged, nothing dispatched', async () => {
  const { github, core } = await runWith({ 'lint.yml': [vr(null, 'in_progress')] });
  assert.deepEqual(github.merged, []);
  assert.equal(core.outputs.merged, 'false');
  assert.deepEqual(github_runs.dispatched, []);
});

test('no live run (cancelled / never ran) dispatches the workflow and waits', async () => {
  const { github } = await runWith({ 'build.yml': [vr('cancelled')], 'lint.yml': [] });
  assert.deepEqual(github.merged, []);
  assert.deepEqual(github_runs.dispatched, ['build.yml', 'lint.yml']);
});

test('only a batch of AUTO_MERGE_BATCH_SIZE PRs merges per verified head', async () => {
  process.env.AUTO_MERGE_BATCH_SIZE = '1';
  try {
    const { github } = await runWith({});
    assert.deepEqual(github.merged, [1]);
  } finally { delete process.env.AUTO_MERGE_BATCH_SIZE; }
});

test('post-merge dispatch list includes pr.yml (master-only heavy jobs, #5674)', async () => {
  assert.ok(MASTER_WORKFLOWS.includes('pr.yml'));
  await runWith({});
  assert.ok(github_runs.dispatched.includes('pr.yml'));
  // pr.yml is dispatched with no inputs, so probe mode (workflow_dispatch input) stays off
  assert.equal(new Set(github_runs.dispatched).size, github_runs.dispatched.length);
});

// Test (macOS) runs only on master's pr.yml; its Build is cancelled with the run,
// so verifyMasterHead cannot see a red macOS job. The merger reads the verdict
// itself and halts like a red master — fix-master PRs excepted.
const withMacos = (conclusion) => (g) => {
  const actions = g.rest.actions;
  Object.assign(github_runs.byWf, { 'pr.yml': [{ id: 7, status: 'in_progress', head_sha: 'z'.repeat(40), html_url: 'u' }] });
  actions.listJobsForWorkflowRun = async () => ({ data: { jobs: [{ name: 'Test (macOS)', status: 'completed', conclusion, html_url: 'j' }] } });
};
test('a red Test (macOS) verdict halts merging even with green Build/Lint; fix-master still lands', async () => {
  const red = await runWith({}, undefined, withMacos('failure'));
  assert.deepEqual(red.github.merged, []);
  assert.equal(red.core.outputs.merged, 'false');
  const fix = await runWith({}, undefined, (g) => {
    withMacos('failure')(g);
    const orig = g.graphql;
    g.graphql = async (q) => { const r = await orig(q); const n = r.repository.pullRequests && r.repository.pullRequests.nodes[0];
      if (n) n.labels = { nodes: [{ name: 'fix-master' }] }; return r; };
  });
  assert.deepEqual(fix.github.merged, [1]);
});

test('a green Test (macOS) verdict, or an unreadable one, does not stop merging', async () => {
  assert.deepEqual((await runWith({}, undefined, withMacos('success'))).github.merged, [1, 2]);
  const unreadable = await runWith({}, undefined, (g) => {
    withMacos('failure')(g);
    g.rest.actions.listJobsForWorkflowRun = async () => { throw new Error('502'); };
  });
  assert.deepEqual(unreadable.github.merged, [1, 2]);
  assert.ok(unreadable.core.warnings.some((w) => /Test \(macOS\) verdict/.test(w)));
});
