const assert = require('node:assert/strict');
const test = require('node:test');
const { findMultiRefs, messageFor, run } = require('./closing-keyword-lint.js');

const flagged = (title, body) => findMultiRefs(title, body);
const nums = (title, body) => flagged(title, body).map((f) => f.numbers);

test('several numbers after one keyword are flagged, in every separator style', () => {
  assert.deepEqual(nums('', 'Fixes #1, #2'), [[1, 2]]);
  assert.deepEqual(nums('', 'Fixes #1, #2, #3'), [[1, 2, 3]]);
  assert.deepEqual(nums('', 'Closes #1, #2 and #3'), [[1, 2, 3]]);
  assert.deepEqual(nums('', 'Resolves #1 & #2'), [[1, 2]]);
  assert.deepEqual(nums('', 'fixes #1 #2'), [[1, 2]]);
  assert.deepEqual(nums('fix: x (Fixes #3, #4)', ''), [[3, 4]]);
  assert.deepEqual(nums('', 'Some text. This PR fixes #7, #8 as well.'), [[7, 8]]);
});

test('a range is flagged and expanded (up to 20)', () => {
  assert.deepEqual(nums('', 'Fixes #5975-#5980'), [[5975, 5976, 5977, 5978, 5979, 5980]]);
  assert.equal(flagged('', 'Fixes #5975-#5980')[0].range, true);
  assert.equal(flagged('', 'Fixes #1-#500').length, 1, 'a huge range is still flagged');
  assert.deepEqual(nums('', 'Fixes #1-#500'), [[]]);
});

test('one number per keyword, or a single number, is never flagged', () => {
  assert.deepEqual(flagged('', 'Fixes #1, fixes #2, fixes #3'), []);
  assert.deepEqual(flagged('', 'Fixes #1\nFixes #2'), []);
  assert.deepEqual(flagged('', 'Fixes #1'), []);
  assert.deepEqual(flagged('Fixes #1', ''), []);
  assert.deepEqual(flagged('', 'Refs #1, #2, #3'), []);
  assert.deepEqual(flagged('', 'Fixes other/repo#1, #2'), []);
  assert.deepEqual(flagged('', 'See #1, #2 and #3'), []);
});

test('the message names the exact fix', () => {
  const [f] = flagged('', 'Fixes #1, #2');
  assert.equal(f.suggestion, 'Fixes #1, fixes #2');
  assert.equal(messageFor(f), 'Write "Fixes #1, fixes #2": GitHub closes only the first number after a keyword, so the others stay open after the merge.');
  assert.equal(flagged('', 'Closes #4, #5, #6')[0].suggestion, 'Closes #4, closes #5, closes #6');
});

test('code examples are ignored (and a missed one is only a harmless extra warning)', () => {
  assert.deepEqual(flagged('', '```\nFixes #1, #2\n```'), []);
  assert.deepEqual(flagged('', '~~~\nFixes #1, #2\n~~~'), []);
  assert.deepEqual(flagged('', 'Use `Fixes #1, #2` to link'), []);
  assert.deepEqual(flagged('', 'Use ``Fixes #1, #2`` to link'), []);
  assert.deepEqual(flagged('', '    Fixes #1, #2'), []);
  assert.deepEqual(flagged('', '<!-- Fixes #1, #2 -->'), []);
  assert.deepEqual(flagged('', '<pre>\nFixes #1, #2\n</pre>'), []);
  assert.deepEqual(flagged('', 'a <code>Fixes #1, #2</code> b'), []);
});

test('run() annotates and summarises but never fails, even on garbage', async () => {
  const mk = () => { const o = { warn: [], summary: [], failed: [] };
    return { o, core: { warning: (m) => o.warn.push(m), setFailed: (m) => o.failed.push(m),
      summary: { addHeading() { return this; }, addList(l) { o.summary.push(...l); return this; }, write: async () => {} } } }; };
  Object.assign(process.env, { PR_TITLE: 't', PR_BODY: 'Fixes #1, #2' });
  const a = mk(); await run({ core: a.core });
  assert.equal(a.o.warn.length, 1);
  assert.equal(a.o.summary.length, 1);
  assert.equal(a.o.failed.length, 0);
  Object.assign(process.env, { PR_TITLE: 't', PR_BODY: 'Fixes #1, fixes #2' });
  const b = mk(); await run({ core: b.core });
  assert.equal(b.o.warn.length + b.o.summary.length + b.o.failed.length, 0);
  delete process.env.PR_TITLE; delete process.env.PR_BODY;
  const c = mk(); await run({ core: c.core });
  assert.equal(c.o.failed.length, 0);
  // A throwing core.summary degrades to a warning, not a failed job.
  Object.assign(process.env, { PR_BODY: 'Fixes #1, #2' });
  const d = mk(); d.core.summary.write = async () => { throw new Error('boom'); };
  await run({ core: d.core });
  assert.equal(d.o.failed.length, 0);
  assert.ok(d.o.warn.some((m) => /could not run/.test(m)));
});
