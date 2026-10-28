const assert = require('node:assert/strict');
const test = require('node:test');
const { branchDeletionDecision, mergeAll } = require('./auto-merge.js');

const pr = (over = {}) => ({ number: 7, headRefName: 'feat-x', isCrossRepository: false, ...over });
const decide = (over = {}) => branchDeletionDecision({
  pr: pr(over.pr), defaultBranch: 'master',
  branch: 'branch' in over ? over.branch : { protected: false },
  stackedOpenPrs: over.stacked || 0,
});

test('same-repo, unprotected, unstacked head branch is deleted', () => {
  assert.deepEqual(decide(), { delete: true });
});
test('fork heads are never deleted', () => {
  const d = decide({ pr: { isCrossRepository: true } });
  assert.equal(d.delete, false); assert.match(d.reason, /fork/);
});
test('default branch is never deleted (even if a PR head is named master)', () => {
  assert.match(decide({ pr: { headRefName: 'master' } }).reason, /default/);
});
test('protected branches are never deleted', () => {
  assert.match(decide({ branch: { protected: true } }).reason, /protected/);
});
test('a branch used as the base of an open PR is kept (stacked PR)', () => {
  const d = decide({ stacked: 1 });
  assert.equal(d.delete, false); assert.match(d.reason, /base/);
});
test('an already-deleted branch is skipped', () => {
  assert.match(decide({ branch: null }).reason, /gone/);
});

// End to end through mergeAll: which branches get a deleteRef call.
function world(prs, { protectedNames = [], stackedBases = [], gone = [] } = {}) {
  const deleted = [];
  const github = {
    deleted,
    graphql: async (q) => {
      const rateLimit = { cost: 1, remaining: 1, resetAt: 'x' };
      if (q.includes('pullRequests(')) return { rateLimit, repository: { pullRequests: { pageInfo: { hasNextPage: false }, nodes: prs } } };
      const repository = {};
      for (const p of prs) repository[`p${p.number}`] = q.includes('files(')
        ? { files: { pageInfo: { hasNextPage: false }, nodes: [{ path: 'a.go' }] } }
        : { reviewThreads: { pageInfo: { hasNextPage: false }, nodes: [] } };
      return { rateLimit, repository };
    },
    rest: {
      repos: { getBranch: async ({ branch }) => {
        if (branch === 'master') return { data: { commit: { sha: 'c'.repeat(40) } } };
        if (gone.includes(branch)) { const e = new Error('Not Found'); e.status = 404; throw e; }
        return { data: { protected: protectedNames.includes(branch) } };
      } },
      actions: {
        listWorkflowRuns: async () => ({ data: { workflow_runs: [{ status: 'completed', conclusion: 'success', head_sha: 'c'.repeat(40) }] } }),
        createWorkflowDispatch: async () => {},
      },
      pulls: {
        merge: async () => ({ data: { sha: 'f'.repeat(40) } }),
        list: async ({ base }) => ({ data: stackedBases.includes(base) ? [{ number: 99 }] : [] }),
      },
      git: { deleteRef: async ({ ref }) => { deleted.push(ref); } },
    },
  };
  return github;
}
const green = [{ __typename: 'CheckRun', name: 'Build', status: 'COMPLETED', conclusion: 'SUCCESS' }, { __typename: 'CheckRun', name: 'Lint', status: 'COMPLETED', conclusion: 'SUCCESS' }];
const open = (number, headRefName, over = {}) => ({
  number, headRefName, isCrossRepository: false, headRefOid: 'a'.repeat(40), isDraft: false, mergeable: 'MERGEABLE',
  labels: { nodes: [] }, commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes: green } } } }] }, ...over,
});
function core() {
  const c = { outputs: {} };
  c.notice = () => {}; c.warning = () => {}; c.info = () => {}; c.setOutput = (k, v) => { c.outputs[k] = v; };
  c.summary = { addHeading() { return this; }, addRaw() { return this; }, addList() { return this; }, async write() {} };
  return c;
}

test('mergeAll deletes only eligible merged head branches', async () => {
  delete process.env.AUTO_MERGE_BATCH_SIZE; process.env.USING_PAT = 'true';
  const github = world(
    [open(1, 'plain'), open(2, 'forked', { isCrossRepository: true }), open(3, 'prot'), open(4, 'stack'), open(5, 'vanished')],
    { protectedNames: ['prot'], stackedBases: ['stack'], gone: ['vanished'] },
  );
  await mergeAll({ github, core: core(), context: { repo: { owner: 'o', repo: 'r' } } });
  delete process.env.USING_PAT;
  assert.deepEqual(github.deleted, ['heads/plain']);
});

test('a failing branch delete never fails the run or un-merges', async () => {
  process.env.USING_PAT = 'true';
  const github = world([open(1, 'plain')]);
  github.rest.git.deleteRef = async () => { throw new Error('boom'); };
  const c = core();
  await mergeAll({ github, core: c, context: { repo: { owner: 'o', repo: 'r' } } });
  delete process.env.USING_PAT;
  assert.equal(c.outputs.merged, 'true');
});
