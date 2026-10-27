const assert = require('node:assert/strict');
const test = require('node:test');
const { selectMergeable, hasUnansweredP1, checksGreen } = require('./auto-merge.js');

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
