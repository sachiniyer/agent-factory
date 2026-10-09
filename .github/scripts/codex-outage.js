// Master Health Watch owns this one comment on the policy issue. The gate only
// reads it; availability here never substitutes for its per-head evidence.
const POLICY_ISSUE = 3932;
const MARKER = '<!-- codex-reviewer-outage:v1 ';
// Bootstrap includes the evidence window on #3932. Later sweeps retain closed
// episodes only after their recovery leaves the 24h recompute window. Episodes
// after the last frozen recovery are rebuilt, so classifier fixes can discover
// earlier evidence without letting the rolling scan duplicate older outages.
const SCAN_SINCE = '2026-09-05T00:00:00.000Z';
const RECOMPUTE_WINDOW_MS = 24 * 60 * 60 * 1000;
const time = (value) => Date.parse(value || '');
const causeLabel = kind => ({
  failure: 'transient failure',
  'usage-limit': 'usage limit',
  unrecognised: 'unrecognised response',
})[kind] || 'unknown cause';
const hours = (start, end) => (Math.max(0, time(end) - time(start)) / 3600000).toFixed(1);

function aggregate(pulls, now = new Date().toISOString(), since = SCAN_SINCE, frozen = []) {
  // Lazy import lets the gate read/render the record without a module cycle.
  const evidence = require('./auto-gate.js').codexEvidence;
  const events = [];
  for (const pull of pulls) {
    for (const artifact of pull.artifacts) {
      if (artifact.user?.login !== evidence.CODEX_REVIEWER) continue;
      const body = artifact.body || '';
      const verdict = evidence.parseReviewedCommit(body);
      const unavailable = evidence.classifyCodexUnavailableArtifact(artifact);
      // A completion claim needs a real artifact, including for superseded commits.
      const responses = evidence.corroboratedCodexSummaryRows(artifact, pull.artifacts,
        (commit, at) => summaryCommitSince(pull, commit, at)).map(row => ({
        time: new Date(row.time).toISOString(), verdict: true,
      }));
      if (verdict || unavailable) responses.push({
        time: (verdict && artifact.updated_at) || artifact.submitted_at || artifact.created_at,
        verdict, kind: unavailable?.kind,
      });
      const eventBody = unavailable?.kind === 'unrecognised' ? unavailable.cause : body;
      for (const response of responses) {
        const at = time(response.time);
        if (!Number.isFinite(at) || at > time(now) || at < time(since)) continue;
        events.push({ ...response, url: artifact.html_url, body: eventBody });
      }
    }
  }
  // Verdict wins a timestamp tie. A real verdict is recovery even if it reports
  // findings; recovery means review capacity returned, not that the code is clean.
  events.sort((a, b) =>
    time(a.time) - time(b.time) ||
    Number(!!a.verdict) - Number(!!b.verdict) ||
    Number(a.kind === 'unrecognised') - Number(b.kind === 'unrecognised'));
  // Keep frozen episode boundaries, but include them in merge attribution.
  const episodes = frozen.map(episode => ({ ...episode, merged: [...episode.merged] }));
  let active;
  for (const event of events) {
    if (event.verdict) {
      if (active) {
        active.end = event.time;
        active.recovery = event.url;
        active = null;
      }
    } else {
      // An unrecognised Codex response strongly says that no review happened,
      // but by itself is weak evidence of a repository-wide outage. It extends
      // an episode opened by a known limit/failure and never opens one alone.
      if (!active && event.kind === 'unrecognised') continue;
      if (!active) {
        active = { start: event.time, end: null, latest: null, merged: [], causes: [] };
        episodes.push(active);
      }
      if (!active.causes.includes(event.kind)) active.causes.push(event.kind);
      active.latest = { time: event.time, url: event.url, body: event.body, kind: event.kind };
    }
  }
  for (const pull of pulls) {
    const merged = time(pull.merged_at);
    if (!Number.isFinite(merged) || merged > time(now)) continue;
    // Replace this scanned PR's prior attribution, including in frozen history.
    // Unscanned PRs retain their recorded counts.
    for (const episode of episodes) episode.merged = episode.merged.filter(n => n !== pull.number);
    const artifacts = pull.artifacts.filter(a => a.user?.login === evidence.CODEX_REVIEWER);
    // Attribute once, to the episode holding the latest qualifying notice.
    // Recovery can precede the merge; notices after the merge cannot move it.
    let attributed;
    let latest = -Infinity;
    for (const episode of episodes) {
      const episodeEnd = time(episode.end || now);
      for (const a of artifacts) {
        const at = time(a.created_at || a.submitted_at || a.updated_at);
        if (evidence.isCodexUsageLimitArtifact(a) && at >= time(episode.start) && at <= episodeEnd && at <= merged && at >= latest) {
          attributed = episode;
          latest = at;
        }
      }
    }
    // #3932's reconstruction: an observed limit before merge and no verdict
    // covering the merged head at that time. Late reviews cannot erase a merge.
    // "Covering" admits the same head names the gate accepted at merge time
    // (#4238): the sweep stores updateBranchContentHead's proof on
    // pull.contentHead, and evidenceHeadShasFor keeps this check and the gate's
    // on one equivalence (#4241) — a second copy would drift again.
    const covering = evidence.evidenceHeadShasFor(pull.head.sha, pull.contentHead);
    if (attributed && !artifacts.some(a => covering.some(sha => {
      const verdict = evidence.parseVerdictArtifact(a, sha, artifacts, summaryCommitSince(pull, sha, merged));
      return verdict && verdict.time <= merged;
    }))) attributed.merged.push(pull.number);
  }
  for (const episode of episodes) {
    episode.merged = [...new Set(episode.merged)].sort((a, b) => a - b);
  }
  return episodes;
}

