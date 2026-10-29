const assert = require('node:assert/strict');
const test = require('node:test');
const { parseClosingRefs: p, closeFixedIssues } = require('./close-fixed-issues.js');

// The grammar is deliberately narrow: only the title, and body lines that BEGIN
// at column 0 with the keyword, outside fenced blocks.

test('a column-0 body line closes exactly the listed issues: lists, separators, keyword forms', () => {
  assert.deepEqual(p('', 'Fixes #5'), [5]);
  assert.deepEqual(p('', 'Fixes #1, #2'), [1, 2]);
  assert.deepEqual(p('', 'Fixes #1, #2, #3'), [1, 2, 3]);
  assert.deepEqual(p('', 'Closes #1, #2 and #3'), [1, 2, 3]);
  assert.deepEqual(p('', 'Resolves #1, #2, and #3'), [1, 2, 3]);
  assert.deepEqual(p('', 'fixes #1 #2 #3'), [1, 2, 3]);
  assert.deepEqual(p('', 'FIXES #1 & #2'), [1, 2]);
  assert.deepEqual(p('', 'Fixes: #7'), [7]);
  assert.deepEqual(p('', 'Fixed #1, closed #2'), [1]);
  assert.deepEqual(p('', 'Closed #2'), [2]);
  assert.deepEqual(p('', 'Intro paragraph.\n\nFixes #1, #2\nCloses #4\n\nMore text #9'), [1, 2, 4]);
  assert.deepEqual(p('', 'Fixes #1\r\nCloses #2\r\n'), [1, 2]);
});

test('the title closes anywhere in it, including a parenthesised last item', () => {
  assert.deepEqual(p('feat: thing (Fixes #3)', ''), [3]);
  assert.deepEqual(p('fix: x, closes #4 and #5', ''), [4, 5]);
  assert.deepEqual(p('Fixes #6', 'no declaration'), [6]);
  assert.deepEqual(p('feat: thing (Fixes #3)', 'Fixes #1, #2'), [1, 2, 3]);
  assert.deepEqual(p('Use `Fixes #123` in titles', ''), []);
  assert.deepEqual(p('Use ``Fixes #123`` in titles', ''), []);
  assert.deepEqual(p('Refs #3', ''), []);
});

test('NOTHING that is not a column-0 declaration closes: indented, quoted, listed, mid-sentence, inline code, comments', () => {
  const none = (body) => assert.deepEqual(p('', body), [], JSON.stringify(body));
  none('    Fixes #123');                      // 4-space indented code block
  none('\tFixes #123');                       // tab-indented code block
  none('  Fixes #123');                        // any indentation
  none('Example:\n\n    Fixes #123\n');
  none('> Fixes #123');                        // blockquote
  none('> > Fixes #123');
  none('- Fixes #123');                        // list items
  none('* Fixes #123');
  none('1. Fixes #123');
  none('- [ ] Fixes #123');
  none('this fixes #9');                       // mid-sentence
  none('Use `Fixes #123` to link');
  none('Use ``Fixes #123`` to link');
  none('`Fixes #123`');                        // starts with a code span
  none('``Fixes #123``');
  none('<!-- Fixes #5 -->');
  none('<!--\nFixes #5\n-->');
  none('See Fixes #5 above');
  none('Refs #5');
  none('Related to #5, see #6');
  none('Prefixes #3 and unfixes #4');
  none('Fixes the bug in #5');
});

test('fenced blocks of 3+ backticks or tildes are ignored, closed only by an equal-or-longer fence of the same character', () => {
  assert.deepEqual(p('', 'Intro\n```\nFixes #5\n```\nFixes #6'), [6]);
  assert.deepEqual(p('', '~~~md\nCloses #5\n~~~\nCloses #6'), [6]);
  assert.deepEqual(p('', '````\n```\nFixes #5\n```\n````\nFixes #6'), [6]);
  assert.deepEqual(p('', '~~~~\n~~~\nCloses #5\n~~~~\nCloses #6'), [6]);
  assert.deepEqual(p('', '```js\nFixes #5\n```\nFixes #6'), [6]);
  assert.deepEqual(p('', '```\n~~~\nFixes #5\n```\nFixes #6'), [6]);
  assert.deepEqual(p('', '```\nunterminated\nFixes #5'), []);
});

test('ranges up to 20 are expanded; wider or backwards ones are ignored', () => {
  assert.deepEqual(p('', 'Fixes #5975-#5980'), [5975, 5976, 5977, 5978, 5979, 5980]);
  assert.deepEqual(p('', 'Fixes #10 - #12, #20'), [10, 11, 12, 20]);
  assert.equal(p('', 'Fixes #100-#119').length, 20);
  assert.deepEqual(p('', 'Fixes #100-#120'), []);
  assert.deepEqual(p('', 'Fixes #9-#3'), []);
  assert.deepEqual(p('', 'Fixes #5\u2013#7'), [5, 6, 7]);
});

test('the list stops at the first non-item; a trailing ")" or "." is fine', () => {
  assert.deepEqual(p('', 'Fixes #1, refs #2'), [1]);
  assert.deepEqual(p('', 'Fixes #1\nRefs #2'), [1]);
  assert.deepEqual(p('', 'Fixes #1, #2.'), [1, 2]);
  assert.deepEqual(p('', 'Fixes #1 (and #2)'), [1]);
});

test('another repository, URLs and non-numeric refs are never closed', () => {
  assert.deepEqual(p('', 'Fixes other/repo#5'), []);
  assert.deepEqual(p('', 'Fixes #1, other/repo#2'), [1]);
  assert.deepEqual(p('', 'Fixes https://github.com/o/r/issues/5'), []);
  assert.deepEqual(p('', 'Fixes #12abc'), []);
});

