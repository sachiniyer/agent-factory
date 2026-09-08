const assert = require('node:assert/strict');
const test = require('node:test');
const { aggregate, render, readRecord, sweep, gateNotice } = require('./codex-outage.js');
const gate = require('./auto-gate.js');
const t = (hour) => `2026-09-05T${String(hour).padStart(2, '0')}:00:00.000Z`;
const tomorrow = (hour) => `2026-09-06T${String(hour).padStart(2, '0')}:00:00.000Z`;
const head = 'a'.repeat(40);
const limits = [
  'You have reached your Codex usage limits for code ' + 'reviews.',
  'Codex usage limits have been reached for code reviews. Please check with the admins of this repo to increase the limits by adding credits.',
  'You have reached your Codex usage limits.',
];
const environmentMissing = 'To use Codex here, [create an environment for this repo](https://chatgpt.com/codex/cloud/settings/environments).';
const findingBody =
  '**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow?style=flat)</sub></sub>  ' +
  'Keep incomplete summary rows out of outage evidence**';
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
async function runRecordedSweep({ episodes, pulls, artifactsByRoute, now }) {
  let record = { id: 42, user: { login: 'sachiniyer' }, body: render(episodes, now) };
  const calls = [];
  const api = async (route, options = {}) => {
    calls.push(route);
    if (options.method === 'PATCH') {
      record = { ...record, body: options.body };
      return record;
    }
    if (route.includes('/issues/3932/comments')) return [record];
    if (route.includes('/pulls?')) return pulls;
    const match = Object.entries(artifactsByRoute).find(([part]) => route.includes(part));
    return match?.[1] || [];
  };
  await sweep(api, 'owner/repo', now);
  return { calls, episodes: readRecord(record).episodes };
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
test('sweep recomputes a recent completed episode with the current classifier', async () => {
  const opener = comment(2, limits[1]);
  const usage = comment(3, limits[0], { html_url: 'https://example.com/real-usage-limit' });
  const finding = comment(4, findingBody, {
    html_url: 'https://example.com/stale-finding',
    pull_request_review_id: 5128730196,
    commit_id: head,
  });
  const recovery = verdict(5);
  const stale = {
    start: t(2),
    end: t(5),
    latest: { time: t(4), url: finding.html_url, body: finding.body, kind: 'unrecognised' },
    merged: [3984],
    causes: ['usage-limit', 'unrecognised'],
    recovery: recovery.html_url,
  };
  let record = {
    id: 42,
    user: { login: 'sachiniyer' },
    body: render([stale], t(6)),
  };
  const api = async (route, options = {}) => {
    if (options.method === 'PATCH') {
      record = { ...record, body: options.body };
      return record;
    }
    if (route.includes('/issues/3932/comments')) return [record];
    if (route.includes('/pulls?')) return [{
      number: 3984, updated_at: t(9), merged_at: t(4), head: { sha: head },
    }];
    if (route.includes('/pulls/3984/comments')) return [finding];
    if (route.includes('/issues/3984/comments')) return [opener, usage];
    if (route.includes('/pulls/3984/reviews')) return [recovery];
    throw new Error(route);
  };

  await sweep(api, 'owner/repo', t(10));
  const [actual] = readRecord(record).episodes;
  assert.deepEqual(
    { start: actual.start, end: actual.end, merged: actual.merged, recovery: actual.recovery },
    { start: stale.start, end: stale.end, merged: stale.merged, recovery: stale.recovery },
  );
  assert.deepEqual(actual.causes, ['usage-limit']);
  assert.deepEqual(actual.latest, {
    time: t(3), url: usage.html_url, body: usage.body, kind: 'usage-limit',
  });
  assert.deepEqual(
    readRecord({ body: render([actual], t(10)), user: { login: 'sachiniyer' } }).episodes,
    [actual],
  );
});
test('recompute scan includes newly recognised evidence before the stored start', async () => {
  const now = '2026-09-06T00:30:00.001Z';
  const newlyRecognised = comment(1,
    'Codex Review: Something went wrong. Try again later by commenting "@codex review". Unknown error');
  const recentNotice = comment(3, limits[0]);
  const recentRecovery = verdict(5);
  const recent = {
    start: t(3), end: t(5),
    latest: { time: t(3), url: recentNotice.html_url, body: recentNotice.body, kind: 'usage-limit' },
    merged: [], causes: ['usage-limit'], recovery: recentRecovery.html_url,
  };
  const result = await runRecordedSweep({
    episodes: [recent],
    pulls: [
      { number: 2, updated_at: t(6), merged_at: null, head: { sha: head } },
      { number: 1, updated_at: t(1), merged_at: null, head: { sha: head } },
    ],
    artifactsByRoute: {
      '/issues/1/comments': [newlyRecognised],
      '/issues/2/comments': [recentNotice],
      '/pulls/2/reviews': [recentRecovery],
    },
    now,
  });

  assert.equal(result.episodes[0].start, t(1));
  assert.deepEqual(result.episodes[0].causes, ['failure', 'usage-limit']);
  assert.ok(result.calls.some(route => route.includes('/issues/1/comments')));
});
test('recompute scan recovers a missed episode after frozen history', async () => {
  const now = '2026-09-06T00:30:00.001Z';
  const frozenEnd = '2026-09-05T00:30:00.000Z';
  const frozen = {
    start: '2026-09-05T00:10:00.000Z', end: frozenEnd,
    latest: { time: '2026-09-05T00:20:00.000Z', url: 'frozen-notice', body: limits[0], kind: 'usage-limit' },
    merged: [], causes: ['usage-limit'], recovery: 'frozen-recovery',
  };
  const missedNotice = comment(1, limits[1]);
  const missedRecovery = verdict(2);
  const recentNotice = comment(3, limits[0]);
  const recentRecovery = verdict(5);
  const recent = {
    start: t(3), end: t(5),
    latest: { time: t(3), url: recentNotice.html_url, body: recentNotice.body, kind: 'usage-limit' },
    merged: [], causes: ['usage-limit'], recovery: recentRecovery.html_url,
  };
  const result = await runRecordedSweep({
    episodes: [frozen, recent],
    pulls: [
      { number: 3, updated_at: t(6), merged_at: null, head: { sha: head } },
      { number: 2, updated_at: t(2), merged_at: null, head: { sha: head } },
    ],
    artifactsByRoute: {
      '/issues/2/comments': [missedNotice],
      '/pulls/2/reviews': [missedRecovery],
      '/issues/3/comments': [recentNotice],
      '/pulls/3/reviews': [recentRecovery],
    },
    now,
  });

  assert.deepEqual(result.episodes.map(episode => [episode.start, episode.end]), [
    [frozen.start, frozen.end], [t(1), t(2)], [t(3), t(5)],
  ]);
});
test('recompute scan never re-aggregates frozen history', async () => {
  const now = '2026-09-06T00:30:00.001Z';
  const frozenNotice = comment(0, limits[0], {
    created_at: '2026-09-05T00:10:00.000Z', html_url: 'frozen-notice',
  });
  const frozenRecovery = verdict(0);
  frozenRecovery.created_at = '2026-09-05T00:30:00.000Z';
  frozenRecovery.html_url = 'frozen-recovery';
  const frozen = {
    start: frozenNotice.created_at, end: frozenRecovery.created_at,
    latest: {
      time: frozenNotice.created_at, url: frozenNotice.html_url,
      body: frozenNotice.body, kind: 'usage-limit',
    },
    merged: [], causes: ['usage-limit'], recovery: frozenRecovery.html_url,
  };
  const recentNotice = comment(3, limits[0]);
  const recentRecovery = verdict(5);
  const recent = {
    start: t(3), end: t(5),
    latest: { time: t(3), url: recentNotice.html_url, body: recentNotice.body, kind: 'usage-limit' },
    merged: [], causes: ['usage-limit'], recovery: recentRecovery.html_url,
  };
  const result = await runRecordedSweep({
    episodes: [frozen, recent],
    pulls: [
      { number: 2, updated_at: t(6), merged_at: null, head: { sha: head } },
      { number: 1, updated_at: t(6), merged_at: null, head: { sha: head } },
    ],
    artifactsByRoute: {
      '/issues/1/comments': [frozenNotice],
      '/pulls/1/reviews': [frozenRecovery],
      '/issues/2/comments': [recentNotice],
      '/pulls/2/reviews': [recentRecovery],
    },
    now,
  });

  assert.ok(result.calls.some(route => route.includes('/issues/1/comments')));
  assert.equal(result.episodes.filter(episode => episode.start === frozen.start).length, 1);
  assert.deepEqual(result.episodes[0], frozen);
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
test('an expired completed episode is preserved after its artifacts leave scan history', async () => {
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
  await sweep(api, 'owner/repo', tomorrow(7));
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

test('top-level inline findings never add outage evidence or degrade a merge', () => {
  const finding = comment(3, findingBody, {
    id: 3947119505,
    pull_request_review_id: 5128730196,
    commit_id: head,
  });
  const opener = comment(2, limits[0]);
  const episodes = aggregate([
    { number: 3985, head: { sha: head }, merged_at: null, artifacts: [opener] },
    { number: 3987, head: { sha: head }, merged_at: t(4), artifacts: [finding] },
  ], t(5));

  assert.deepEqual(episodes[0].causes, ['usage-limit']);
  assert.deepEqual(episodes[0].latest, {
    time: t(2), url: opener.html_url, body: limits[0], kind: 'usage-limit',
  });
  assert.deepEqual(episodes[0].merged, []);
  assert.equal(gate.codexEvidence.classifyCodexUnavailableArtifact(finding), null);
});

test('finding-shaped pull-review replies keep their body guard', () => {
  const reply = comment(3, findingBody, {
    pull_request_review_id: 5128730196,
    in_reply_to_id: 3947119505,
    commit_id: head,
  });

  assert.equal(gate.codexEvidence.classifyCodexUnavailableArtifact(reply), null);
  assert.deepEqual(
    gate.codexEvidence.classifyCodexUnavailableArtifact({ ...reply, body: limits[0] }),
    { kind: 'usage-limit' },
  );
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
  const proof = comment(3, "", { commit_id: "b".repeat(40), submitted_at: t(3), id: 3606 });
  const episodes = collect([failure, summary, proof]);
  assert.equal(episodes[0].end, t(4));
  assert.equal(episodes[0].recovery, summary.html_url);
  // Every row is an event: neither the first row nor the current head is special.
  const multiple = collect([failure, proof, comment(5, limits[0]), summaryArtifact([summaryRow(7), summaryRow(4)])]);
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

test('#4052: a Completed row without a review artifact cannot close an episode', () => {
  for (const sha of [head, 'b'.repeat(40)]) {
    const summary = summaryArtifact([summaryRow(3, { commit: sha.slice(0, 7) })]);
    const episodes = aggregate([{ number: 4051, head: { sha: head }, merged_at: t(4),
      artifacts: [comment(2, limits[0]), summary] }], t(5));
    assert.equal(episodes[0].end, null);
    assert.deepEqual(episodes[0].merged, [4051]);
  }
});

test('#4052: superseded rows need matching fresh corroboration and cannot backdate a merge', () => {
  const old = 'b'.repeat(40);
  const summary = summaryArtifact([summaryRow(3, { commit: old.slice(0, 7) })]);
  const review = comment(5, '', { id: 3606, commit_id: old, submitted_at: t(5) });
  const pull = { number: 4051, head: { sha: head }, created_at: t(0), merged_at: t(4),
    commitDates: { [old]: t(1) }, artifacts: [comment(2, limits[0]), summary, review] };
  assert.deepEqual(aggregate([pull], t(6)).map(e => [e.end, e.merged]), [[t(5), [4051]]]);
  for (const invalid of [
    { ...review, commit_id: head },
    { ...review, user: { login: 'someone' } },
    { ...review, created_at: t(1), submitted_at: t(1) },
  ]) assert.equal(aggregate([{ ...pull, artifacts: [pull.artifacts[0], summary, invalid] }], t(6))[0].end, null);
});

test('#4052: live sweep reads commit and paginated push anchors before accepting a row', async () => {
  const summary = summaryArtifact([summaryRow(4, { commit: head.slice(0, 7) })]);
  const review = comment(2, '', { id: 3606, commit_id: head, submitted_at: t(2) });
  let unreadable = false;
  const calls = [];
  const writes = [];
  const api = async (route, options = {}) => {
    calls.push([route, options]);
    if (options.method) { writes.push(options.body); return { body: options.body }; }
    if (route.includes('/issues/3932/comments')) return [];
    if (route.includes('/pulls?')) return [{ number: 4051, head: { sha: head }, created_at: t(0), updated_at: t(5), merged_at: t(5) }];
    if (route.includes('/issues/4051/comments')) return [comment(3, limits[0]), summary];
    if (route.includes('/pulls/4051/reviews')) return [review];
    if (route.includes('/pulls/4051/comments')) return [];
    if (route.includes('/commits/')) return { sha: head, commit: { committer: { date: t(1) } } };
    if (route === 'graphql') {
      if (unreadable) throw Error('push history unavailable');
      const first = !options.variables.cursor;
      return { data: { repository: { pullRequest: { timelineItems: {
        pageInfo: { hasNextPage: first, endCursor: first ? 'next' : null },
        nodes: first ? [] : [{ createdAt: t(3), afterCommit: { oid: head } }],
      } } } } };
    }
    throw Error(route);
  };
  await sweep(api, 'owner/repo', t(6));
  const record = readRecord({ body: writes[0], user: { login: 'sachiniyer' } });
  assert.equal(record.episodes[0].end, null, 'the review predates the force-push');
  assert.deepEqual(record.episodes[0].merged, [4051]);
  assert.equal(calls.filter(([route]) => route === 'graphql').length, 2);
  unreadable = true;
  await assert.rejects(sweep(api, 'owner/repo', t(6)), /push history unavailable/);
  assert.equal(writes.length, 1, 'failed reads must preserve the previous record');
});