// Reconstruct the anchor as of the row/merge, so a later rewind cannot erase
// a genuine historical recovery. Unknown dates from the live sweep abort it.
function summaryCommitSince(pull, commit, at) {
  const dates = Object.entries(pull.commitDates || {}).filter(([sha]) => sha.startsWith(commit));
  const pushes = (pull.headForcePushes || []).filter(push =>
    push.afterCommit?.oid?.startsWith(commit) && time(push.createdAt) <= at);
  const markers = pull.artifacts.filter(a => a.user?.login === 'sachiniyer' &&
    time(a.created_at) <= at &&
    new RegExp(`^Head SHA: ${commit}[0-9a-f]*\\s*$`, 'm').test(a.body || ''));
  return Math.max(time(pull.created_at) || 0, ...dates.map(([, date]) => time(date) || 0),
    ...pushes.map(push => time(push.createdAt)), ...markers.map(a => time(a.created_at) || 0));
}

async function readForcePushes(api, repo, number) {
  const [owner, name] = repo.split('/');
  const pushes = [];
  let cursor;
  do {
    const response = await api('graphql', { query: `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
      repository(owner: $owner, name: $name) { pullRequest(number: $number) {
        timelineItems(first: 100, after: $cursor, itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT]) {
          pageInfo { hasNextPage endCursor }
          nodes { ... on HeadRefForcePushedEvent { createdAt afterCommit { oid } } }
        }
      } }
    }`, variables: { owner, name, number, ...(cursor ? { cursor } : {}) } });
    const timeline = response.data?.repository?.pullRequest?.timelineItems;
    if (!timeline || response.errors) throw new Error(`Unreadable force-push history: PR #${number}`);
    pushes.push(...timeline.nodes);
    cursor = timeline.pageInfo.hasNextPage ? timeline.pageInfo.endCursor : null;
    if (timeline.pageInfo.hasNextPage && !cursor) throw new Error('Missing force-push cursor');
  } while (cursor);
  if (pushes.some(push => !Number.isFinite(time(push.createdAt)))) throw new Error('Unreadable force-push date');
  return pushes;
}

