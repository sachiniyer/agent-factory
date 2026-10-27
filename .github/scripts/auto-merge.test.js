const assert = require('node:assert/strict');
const test = require('node:test');
const { isTruncated, ACK_RE, selectMergeable, hasUnansweredP1, checksGreen, touchesWorkflows, findRedMasterRun, filesQuery, WORKFLOW_SKIP_REASON, mergeAll } = require('./auto-merge.js');

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
const github_runs = { runs: [] };
function fakeGithub(files) {
  github_runs.runs = [];
  const merged = [];
  return { merged, graphql: async (q) => {
    const rateLimit = { cost: 1, remaining: 1, resetAt: 'x' };
    if (q.includes('pullRequests(')) return { rateLimit, repository: { pullRequests: { pageInfo: { hasNextPage: false }, nodes: [pr({ number: 1 }), pr({ number: 2 })] } } };
    const repository = {};
    for (const n of [1, 2]) if (q.includes(`p${n}:`)) repository[`p${n}`] = q.includes('files(')
      ? { files: { pageInfo: { hasNextPage: false }, nodes: files[n].map((path) => ({ path })) } }
      : { reviewThreads: { pageInfo: { hasNextPage: false }, nodes: [] } };
    return { rateLimit, repository };
  }, rest: { actions: { listWorkflowRunsForRepo: async () => ({ data: { workflow_runs: github_runs.runs } }), createWorkflowDispatch: async () => {} }, pulls: { merge: async ({ pull_number }) => { merged.push(pull_number); return { data: { sha: 'f'.repeat(40) } }; } },
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

const mr = (path, conclusion, id = 1) => ({ id, path: `.github/workflows/${path}`, name: path, conclusion, head_sha: 'b'.repeat(40), html_url: 'u' });
test('findRedMasterRun: newest completed Build/Lint decides; later pass resumes', () => {
  assert.equal(findRedMasterRun([]), null);
  assert.equal(findRedMasterRun([mr('build.yml', 'success', 3), mr('lint.yml', 'success', 2)]), null);
  assert.equal(findRedMasterRun([mr('build.yml', 'failure', 3), mr('build.yml', 'success', 1)]).id, 3);
  assert.equal(findRedMasterRun([mr('build.yml', 'success', 4), mr('build.yml', 'failure', 3)]), null);
  assert.equal(findRedMasterRun([mr('lint.yml', 'failure', 5), mr('build.yml', 'success', 4)]).id, 5);
  // cancelled runs are ignored; other workflows do not count
  assert.equal(findRedMasterRun([mr('build.yml', 'cancelled', 6), mr('build.yml', 'success', 5)]), null);
  assert.equal(findRedMasterRun([mr('docs.yml', 'failure', 7)]), null);
});

test('mergeAll merges nothing while master Build/Lint is red, and says which run', async () => {
  delete process.env.USING_PAT;
  const core = fakeCore(); const github = fakeGithub({ 1: ['a.go'], 2: ['a.go'] });
  github_runs.runs = [mr('build.yml', 'failure', 99)];
  let raw = '';
  core.summary.addRaw = function (t) { raw += t; return this; };
  await mergeAll({ github, core, context: { repo: { owner: 'o', repo: 'r' } } });
  assert.deepEqual(github.merged, []);
  assert.equal(core.outputs.merged, 'false');
  assert.match(raw, /\b99\b/);
  // a fix-master PR may still land so master can turn green
  const g2 = fakeGithub({ 1: ['a.go'], 2: ['a.go'] }); github_runs.runs = [mr('build.yml', 'failure', 99)];
  const orig = g2.graphql;
  g2.graphql = async (q) => { const r = await orig(q); const pr1 = r.repository.pullRequests && r.repository.pullRequests.nodes[0];
    if (pr1) pr1.labels = { nodes: [{ name: 'fix-master' }] }; return r; };
  await mergeAll({ github: g2, core: fakeCore(), context: { repo: { owner: 'o', repo: 'r' } } });
  assert.deepEqual(g2.merged, [1]);
  github_runs.runs = [mr('build.yml', 'failure', 99)];
  // a later passing run resumes merging
  github_runs.runs = [mr('build.yml', 'success', 100), mr('build.yml', 'failure', 99)];
  await mergeAll({ github, core, context: { repo: { owner: 'o', repo: 'r' } } });
  assert.deepEqual(github.merged, [1, 2]);
});

test('ack marker must START the reply: UNRESOLVED / NOT ACCEPTED do not clear a P1 (#5676)', () => {
  const threads = (...nodes) => ({ reviewThreads: { pageInfo: { hasNextPage: false }, nodes } });
  for (const body of ['UNRESOLVED', 'NOT ACCEPTED', 'this is not RESOLVED yet', 'unresolved: RESOLVED?', 'ACCEPTEDLY']) {
    assert.equal(hasUnansweredP1(pr(threads(p1([body])))), true, body);
  }
  for (const body of ['RESOLVED in abc', 'ACCEPTED: fine', '  RESOLVED', '**ACCEPTED** by design', '[gate-ack] ok']) {
    assert.equal(hasUnansweredP1(pr(threads(p1([body])))), false, body);
  }
  assert.ok(ACK_RE.test('RESOLVED'));
});

test('truncated labels or contexts fail closed (#5678)', () => {
  assert.equal(isTruncated({ pageInfo: { hasNextPage: true }, nodes: [] }), true);
  assert.equal(isTruncated({ totalCount: 101, nodes: new Array(100).fill({}) }), true);
  assert.equal(isTruncated({ totalCount: 2, pageInfo: { hasNextPage: false }, nodes: [{}, {}] }), false);
  assert.equal(isTruncated(undefined), false);
  // labels: a hold label may sit beyond the page
  assert.ok(reason(pr({ labels: { pageInfo: { hasNextPage: true }, nodes: [] } })));
  // contexts: Build/Lint green in the first 100 but more exist (a failing duplicate may be hidden)
  const ctx = (extra) => ({ commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes: green, ...extra } } } }] } });
  assert.equal(checksGreen(pr(ctx({ pageInfo: { hasNextPage: true } }))), false);
  assert.equal(checksGreen(pr(ctx({ totalCount: 150 }))), false);
  assert.equal(checksGreen(pr(ctx({ totalCount: 3, pageInfo: { hasNextPage: false } }))), true);
  assert.equal(reason(pr()), null);
});
