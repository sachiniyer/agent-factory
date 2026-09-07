// Master Health Watch owns this one comment on the policy issue. The gate only
// reads it; availability here never substitutes for its per-head evidence.
const POLICY_ISSUE = 3932;
const MARKER = '<!-- codex-reviewer-outage:v1 ';
// Bootstrap includes the evidence window on #3932. Later sweeps retain closed
// episodes only after their recovery leaves the 24h recompute window. Episodes
// still inside it are rebuilt from the oldest start, so classifier fixes can
// heal recent history without letting the rolling scan forget older outages.
const SCAN_SINCE = '2026-09-05T00:00:00.000Z';
const RECOMPUTE_WINDOW_MS = 24 * 60 * 60 * 1000;
const time = (value) => Date.parse(value || '');
const causeLabel = kind => ({
  failure: 'transient failure',
  'usage-limit': 'usage limit',
  unrecognised: 'unrecognised response',
})[kind] || 'unknown cause';
const hours = (start, end) => (Math.max(0, time(end) - time(start)) / 3600000).toFixed(1);

function aggregate(pulls, now = new Date().toISOString(), since = SCAN_SINCE) {
  // Lazy import lets the gate read/render the record without a module cycle.
  const evidence = require('./auto-gate.js').codexEvidence;
  const events = [];
  for (const pull of pulls) {
    for (const artifact of pull.artifacts) {
      if (artifact.user?.login !== evidence.CODEX_REVIEWER) continue;
      const body = artifact.body || '';
      const verdict = evidence.parseReviewedCommit(body);
      const unavailable = evidence.classifyCodexUnavailableArtifact(artifact);
      // Every completed row is a recovery, including superseded commits.
      // Head matching remains necessary only for the merge accounting below.
      const responses = evidence.completedCodexSummaryRows(artifact).map(row => ({
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
  const episodes = [];
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
  for (const episode of episodes) {
    episode.merged = [...new Set(pulls.filter((pull) => {
      const merged = time(pull.merged_at);
      if (!(merged >= time(episode.start) && merged < time(episode.end || now))) return false;
      const artifacts = pull.artifacts.filter(a => a.user?.login === evidence.CODEX_REVIEWER);
      // #3932's reconstruction: an observed limit before merge and no verdict
      // covering the merged head at that time. Late reviews cannot erase a merge.
      const episodeEnd = time(episode.end || now);
      return artifacts.some(a => {
        const at = time(a.created_at || a.submitted_at || a.updated_at);
        return evidence.isCodexUsageLimitArtifact(a) && at >= time(episode.start) && at <= episodeEnd && at <= merged;
      }) &&
        !artifacts.some(a => {
          const verdict = evidence.parseVerdictArtifact(a, pull.head.sha);
          return verdict && verdict.time <= merged;
        });
    }).map(p => p.number))].sort((a, b) => a - b);
  }
  return episodes;
}

function render(episodes, now) {
  const active = episodes.at(-1);
  const heading = active && !active.end
    ? `Codex reviewer unavailable since ${active.start}`
    : 'Codex reviewer availability — recovered';
  const lines = [`## ${heading}`, '', 'Owned by Master Health Watch. Policy and evidence: #3932.',
    `Last sweep: ${now}. History scanned since ${SCAN_SINCE}.`,
    'Degraded merges are reconstructed from pre-merge reviewer-unavailable notices and absence of a verdict covering the merged head (the #3932 method).', ''];
  for (const episode of [...episodes].reverse()) {
    lines.push(`### Unavailable since ${episode.start}`, `${hours(episode.start, episode.end || now)}h elapsed.`,
      `Observed causes: ${(episode.causes || []).map(causeLabel).join(', ') || 'not recorded'}.`,
      `Latest reviewer-unavailable notice: [${episode.latest.time}](${episode.latest.url})`,
      `> ${escapeCommentDelimiters(episode.latest.body).replace(/\n/g, '\n> ')}`,
      `Degraded merges: ${episode.merged.length}${episode.merged.length ? ` (${episode.merged.map(n => `#${n}`).join(', ')})` : ''}.`,
      episode.end ? `Recovered: ${episode.end} — [first real verdict](${episode.recovery}). Final degraded-merge count: ${episode.merged.length}.` : 'Status: unavailable.', '');
  }
  const json = JSON.stringify({ episodes, observedAt: now }).replace(/--/g, '-\\u002d');
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
    const active = record?.state.episodes.at(-1);
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

// api paginates GET collections; failures abort before any record write. The
// caller uses gh, so this scheduled task adds no dependency or gate polling.
async function sweep(api, repo, now = new Date().toISOString()) {
  const root = `repos/${repo}`;
  const comments = await api(`${root}/issues/${POLICY_ISSUE}/comments?per_page=100`);
  const records = comments.filter(c => readRecord(c));
  if (records.length > 1) throw new Error('Multiple outage records; reconcile before updating');
  const previous = records.length ? readRecord(records[0]).episodes : [];
  const last = previous.at(-1);
  const recomputeCutoff = time(now) - RECOMPUTE_WINDOW_MS;
  const isFrozen = episode => episode.end && time(episode.end) < recomputeCutoff;
  const frozen = previous.filter(isFrozen);
  const oldestRecomputed = previous.find(episode => !isFrozen(episode));
  const since = oldestRecomputed?.start ||
    (last?.end ? new Date(time(last.end) + 1).toISOString() : last?.start || SCAN_SINCE);
  const pulls = [];
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
      pulls.push({ ...pull, artifacts });
    }
    if (batch.length < 100 || time(batch.at(-1).updated_at) < time(since)) break;
  }
  const episodes = [...frozen, ...aggregate(pulls, now, since)];
  if (!episodes.length && !records.length) return null;
  const body = render(episodes, now);
  if (records.length) {
    return api(`${root}/issues/comments/${records[0].id}`, { method: 'PATCH', body });
  }
  return api(`${root}/issues/${POLICY_ISSUE}/comments`, { method: 'POST', body });
}

if (require.main === module) {
  const { execFileSync } = require('node:child_process');
  const api = async (route, options = {}) => {
    if (options.method && process.argv.includes('--dry-run')) {
      console.log(options.body);
      return null;
    }
    const args = ['api', route];
    if (options.method) args.push('--method', options.method, '--input', '-');
    else if (!options.singlePage) args.push('--paginate', '--jq', '.[] | @json');
    const output = execFileSync('gh', args, {
      encoding: 'utf8', maxBuffer: 64 * 1024 * 1024,
      input: options.method ? JSON.stringify({ body: options.body }) : undefined,
    });
    return !options.method && !options.singlePage
      ? output.trim().split('\n').filter(Boolean).map(line => JSON.parse(line))
      : JSON.parse(output);
  };
  sweep(api, process.argv[2] || 'sachiniyer/agent-factory')
    .then(record => { if (!process.argv.includes('--dry-run')) console.log(record?.html_url || 'No Codex outage observed'); })
    .catch(error => { console.error(error); process.exitCode = 1; });
}
module.exports = { aggregate, render, readRecord, sweep, gateNotice };