// The content-head proof the gate runs at merge time (#3803, #4238), resolved
// for the head a merged PR carried. The sweep's `api` is a `gh api` route
// helper, so an octokit-shaped adapter answers the three reads the proof makes
// — commit, compare, recursive tree. A read that cannot be answered throws out
// of retryRead and aborts the sweep rather than silently narrowing the
// covering-head set to the merged head alone (#4241).
async function readUpdateBranchContentHead(api, root, pull) {
  const headSha = pull.head?.sha;
  const baseRefName = pull.base?.ref;
  if (!headSha || !baseRefName) return null;
  const head = await api(`${root}/commits/${headSha}`, { singlePage: true });
  if (!Array.isArray(head?.parents)) throw new Error(`Unreadable head commit: ${headSha}`);
  const single = async (route) => ({ data: await api(`${root}/${route}`, { singlePage: true }) });
  const github = {
    rest: {
      repos: {
        getCommit: ({ ref }) => single(`commits/${ref}`),
        compareCommitsWithBasehead: ({ basehead, per_page }) =>
          single(`compare/${basehead}?per_page=${per_page || 100}`),
      },
      git: {
        getTree: ({ tree_sha, recursive }) =>
          single(`git/trees/${tree_sha}${recursive ? '?recursive=1' : ''}`),
      },
    },
  };
  const [owner, name] = root.split('/');
  return require('./auto-gate.js').codexEvidence.updateBranchContentHead({
    github,
    context: { repo: { owner, repo: name } },
    baseRefName,
    headSha,
    headParents: head.parents.map(parent => ({ oid: parent.sha })),
  });
}

// An incomplete sweep carries the last complete episodes verbatim and says so:
// `observedAt` stays at the previous complete scan so gateNotice's "as of"
// keeps describing the data's real age, and the record marks itself partial
// rather than posing as a clean read (#4629).
function render(episodes, now, incomplete = null) {
  const active = episodes.at(-1);
  const heading = incomplete
    ? 'Codex reviewer availability — unknown (sweep incomplete)'
    : active && !active.end
      ? `Codex reviewer unavailable since ${active.start}`
      : 'Codex reviewer availability — recovered';
  const lines = [`## ${heading}`, '', 'Owned by Master Health Watch. Policy and evidence: #3932.',
    `Last sweep: ${now}. History scanned since ${SCAN_SINCE}.`,
    'Degraded merges are reconstructed from pre-merge reviewer-unavailable notices and absence of a verdict covering the merged head — where coverage admits the content head the gate\'s update-branch proof verifies, the same head set the gate accepts (the #3932 method, #4238\'s equivalence).', ''];
  if (incomplete) {
    lines.push(`**This sweep was incomplete** — ${escapeCommentDelimiters(incomplete.reason)}. ` +
      `The episode history below is the last complete record${incomplete.observedAt ? `, observed ${incomplete.observedAt}` : ''}; nothing in it reflects this run.`, '');
  }
  // An open episode's elapsed time stops at the last complete observation —
  // a recovery may have happened in the unobserved gap, so `now` would claim
  // an unbroken outage the failed sweep never saw.
  const horizon = incomplete?.observedAt || now;
  for (const episode of [...episodes].reverse()) {
    lines.push(`### Unavailable since ${episode.start}`, `${hours(episode.start, episode.end || horizon)}h elapsed.`,
      `Observed causes: ${(episode.causes || []).map(causeLabel).join(', ') || 'not recorded'}.`,
      `Latest reviewer-unavailable notice: [${episode.latest.time}](${episode.latest.url})`,
      `> ${escapeCommentDelimiters(episode.latest.body).replace(/\n/g, '\n> ')}`,
      `Degraded merges: ${episode.merged.length}${episode.merged.length ? ` (${episode.merged.map(n => `#${n}`).join(', ')})` : ''}.`,
      episode.end ? `Recovered: ${episode.end} — [first real verdict](${episode.recovery}). Final degraded-merge count: ${episode.merged.length}.`
        : incomplete ? `Status: unavailable as of ${horizon} (last complete observation).`
          : 'Status: unavailable.', '');
  }
  const json = JSON.stringify({
    episodes,
    observedAt: incomplete?.observedAt || now,
    ...(incomplete ? { incomplete: { at: incomplete.at, reason: incomplete.reason } } : {}),
  }).replace(/--/g, '-\\u002d');
  lines.push(`${MARKER}${json} -->`);
  return lines.join('\n');
}