test('references with more than 7 digits are rejected before any range is expanded (no hang)', () => {
  assert.deepEqual(p('', 'Fixes #9007199254740992'), []);
  assert.deepEqual(p('', 'Fixes #9007199254740992-#9007199254740993'), []);
  assert.deepEqual(p('', 'Fixes #5-#99999999999'), []);
  assert.deepEqual(p('', 'Fixes #1, #12345678, #3'), [1]);
  assert.deepEqual(p('', 'Fixes #9999999'), [9999999]);
  assert.deepEqual(p('', 'Fixes #99999999999999999999999'), []);
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
  const closed = await closeFixedIssues({ github, core, owner: 'o', repo: 'r', pr: 9, title: 't', body: 'Fixes #1-#5, #6, #9', baseRef: 'master', defaultBranch: 'master' });
  assert.deepEqual(closed, [1, 2]);
  assert.deepEqual(log.map((a) => a.issue_number), [1, 2]);
  assert.equal(log[0].body, 'old body\n\nClosed by #9 (merged)');
  assert.equal(log[1].body, 'Closed by #9 (merged)');
  for (const a of log) { assert.equal(a.state, 'closed'); assert.equal(a.state_reason, 'completed'); }
  assert.ok(core.warns.some((w) => /#6/.test(w)), 'a missing issue warns instead of failing');
});

test('closeFixedIssues: Refs-only PR touches nothing', async () => {
  const log = [];
  const closed = await closeFixedIssues({ github: mkGithub({ 1: { state: 'open', body: '' } }, log), core, owner: 'o', repo: 'r', pr: 9, title: 'Refs #1', body: 'Refs #1', baseRef: 'master', defaultBranch: 'master' });
  assert.deepEqual(closed, []);
  assert.equal(log.length, 0);
});

test('closeFixedIssues: a PR merged into a non-default base (a stacked PR) closes nothing', async () => {
  const log = [];
  const github = mkGithub({ 1: { state: 'open', body: 'b' } }, log);
  for (const [baseRef, defaultBranch] of [['siyer/parent-branch', 'master'], ['master', 'main'], [undefined, 'master'], ['master', undefined]]) {
    const closed = await closeFixedIssues({ github, core, owner: 'o', repo: 'r', pr: 9, title: 'Fixes #1', body: '', baseRef, defaultBranch });
    assert.deepEqual(closed, [], `${baseRef} vs ${defaultBranch}`);
  }
  assert.equal(log.length, 0);
  // Control: the same PR into the default branch does close it.
  assert.deepEqual(await closeFixedIssues({ github, core, owner: 'o', repo: 'r', pr: 9, title: 'Fixes #1', body: '', baseRef: 'master', defaultBranch: 'master' }), [1]);
});

test('closeFixedIssues: a read-only token (fork PR) warns and finishes instead of throwing', async () => {
  const warns = [];
  const github = { rest: { issues: {
    get: async () => ({ data: { state: 'open', body: 'b' } }),
    update: async () => { const e = new Error('Resource not accessible by integration'); e.status = 403; throw e; },
  } } };
  const closed = await closeFixedIssues({ github, core: { info() {}, notice() {}, warning: (m) => warns.push(m) }, owner: 'o', repo: 'r', pr: 9, title: 'Fixes #1, #2', body: '', baseRef: 'master', defaultBranch: 'master' });
  assert.deepEqual(closed, []);
  assert.equal(warns.length, 2);
  assert.match(warns[0], /read-only/);
});

test('closeFixedIssues reports every failed close to its caller', async () => {
  const failed = [];
  const github = { rest: { issues: {
    get: async () => ({ data: { state: 'open', body: 'b' } }),
    update: async ({ issue_number }) => { if (issue_number === 2) { const e = new Error('nope'); e.status = 403; throw e; } },
  } } };
  const closed = await closeFixedIssues({ github, core: { info() {}, notice() {}, warning() {} }, owner: 'o', repo: 'r', pr: 9, title: 'Fixes #1, #2', body: '', baseRef: 'master', defaultBranch: 'master', failed });
  assert.deepEqual(closed, [1]);
  assert.deepEqual(failed.map((f) => f.number), [2]);
});

const { run } = require('./close-fixed-issues.js');
test('run(): a failed close fails the job and lands in the job summary; success stays green', async () => {
  const mk = (updateFails) => {
    const out = { failed: [], summary: [] };
    const core = { info() {}, notice() {}, warning() {}, setFailed: (m) => out.failed.push(m),
      summary: { addHeading(h) { out.summary.push(h); return this; }, addList(l) { out.summary.push(...l); return this; }, write: async () => {} } };
    const github = { rest: { issues: {
      get: async () => ({ data: { state: 'open', body: 'b' } }),
      update: async () => { if (updateFails) { const e = new Error('denied'); e.status = 403; throw e; } },
    } } };
    return { out, core, github };
  };
  Object.assign(process.env, { PR_NUMBER: '9', PR_TITLE: 't', PR_BODY: 'Fixes #1', PR_BASE_REF: 'master', DEFAULT_BRANCH: 'master' });
  const bad = mk(true);
  await run({ github: bad.github, core: bad.core, context: { repo: { owner: 'o', repo: 'r' } } });
  assert.equal(bad.out.failed.length, 1);
  assert.ok(bad.out.summary.some((l) => /#1/.test(l)));
  const good = mk(false);
  await run({ github: good.github, core: good.core, context: { repo: { owner: 'o', repo: 'r' } } });
  assert.equal(good.out.failed.length, 0);
});
