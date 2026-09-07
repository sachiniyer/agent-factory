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
const environmentMissing = 'To use Codex here, [create an environment for this repo](https://chatgpt.com/codex/cloud/settings/environments).';
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
test('usage-limit predicate recognizes all three captured variants', () => {
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

test('transient failure starts an outage through the shared artifact predicate', () => {
  const failure = comment(2, 'Codex Review: Something went wrong. Try again later by commenting "@codex review". Unknown error');
  const pulls = [{ number: 3951, head: { sha: head }, merged_at: t(3), artifacts: [failure, verdict(4)] }];
  assert.deepEqual(aggregate(pulls, t(5)).map(e => [e.start, e.end, e.merged]), [[t(2), t(4), [3951]]]);
});

test('outage history records failure and quota causes without a false diagnosis', () => {
  const failure = comment(2, 'Codex Review: Something went wrong. Unknown error');
  const episodes = aggregate([{ number: 3953, head: { sha: head }, artifacts: [failure] }], t(5));
  assert.deepEqual(episodes[0].causes, ['failure']);
  assert.match(render(episodes, t(5)), /transient failure/);
  assert.doesNotMatch(render(episodes, t(5)), /limit notice|usage.limit/);
  const mixed = aggregate([{ number: 3953, head: { sha: head }, artifacts: [failure, comment(3, limits[0])] }], t(5));
  assert.deepEqual(mixed[0].causes, ['failure', 'usage-limit']);
  assert.equal(mixed[0].latest.kind, 'usage-limit');
  assert.match(render(mixed, t(5)), /transient failure, usage limit/);
});

test('an unrecognised response extends an outage but never opens an episode alone', () => {
  const unknown = comment(3, environmentMissing);
  const alone = [{ number: 3985, head: { sha: head }, merged_at: t(4), artifacts: [unknown] }];
  assert.deepEqual(aggregate(alone, t(5)), [], 'weak evidence must not open an outage or count a merge');

  const active = aggregate([{
    number: 3985,
    head: { sha: head },
    merged_at: t(4),
    artifacts: [comment(2, limits[0]), unknown],
  }], t(5));
  assert.equal(active.length, 1);
  assert.equal(active[0].start, t(2));
  assert.deepEqual(active[0].causes, ['usage-limit', 'unrecognised']);
  assert.deepEqual(active[0].merged, [3985]);
  assert.deepEqual(active[0].latest, {
    time: t(3), url: unknown.html_url, body: environmentMissing, kind: 'unrecognised',
  });
  assert.match(render(active, t(5)), /usage limit, unrecognised response/);

  const afterRecovery = aggregate([{
    number: 3985,
    head: { sha: head },
    merged_at: t(5),
    artifacts: [comment(2, limits[0]), verdict(3), comment(4, environmentMissing)],
  }], t(6));
  assert.deepEqual(afterRecovery.map(e => [e.start, e.end, e.merged]), [[t(2), t(3), []]]);
});

test('summary status rows never add outage evidence or replace the latest notice', () => {
  const running = summaryArtifact([summaryRow(3, { status: 'Running' })]);
  const episodes = aggregate([{ number: 1, head: { sha: head }, artifacts: [comment(2, limits[0]), running] }], t(5));
  assert.deepEqual(episodes[0].causes, ['usage-limit']);
  assert.equal(episodes[0].latest.body, limits[0]);
});

test('outage records escape comment delimiters and round-trip their state', () => {
  const body = 'first --> second\n<!-- summary -->';
  const episodes = [{ start: t(2), end: null, merged: [], causes: ['unrecognised'],
    latest: { time: t(3), url: 'https://example.com/3', body, kind: 'unrecognised' } }];
  const rendered = render(episodes, t(4));
  assert.equal(rendered.slice(0, rendered.lastIndexOf(' -->')).includes('-->'), false);
  assert.deepEqual(readRecord({ body: rendered, user: { login: 'sachiniyer' } }).episodes, episodes);
});

test('pre-episode unrecognised evidence does not degrade a merge, but in-episode evidence does', () => {
  const before = comment(1, environmentMissing);
  const opener = comment(2, limits[0]);
  const inEpisode = comment(2, environmentMissing, { created_at: '2026-09-05T02:30:00.000Z' });
  const base = [
    { number: 1, head: { sha: head }, merged_at: t(3), artifacts: [before] },
    { number: 2, head: { sha: head }, merged_at: null, artifacts: [opener] },
  ];
  assert.deepEqual(aggregate(base, t(4))[0].merged, []);
  assert.deepEqual(aggregate([
    { ...base[0], artifacts: [before, inEpisode] }, base[1],
  ], t(4))[0].merged, [1]);
});

function summaryRow(hour, { status = 'Completed', commit = 'bbbbbbb' } = {}) {
  const timestamp = hour == null ? '' : `<relative-time datetime="${t(hour)}"></relative-time>`;
  return `| Code Review | ${status} ${timestamp} | \`${commit}\` | New commits |`;
}
function summaryArtifact(rows, extra = {}) {
  return comment(9, `<!-- codex-pull-request-review-summary -->\n## Codex Review Summary\n${rows.join('\n')}`, extra);
}
test('older-head completed summary rows recover outages at every row time', () => {
  const failure = comment(2, 'Codex Review: Something went wrong. Unknown error');
  const collect = artifacts => aggregate([{ number: 3953, head: { sha: head }, artifacts }], t(8));
  const summary = summaryArtifact([summaryRow(4)]);
  const episodes = collect([failure, summary]);
  assert.equal(episodes[0].end, t(4));
  assert.equal(episodes[0].recovery, summary.html_url);
  // Every row is an event: neither the first row nor the current head is special.
  const multiple = collect([failure, comment(5, limits[0]), summaryArtifact([summaryRow(7), summaryRow(4)])]);
  assert.deepEqual(multiple.map(e => [e.start, e.end]), [[t(2), t(4)], [t(5), t(7)]]);
  for (const invalid of [
    summaryArtifact([summaryRow(4, { status: 'Running' })]),
    summaryArtifact([summaryRow(null)]),
    summaryArtifact([summaryRow(4, { commit: '' })]),
    summaryArtifact([summaryRow(4)], { user: { login: 'someone' } }),
    comment(9, `Quoted summary:\n${summary.body}`),
  ]) assert.equal(collect([failure, invalid])[0].end, null);
});

test('record-backed notices use the episode causes for the entire adopted span', async () => {
  for (const [causes, kind, expected] of [
    [['failure', 'usage-limit'], 'usage-limit', 'unavailable since'],
    [['usage-limit', 'failure'], 'failure', 'unavailable since'],
    [['failure'], 'usage-limit', 'unavailable since'],
    [['usage-limit'], 'failure', 'unavailable since'],
    [['failure'], 'failure', 'unavailable after a transient failure since'],
    [['usage-limit'], 'usage-limit', 'usage-limited since'],
    [['usage-limit', 'unrecognised'], 'unrecognised', 'unavailable since'],
    [undefined, 'usage-limit', 'unavailable since'],
  ]) {
    const episodes = [{ start: t(2), end: null, causes, merged: [], latest: { time: t(3), body: '', url: 'notice' } }];
    const github = { rest: { issues: { listComments() {} } }, paginate: async () => [
      { user: { login: 'sachiniyer' }, body: render(episodes, t(4)), html_url: 'record' },
    ] };
    const notice = await gateNotice({ github, context: { repo: {} }, since: t(3), now: t(4), kind });
    assert.ok(notice.startsWith(`Codex ${expected} ${t(2)}, 2.0h ago`), notice);
    if (expected === 'unavailable since' && causes) {
      const labels = causes.map(c => ({
        failure: 'transient failure', 'usage-limit': 'usage limit', unrecognised: 'unrecognised response',
      })[c]).join(', then ');
      assert.ok(notice.includes(`(${labels})`), notice);
    }
  }
});

test('missing and unreadable records retain local cause and local duration', async () => {
  for (const kind of ['failure', 'usage-limit', 'unrecognised']) {
    for (const paginate of [async () => [], async () => { throw Error('offline'); }]) {
      const github = { rest: { issues: { listComments() {} } }, paginate };
      const notice = await gateNotice({ github, context: { repo: {} }, since: t(3), now: t(4), kind });
      const label = ({
        failure: 'unavailable after a transient failure',
        'usage-limit': 'usage-limited',
        unrecognised: 'unavailable after an unrecognised response',
      })[kind];
      assert.ok(notice.startsWith(`Codex ${label} since ${t(3)}, 1.0h ago`), notice);
    }
  }
});