function escapeCommentDelimiters(value) {
  return String(value || '').replace(/--/g, '-\\u002d');
}

function readRecord(comment) {
  if (comment.user?.login !== 'sachiniyer') return null;
  const body = comment.body || '';
  const index = body.lastIndexOf(MARKER);
  if (index < 0) return null;
  try {
    const state = JSON.parse(body.slice(index + MARKER.length, body.indexOf(' -->', index)));
    return Array.isArray(state.episodes) ? state : null;
  } catch { return null; }
}

async function gateNotice({ github, context, since, kind = "usage-limit", now = new Date().toISOString() }) {
  let suffix = ' (repository record not yet updated; duration observed on this PR)';
  let start = since;
  let description = kind === 'failure'
    ? 'unavailable after a transient failure'
    : kind === 'unrecognised'
      ? 'unavailable after an unrecognised response'
      : 'usage-limited';
  let observedCauses = '';
  try {
    const comments = await github.paginate(github.rest.issues.listComments, {
      ...context.repo, issue_number: POLICY_ISSUE, per_page: 100,
    });
    const record = comments.map(comment => ({ comment, state: readRecord(comment) })).find(r => r.state);
    const state = record?.state;
    // An incomplete record's episodes are the last complete scan, verbatim;
    // adopting a stale active episode would claim an outage across a window
    // the failed sweep never observed (#4629). The PR's own evidence keeps
    // the duration local instead.
    const active = state && !state.incomplete ? state.episodes.at(-1) : null;
    if (state?.incomplete) {
      suffix = ` (Master Health Watch sweep incomplete at ${state.incomplete.at || state.observedAt}; duration observed on this PR)`;
    }
    if (active && !active.end && time(active.start) <= time(since)) {
      start = active.start;
      const causes = active.causes || [];
      if (causes.length !== 1 || causes[0] !== kind) {
        description = 'unavailable';
        observedCauses = ` (${causes.map(causeLabel).join(', then ') || 'causes not recorded'})`;
      }
      suffix = ` (Master Health Watch as of ${record.state.observedAt}; ${record.comment.html_url})`;
    }
  } catch {
    suffix = ' (repository record unavailable; duration observed on this PR)';
  }
  return `Codex ${description} since ${start}, ${hours(start, now)}h ago${observedCauses}${suffix}`;
}

