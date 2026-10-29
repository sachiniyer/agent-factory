const assert = require('node:assert/strict');
const test = require('node:test');
const { parseClosingRefs: p, closeFixedIssues } = require('./close-fixed-issues.js');

test('single keyword, lists with commas, "and", "&", and plain spaces', () => {
  assert.deepEqual(p('', 'Fixes #5'), [5]);
  assert.deepEqual(p('', 'Fixes #1, #2, #3'), [1, 2, 3]);
  assert.deepEqual(p('', 'Closes #1, #2 and #3'), [1, 2, 3]);
  assert.deepEqual(p('', 'Resolves #1, #2, and #3'), [1, 2, 3]);
  assert.deepEqual(p('', 'fixes #1 #2 #3'), [1, 2, 3]);
  assert.deepEqual(p('', 'Fixes #1 & #2'), [1, 2]);
  assert.deepEqual(p('', 'Fixes: #7'), [7]);
  assert.deepEqual(p('', 'Fixed #1, closed #2, resolved #3'), [1, 2, 3]);
});

test('ranges up to 20 are expanded; wider or backwards ones are ignored', () => {
  assert.deepEqual(p('', 'Fixes #5975-#5980'), [5975, 5976, 5977, 5978, 5979, 5980]);
  assert.deepEqual(p('', 'Fixes #10 - #12, #20'), [10, 11, 12, 20]);
  assert.equal(p('', 'Fixes #100-#119').length, 20);
  assert.deepEqual(p('', 'Fixes #100-#120'), []);
  assert.deepEqual(p('', 'Fixes #9-#3'), []);
  assert.deepEqual(p('', 'Fixes #5–#7'), [5, 6, 7]);
});

test('a last item followed by ")" or punctuation is still closed', () => {
  assert.deepEqual(p('feat: thing (Fixes #12)', ''), [12]);
  assert.deepEqual(p('', '(Fixes #1, #2)'), [1, 2]);
  assert.deepEqual(p('', 'Fixes #1, #2.'), [1, 2]);
  assert.deepEqual(p('', 'Fixes #1, #2\n\nMore text #9'), [1, 2]);
});

test('Refs and non-keywords never close; the list stops at a non-item', () => {
  assert.deepEqual(p('', 'Refs #5'), []);
  assert.deepEqual(p('', 'Related to #5, see #6'), []);
  assert.deepEqual(p('', 'Fixes #1, refs #2'), [1]);
  assert.deepEqual(p('', 'Fixes #1\nRefs #2'), [1]);
  assert.deepEqual(p('', 'Prefixes #3 and unfixes #4'), []);
  assert.deepEqual(p('', 'Fixes the bug in #5'), []);
});

test('another repository, URLs and non-numeric refs are never closed', () => {
  assert.deepEqual(p('', 'Fixes other/repo#5'), []);
  assert.deepEqual(p('', 'Fixes #1, other/repo#2'), [1]);
  assert.deepEqual(p('', 'Fixes https://github.com/o/r/issues/5'), []);
  assert.deepEqual(p('', 'Fixes #12abc'), []);
});

test('code blocks, inline code and HTML comments are ignored', () => {
  assert.deepEqual(p('', 'Intro\n```\nFixes #5\n```\nFixes #6'), [6]);
  assert.deepEqual(p('', '~~~md\nCloses #5\n~~~\n'), []);
  assert.deepEqual(p('', 'Use `Fixes #5` to link'), []);
  assert.deepEqual(p('', '<!-- Fixes #5 -->\nFixes #6'), [6]);
  assert.deepEqual(p('', '```\nunterminated Fixes #5'), []);
});

test('title and body are both read; duplicates collapse; result is ascending', () => {
  assert.deepEqual(p('fix: x (Fixes #3)', 'Fixes #2, #3\n\nCloses #1'), [1, 2, 3]);
  assert.deepEqual(p(null, undefined), []);
});

const mkGithub = (issues, log) => ({ rest: { issues: {
  get: async ({ issue_number }) => { if (!issues[issue_number]) throw new Error('404'); return { data: issues[issue_number] }; },
  update: async (args) => { log.push(args); },
} } });
const core = { infos: [], warns: [], info() {}, notice() {}, warning(m) { this.warns.push(m); } };

test('closeFixedIssues: one PATCH per open issue, appending the line, no comment', async () => {
  const log = [];
  const github = mkGithub({
    1: { state: 'open', body: 'old body' },
    2: { state: 'open', body: null },
    3: { state: 'closed', body: 'x' },
    4: { state: 'open', body: 'x', pull_request: {} },
    5: { state: 'open', body: 'already\n\nClosed by #9 (merged)' },
  }, log);
  const closed = await closeFixedIssues({ github, core, owner: 'o', repo: 'r', pr: 9, title: 't', body: 'Fixes #1-#5, #6, #9' });
  assert.deepEqual(closed, [1, 2]);
  assert.deepEqual(log.map((a) => a.issue_number), [1, 2]);
  assert.equal(log[0].body, 'old body\n\nClosed by #9 (merged)');
  assert.equal(log[1].body, 'Closed by #9 (merged)');
  for (const a of log) { assert.equal(a.state, 'closed'); assert.equal(a.state_reason, 'completed'); }
  assert.ok(core.warns.some((w) => /#6/.test(w)), 'a missing issue warns instead of failing');
});

test('closeFixedIssues: Refs-only PR touches nothing', async () => {
  const log = [];
  const closed = await closeFixedIssues({ github: mkGithub({ 1: { state: 'open', body: '' } }, log), core, owner: 'o', repo: 'r', pr: 9, title: 'Refs #1', body: 'Refs #1' });
  assert.deepEqual(closed, []);
  assert.equal(log.length, 0);
});
