const assert = require('node:assert/strict');
const test = require('node:test');
const { selectMergeable, isDetailPr } = require('./auto-merge.js');

const run = (name) => ({ __typename: 'CheckRun', name, status: 'COMPLETED', conclusion: 'SUCCESS' });
const finding = (level, replies = []) => ({
  comments: { pageInfo: { hasNextPage: false }, nodes: [
    { author: { login: 'chatgpt-codex-connector' }, body: `![${level} Badge](x) finding` },
    ...replies.map((body) => ({ author: { login: 'someone' }, body })),
  ] },
});
const pr = (headRefName, ...threads) => ({
  number: 1, headRefName, isDraft: false, mergeable: 'MERGEABLE', labels: { nodes: [] },
  commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes: [run('Build'), run('Lint')] } } } }] },
  reviewThreads: { pageInfo: { hasNextPage: false }, nodes: threads },
});
const reason = (p) => selectMergeable([p])[0].reason;
const DETAIL_P2 = 'unanswered Codex P2 on a Detail PR';

test('a Detail PR with an unanswered Codex P2 is skipped, with the reason', () => {
  assert.equal(reason(pr('detail/fix-docs-1', finding('P2'))), DETAIL_P2);
});

test('a Detail PR clears once its P2 is answered (RESOLVED / ACCEPTED / gate-ack)', () => {
  for (const ack of ['RESOLVED in abc', 'ACCEPTED: out of scope', '[gate-ack]']) {
    assert.equal(reason(pr('detail/fix-docs-1', finding('P2', [ack]))), null, ack);
  }
  // A non-acknowledging reply does not clear it.
  assert.equal(reason(pr('detail/fix-docs-1', finding('P2', ['thanks']))), DETAIL_P2);
});

test('a non-Detail PR with the same unanswered P2 still merges (throughput mode)', () => {
  for (const branch of ['siyer/fix', 'devin/x', 'feature/detail/x', 'detail', 'Detail/x', '']) {
    assert.equal(reason(pr(branch, finding('P2'))), null, branch);
  }
});

test('P1 still blocks every PR, Detail or not, and is reported first', () => {
  assert.equal(reason(pr('siyer/fix', finding('P1'))), 'unanswered P1 Codex finding');
  assert.equal(reason(pr('detail/x', finding('P1'))), 'unanswered P1 Codex finding');
  assert.equal(reason(pr('detail/x', finding('P2'), finding('P1'))), 'unanswered P1 Codex finding');
});

test('P3 stays advisory even on a Detail PR; a clean Detail PR merges', () => {
  assert.equal(reason(pr('detail/x', finding('P3'))), null);
  assert.equal(reason(pr('detail/x')), null);
});

test('a truncated thread page fails closed on a Detail PR, and a truncated comment page on its P2 thread', () => {
  const truncatedThreads = pr('detail/x');
  truncatedThreads.reviewThreads.pageInfo.hasNextPage = true;
  assert.ok(reason(truncatedThreads));
  const longThread = finding('P2', ['RESOLVED in abc']);
  longThread.comments.pageInfo.hasNextPage = true;
  assert.equal(reason(pr('detail/x', longThread)), DETAIL_P2);
  // …while the same truncated P2 thread on a non-Detail PR is ignored.
  assert.equal(reason(pr('siyer/x', longThread)), null);
});

test('isDetailPr matches only a head branch that starts with detail/', () => {
  assert.equal(isDetailPr({ headRefName: 'detail/a' }), true);
  assert.equal(isDetailPr({ headRefName: 'x/detail/a' }), false);
  assert.equal(isDetailPr({}), false);
});