// api paginates GET collections; the caller wraps `gh api`, so this scheduled
// task adds no dependency or gate polling.
// A scan read that outlives api's retries must not pass for a clean sweep —
// and must not exit without writing (#4629): the run stops scanning (a dead
// window outlasted its backoff), preserves the last complete episodes, and
// the record written below marks itself incomplete. Only the two irreducible
// calls stay fatal: the record read (an unreadable record cannot be PATCHed,
// and a blind POST could duplicate it) and the write itself.
async function sweep(api, repo, now = new Date().toISOString()) {
  const root = `repos/${repo}`;
  const comments = await api(`${root}/issues/${POLICY_ISSUE}/comments?per_page=100`);
  const records = comments.filter(c => readRecord(c));
  if (records.length > 1) throw new Error('Multiple outage records; reconcile before updating');
  const previousState = records.length ? readRecord(records[0]) : null;
  const previous = previousState?.episodes || [];
  const recomputeCutoff = time(now) - RECOMPUTE_WINDOW_MS;
  const isFrozen = episode => episode.end && time(episode.end) < recomputeCutoff;
  const frozen = previous.filter(isFrozen);
  const lastFrozen = frozen.at(-1);
  const since = lastFrozen?.end
    ? new Date(time(lastFrozen.end) + 1).toISOString()
    : SCAN_SINCE;
  const pulls = [];
  let scanError;
  try {
    // Updated ordering includes old PRs receiving late reviews. Stop only after
    // the active outage/last recovery; never cap a search at GitHub's 1000-item limit.
    for (let page = 1; ; page++) {
      const batch = await api(`${root}/pulls?state=all&sort=updated&direction=desc&per_page=100&page=${page}`, { singlePage: true });
      for (const pull of batch) {
        if (time(pull.updated_at) < time(since)) continue;
        const artifacts = [];
        for (const endpoint of [`pulls/${pull.number}/comments`, `issues/${pull.number}/comments`, `pulls/${pull.number}/reviews`]) {
          artifacts.push(...await api(`${root}/${endpoint}?per_page=100`));
        }
        const evidence = require('./auto-gate.js').codexEvidence;
        const rows = artifacts.flatMap(a => evidence.completedCodexSummaryRows(a));
        const commitDates = {};
        const commits = new Set(artifacts.filter(a => a.user?.login === evidence.CODEX_REVIEWER)
          .map(a => a.commit_id || evidence.parseReviewedCommit(a.body || ""))
          .filter(sha => /^[0-9a-f]{7,40}$/i.test(sha || "") &&
            rows.some(row => sha.startsWith(row.commit) || row.commit.startsWith(sha))));
        for (const sha of commits) {
          const commit = await api(`${root}/commits/${sha}`, { singlePage: true });
          commitDates[commit.sha || sha] = commit.commit?.committer?.date;
          if (!Number.isFinite(time(commitDates[commit.sha || sha]))) throw new Error(`Unreadable commit date: ${sha}`);
        }
        const headForcePushes = commits.size ? await readForcePushes(api, repo, pull.number) : [];
        // Only a merged head can be counted, and only a proven update-branch
        // merge has a content head — the same proof the gate ran (#4238), so a
        // verdict bound to it covers the merge here exactly as it did there.
        const contentHead = Number.isFinite(time(pull.merged_at))
          ? await readUpdateBranchContentHead(api, root, pull)
          : null;
        pulls.push({ ...pull, artifacts, commitDates, headForcePushes, contentHead });
      }
      if (batch.length < 100 || time(batch.at(-1).updated_at) < time(since)) break;
    }
  } catch (error) {
    scanError = error;
  }
  const episodes = scanError ? previous : aggregate(pulls, now, since, frozen);
  if (!scanError && !episodes.length && !records.length) return null;
  const body = render(episodes, now, scanError && {
    at: now,
    reason: String(scanError.message || scanError).slice(0, 300),
    observedAt: previousState?.observedAt,
  });
  if (records.length) {
    return api(`${root}/issues/comments/${records[0].id}`, { method: 'PATCH', body });
  }
  return api(`${root}/issues/${POLICY_ISSUE}/comments`, { method: 'POST', body });
}

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

// Retries at the `gh api` boundary, where the failure classes that killed
// #4629's runs actually surface: a small bounded backoff, since the sweep is
// hourly and a dead window that outlasts it is degraded by sweep() anyway.
const GH_API_RETRY_DELAYS_MS = [2000, 8000, 30000];
// GitHub's rate-limit guidance: honor the returned retry timing — which `gh`
// does not surface on stderr — and otherwise wait at least a minute between
// retries; continuing at transport pace inside a secondary-limit window only
// extends the throttle.
const GH_API_RATE_LIMIT_MIN_DELAY_MS = 60000;

function ghFailureText(error) {
  return [error?.stderr, error?.message].filter(Boolean).join('\n');
}

// gh answers HTTP rejections as "gh: <message> (HTTP <status>)" on stderr; a
// transport failure (dial timeout, ECONNRESET, EOF, DNS) carries no status.
function ghHttpStatus(error) {
  const match = /\bHTTP (\d{3})\b/i.exec(ghFailureText(error));
  return match ? Number(match[1]) : null;
}

function isGhRateLimit(error) {
  const status = ghHttpStatus(error);
  if (status === 429) return true;
  // GraphQL secondary limits can answer HTTP 200 with an errors payload — gh
  // exits nonzero with the error text and no `HTTP nnn` suffix, so the
  // message must classify with no status too; a bare transport failure never
  // carries it.
  if (status === null || status === 403) {
    return /rate limit|secondary|abuse detection|retry after/i.test(ghFailureText(error));
  }
  return false;
}

