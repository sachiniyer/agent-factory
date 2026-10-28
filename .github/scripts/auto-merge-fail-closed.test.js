const assert = require('node:assert/strict');
const test = require('node:test');
const { isTruncated, ACK_RE, selectMergeable, hasUnansweredP1, checksGreen } = require('./auto-merge.js');

const run = (name) => ({ __typename: 'CheckRun', name, status: 'COMPLETED', conclusion: 'SUCCESS' });
const green = [run('Build'), run('Lint')];
const p1 = (replies = []) => ({
  comments: { pageInfo: { hasNextPage: false }, nodes: [
    { author: { login: 'chatgpt-codex-connector' }, body: '![P1 Badge](x) bad' },
    ...replies.map((body) => ({ author: { login: 'someone' }, body })),
  ] },
});
const pr = (over = {}) => ({
  number: 1, isDraft: false, mergeable: 'MERGEABLE', labels: { nodes: [] },
  commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes: green } } } }] },
  reviewThreads: { pageInfo: { hasNextPage: false }, nodes: [] },
  ...over,
});
const threads = (...nodes) => ({ reviewThreads: { pageInfo: { hasNextPage: false }, nodes } });

test('ack marker must START the reply: UNRESOLVED / NOT ACCEPTED do not clear a P1 (#5676)', () => {
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
  const reason = (p) => selectMergeable([p])[0].reason;
  // a hold label may sit beyond the page we read
  assert.ok(reason(pr({ labels: { pageInfo: { hasNextPage: true }, nodes: [] } })));
  // Build/Lint green in the first 100 contexts, but more exist (a failing duplicate may be hidden)
  const ctx = (extra) => ({ commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes: green, ...extra } } } }] } });
  assert.equal(checksGreen(pr(ctx({ pageInfo: { hasNextPage: true } }))), false);
  assert.equal(checksGreen(pr(ctx({ totalCount: 150 }))), false);
  assert.equal(checksGreen(pr(ctx({ totalCount: 2, pageInfo: { hasNextPage: false } }))), true);
  assert.equal(reason(pr()), null);
});
