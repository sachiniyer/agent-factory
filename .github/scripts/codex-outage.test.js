const assert = require('node:assert/strict');
const test = require('node:test');
const { aggregate, render, readRecord, sweep, gateNotice } = require('./codex-outage.js');
const gate = require('./auto-gate.js');
const t = (hour) => `2026-09-05T${String(hour).padStart(2, '0')}:00:00.000Z`;
const head = 'a'.repeat(40);
const limits = [
  'You have reached your Codex usage limits for code ' + 'reviews.',
  'Codex usage limits have been reached for code reviews. Please check with the admins of this repo to increase the limits by adding credits.',
  'You have reached your Codex usage limits.',
];
const comment = (hour, body, extra = {}) => ({ created_at: t(hour), body,
  user: { login: 'chatgpt-codex-connector[bot]' }, html_url: `https://example.com/${hour}`, ...extra });
const verdict = (hour, sha = head) => comment(hour, `Codex Review\nReviewed commit: \`${sha.slice(0, 10)}\``);
function fixture() {
  return [
    { number: 1, head: { sha: head }, merged_at: t(4), artifacts: [verdict(1), comment(2, limits[0]), comment(3, limits[1])] },
    { number: 2, head: { sha: head }, merged_at: t(4), artifacts: [comment(3, limits[2]), verdict(7)] },
    { number: 3, head: { sha: head }, merged_at: t(6), artifacts: [comment(4, limits[1]), verdict(5)] },
    { number: 4, head: { sha: head }, merged_at: t(10), artifacts: [comment(8, limits[2]), comment(9, limits[1])] },
    { number: 5, head: { sha: head }, merged_at: null, artifacts: [comment(11, limits[0], { user: { login: 'someone' } })] },
  ];
}
test('shared predicate recognizes all three observed wordings', () => {
  for (const body of limits) assert.equal(gate.codexEvidence.codexReportsReviewUsageLimit(body), true);
});
test('mixed history closes at first verdict, counts merged heads once, keeps recurrence', () => {
  const episodes = aggregate(fixture(), t(12));
  assert.deepEqual(episodes.map(e => [e.start, e.end, e.merged]), [
    [t(2), t(5), [2]], [t(8), null, [4]],
  ]);
  assert.equal(episodes[0].latest.time, t(4));
  const body = render(episodes, t(12));
  assert.match(body, /unavailable since 2026-09-05T08/);
  assert.match(body, /4.0h/);
  assert.match(body, /Recovered: 2026-09-05T05/);
  assert.deepEqual(readRecord({ body, user: { login: 'sachiniyer' } }).episodes, episodes);
  assert.equal(readRecord({ body, user: { login: 'someone' } }), null);
  assert.deepEqual(aggregate(fixture().reverse(), t(12)), episodes);
});
test('late verdict cannot cover an earlier merge and quoted notices do not start outages', () => {
  const pulls = [{ number: 1, head: { sha: head }, merged_at: t(4), artifacts: [
    comment(2, limits[1]), verdict(5), comment(6, `Codex Review\nReviewed commit: \`${head}\`\n${limits[0]}`),
  ] }];
  assert.deepEqual(aggregate(pulls, t(8)).map(e => [e.start, e.end, e.merged]), [[t(2), t(5), [1]]]);
});
test('sweep paginates both comment endpoints unfiltered, updates existing record', async () => {
  const calls = [];
  const existing = { id: 42, user: { login: 'sachiniyer' }, body: render([], t(1)) };
  const api = async (route, options = {}) => {
    calls.push([route, options]);
    if (options.method === 'PATCH') return { ...existing, body: options.body };
    if (route.includes('/issues/3932/comments')) return [existing];
    if (route.includes('/pulls?')) return [{ number: 2, updated_at: t(9), merged_at: t(8), head: { sha: head } }];
    if (route.endsWith('/pulls/2/comments?per_page=100')) return [comment(2, limits[1], { line: null, in_reply_to_id: 5 })];
    if (route.endsWith('/issues/2/comments?per_page=100')) return [comment(3, limits[2])];
    if (route.endsWith('/pulls/2/reviews?per_page=100')) return [];
    throw new Error(route);
  };
  await sweep(api, 'owner/repo', t(10));
  const write = calls.find(([, o]) => o.method === 'PATCH');
  assert.equal(write[0], 'repos/owner/repo/issues/comments/42');
  assert.match(write[1].body, /Degraded merges: 1/);
  assert.equal(calls.filter(([, o]) => o.method === 'POST').length, 0);
});
test('gate duration uses shared record and falls back without changing policy', async () => {
  const episodes = aggregate(fixture(), t(12));
  const github = { rest: { issues: { listComments() {} } }, paginate: async () => [
    { body: render(episodes, t(12)), user: { login: 'sachiniyer' }, html_url: 'https://example.com/record' },
  ] };
  assert.match(await gateNotice({ github, context: { repo: {} }, since: t(9), now: t(12) }), /since .*08:00.*4.0h ago/);
  github.paginate = async () => { throw Error('offline'); };
  assert.match(await gateNotice({ github, context: { repo: {} }, since: t(9), now: t(12) }), /3.0h ago.*record unavailable/);
});
test('a failed sweep preserves the previous record; a first sweep creates only one', async () => {
  const writes = [];
  let record;
  let fail = false;
  const api = async (route, options = {}) => {
    if (options.method) {
      writes.push(options.method);
      record = { id: 42, user: { login: 'sachiniyer' }, body: options.body };
      return record;
    }
    if (route.includes('/issues/3932/comments')) return record ? [record] : [];
    if (route.includes('/pulls?')) return [{ number: 2, updated_at: t(9), merged_at: t(8), head: { sha: head } }];
    if (route.includes('/issues/2/comments')) {
      if (fail) throw Error('API read failed');
      return [comment(2, limits[1])];
    }
    return [];
  };
  await sweep(api, 'owner/repo', t(10));
  await sweep(api, 'owner/repo', t(11));
  assert.deepEqual(writes, ['POST', 'PATCH']);
  const previous = record.body;
  fail = true;
  await assert.rejects(sweep(api, 'owner/repo', t(12)), /API read failed/);
  assert.equal(record.body, previous);
  assert.deepEqual(writes, ['POST', 'PATCH']);
});
test('successive sweeps close the same record and retain recovery beyond the scan window', async () => {
  let artifacts = [comment(2, limits[1])];
  let record;
  let pulls = [{ number: 2, updated_at: t(9), merged_at: t(4), head: { sha: head } }];
  const methods = [];
  const api = async (route, options = {}) => {
    if (options.method) {
      methods.push(options.method);
      record = { id: 42, user: { login: 'sachiniyer' }, body: options.body };
      return record;
    }
    if (route.includes('/issues/3932/comments')) return record ? [record] : [];
    if (route.includes('/pulls?')) return pulls;
    return route.includes('/issues/2/comments') ? artifacts : [];
  };
  await sweep(api, 'owner/repo', t(5));
  assert.equal(readRecord(record).episodes[0].end, null);
  artifacts = [...artifacts, verdict(6), verdict(7)];
  await sweep(api, 'owner/repo', t(8));
  const closed = readRecord(record).episodes;
  assert.equal(closed[0].end, t(6));
  assert.deepEqual(closed[0].merged, [2]);
  pulls = [];
  await sweep(api, 'owner/repo', t(12));
  assert.deepEqual(readRecord(record).episodes, closed);
  assert.deepEqual(methods, ['POST', 'PATCH', 'PATCH']);
});