function isRetryableGhFailure(error, method) {
  // The record-create POST is the one non-idempotent call. Only a rate-limit
  // refusal is a definitive "nothing was created" worth replaying — the gate's
  // isDefinitiveRateLimitResponse convention: even a 5xx answer can report a
  // committed create, and a duplicate record fails every later sweep on
  // "Multiple outage records". Any other POST failure exits; the next hourly
  // run creates the record.
  if (method === 'POST') return isGhRateLimit(error);
  const status = ghHttpStatus(error);
  // No HTTP answer: a transport error, or a body that reached us unreadable
  // (empty or cut mid-payload — the SyntaxError JSON.parse throws). Safe to
  // replay for reads and the idempotent PATCH.
  if (status === null) return true;
  // Any other answered 4xx is a verdict, not a flap.
  return status >= 500 || status === 408 || isGhRateLimit(error);
}

// The sweep's gh-backed route helper. execFileSync and sleep are injectable so
// the retry policy is pinned by tests without a live gh. One transient failure
// inside the multi-hundred-call scan must not kill the run; a failure that
// outlives the bounded backoff rethrows for sweep() to degrade on.
function createGhApi({ execFileSync, sleep = delay, delays = GH_API_RETRY_DELAYS_MS, log = console.error }) {
  const run = (route, options) => {
    if (options.query) {
      const args = ['api', 'graphql', '-f', `query=${options.query}`];
      for (const [key, value] of Object.entries(options.variables)) args.push('-F', `${key}=${value}`);
      return JSON.parse(execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 }));
    }
    const args = ['api', route];
    if (options.method) args.push('--method', options.method, '--input', '-');
    // `@json` prints each page as one JSON-array line, so a completed read
    // emits at least `[]` — empty stdout can only be a body that never
    // reached us, and a non-array page an error document. `.[] | @json`
    // printed nothing for an empty collection, which read identically to a
    // dropped response and could have blinded the record read into a
    // duplicate POST (#4629 review).
    else if (!options.singlePage) args.push('--paginate', '--jq', '@json');
    const output = execFileSync('gh', args, {
      encoding: 'utf8', maxBuffer: 64 * 1024 * 1024,
      input: options.method ? JSON.stringify({ body: options.body }) : undefined,
    });
    if (!options.method && !options.singlePage) {
      const text = output.trim();
      if (!text) throw new SyntaxError(`gh api --paginate produced no output for ${route}`);
      const pages = text.split('\n').map(line => JSON.parse(line));
      if (pages.some(page => !Array.isArray(page))) {
        throw new Error(`gh api --paginate returned a non-collection page for ${route}`);
      }
      return pages.flat();
    }
    return JSON.parse(output);
  };
  return async (route, options = {}) => {
    if (options.method && process.argv.includes('--dry-run')) {
      console.log(options.body);
      return null;
    }
    for (let attempt = 0; ; attempt++) {
      try {
        return run(route, options);
      } catch (error) {
        if (!isRetryableGhFailure(error, options.method) || attempt >= delays.length) throw error;
        const waitMs = isGhRateLimit(error)
          ? Math.max(delays[attempt], GH_API_RATE_LIMIT_MIN_DELAY_MS)
          : delays[attempt];
        const detail = String(error?.stderr || error?.message || error).trim().split('\n').pop().slice(0, 200);
        log(`codex-outage: gh api ${options.query ? 'graphql' : route} failed (${detail}); retrying in ${waitMs}ms`);
        await sleep(waitMs);
      }
    }
  };
}

if (require.main === module) {
  const { execFileSync } = require('node:child_process');
  const api = createGhApi({ execFileSync });
  sweep(api, process.argv[2] || 'sachiniyer/agent-factory')
    .then(record => { if (!process.argv.includes('--dry-run')) console.log(record?.html_url || 'No Codex outage observed'); })
    .catch(error => { console.error(error); process.exitCode = 1; });
}
module.exports = { aggregate, render, readRecord, sweep, gateNotice, createGhApi };
