// A composition halt (#7151). While a "master broken by merge batch" issue is
// open and master still fails the composition check, a green fix-master PR may
// land through the merger, and only once its would-be merge result passes that
// same check. Nothing else lands, and the red-Build fix-master path is unchanged.
//
// These tests run auto-merge.yml's job itself, not a model of it. Every step's
// `if:` and `${{ }}` env is evaluated from the file, every github-script step
// runs its real script against a fake GitHub, and every `run:` step runs its real
// shell. The fake GitHub is backed by a scratch git origin (real git, real squash
// merges), and `go` is a stub, so the composition check really runs, on whatever
// tree the step under test hands it.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync, spawnSync } = require('node:child_process');
const test = require('node:test');

const ROOT = path.join(__dirname, '..', '..');
const YML = fs.readFileSync(path.join(ROOT, '.github', 'workflows', 'auto-merge.yml'), 'utf8');
const merger = require('./auto-merge.js');

// ---- auto-merge.yml's steps ----------------------------------------------------

// The job's steps as {name, id, if, uses, run, env, with, 'continue-on-error'}.
// Only the shapes this file uses: scalars, one level of map, `|` blocks.
function parseSteps(yaml) {
  const lines = yaml.slice(yaml.indexOf('\n    steps:\n') + 1).split('\n').slice(1);
  const steps = [];
  let step = null;
  let map = null;
  let block = null;
  const flush = () => {
    if (block) block.into[block.key] = block.lines.join('\n').trimEnd() + '\n';
    block = null;
  };
  for (const line of lines) {
    if (block && (line.startsWith(block.indent) || !line.trim())) {
      block.lines.push(line.slice(block.indent.length));
      continue;
    }
    flush();
    const item = line.match(/^ {6}- name: (.*)$/);
    if (item) {
      steps.push((step = { name: item[1], env: {}, with: {} }));
      map = null;
      continue;
    }
    const kv = line.match(/^( {8}| {10})([\w-]+):\s*(.*)$/);
    if (!step || !kv) continue;
    const [, indent, key, value] = kv;
    if (indent.length === 8) map = null;
    const into = indent.length === 8 ? step : map;
    if (!into) continue;
    if (value === '|') block = { into, key, indent: indent + '  ', lines: [] };
    else if (value === '') map = into[key] = into[key] || {};
    else into[key] = value;
  }
  flush();
  return steps;
}

// The expressions in this file (==, !=, &&, ||, parentheses, 'strings') are valid
// JavaScript. Anything unset reads as '', as GitHub's null does.
const lenient = (obj) => new Proxy(obj, { get: (t, k) => (k in t ? t[k] : '') });
function evaluate(expr, steps) {
  const fn = new Function('steps', 'secrets', 'vars', 'github', `return (${expr});`);
  return fn(steps, lenient({}), lenient({}), lenient({ token: 'github-token' }));
}
const interpolate = (value, steps) =>
  value.replace(/\$\{\{\s*([\s\S]*?)\s*\}\}/g, (_, expr) => String(evaluate(expr, steps)));

// ---- A scratch origin: real git, a stub go -------------------------------------

// Mimic the runner: no identity, no user or system config, nothing inherited
// from the git process (or af worktree) this test may run under.
const GIT_ENV = Object.fromEntries(Object.entries(process.env).filter(([k]) => !/^(GIT_|EMAIL$)/.test(k)));
Object.assign(GIT_ENV, { GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1', GIT_TERMINAL_PROMPT: '0' });
const git = (cwd, ...args) =>
  execFileSync('git', ['-c', 'user.name=fixture', '-c', 'user.email=fixture@example.com', ...args], {
    cwd, env: GIT_ENV, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
  }).trim();

// The composition check is real; its parts are stand-ins. `go` fails anywhere a
// BROKEN_BUILD file exists, and the file-length lint allows big.txt three lines.
// The lint anchors to its own parent directory exactly as the real one does
// (scripts/lint-file-length.sh: ROOT is dirname/..), so WHICH copy runs decides
// which tree is linted.
const STUB_GO = '#!/bin/sh\ntest ! -e BROKEN_BUILD\n';
const STUB_LINT = '#!/usr/bin/env bash\ncd "$(dirname "${BASH_SOURCE[0]}")/.." && test "$(wc -l < big.txt)" -le 3\n';
const lines = (n) => 'line\n'.repeat(n);
// What actions/checkout persists unless `persist-credentials: false`.
const TOKEN_HEADER = 'AUTHORIZATION: basic FAKE-WRITE-TOKEN';

// master:  base (big.txt 2 lines) -> broken batch (4 lines: the lint fails)
// #11 fix-master, branched from base: adds a note. Its head alone passes the
//     lint, but merged onto master it is still broken.
// #12 fix-master, branched from master: trims big.txt. Its merge passes.
// #13 an ordinary PR, branched from master: adds a feature.
// Tests add more with origin.addPr, always branched from the broken master.
const roots = [];
test.after(() => { for (const dir of roots) fs.rmSync(dir, { recursive: true, force: true }); });

function makeOrigin() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'auto-merge-halt-'));
  roots.push(root);
  const work = path.join(root, 'work');
  const bin = path.join(root, 'bin');
  fs.mkdirSync(bin);
  fs.writeFileSync(path.join(bin, 'go'), STUB_GO, { mode: 0o755 });
  git(root, 'init', '-q', '-b', 'master', work);
  const commit = (files, message) => {
    for (const [file, body] of Object.entries(files)) {
      fs.mkdirSync(path.dirname(path.join(work, file)), { recursive: true });
      fs.writeFileSync(path.join(work, file), body, { mode: file.endsWith('.sh') ? 0o755 : 0o644 });
    }
    git(work, 'add', '-A');
    git(work, 'commit', '-q', '-m', message);
    return git(work, 'rev-parse', 'HEAD');
  };
  commit({ 'big.txt': lines(2), 'scripts/lint-file-length.sh': STUB_LINT }, 'base');
  git(work, 'checkout', '-q', '-b', 'pr11');
  const fix11 = commit({ 'notes.txt': 'claims a fix\n' }, 'a fix that fixes nothing');
  git(work, 'checkout', '-q', 'master');
  const broken = commit({ 'big.txt': lines(4) }, 'the broken batch');
  git(work, 'checkout', '-q', '-b', 'pr12');
  const fix12 = commit({ 'big.txt': lines(2) }, 'the fix');
  git(work, 'checkout', '-q', '-b', 'pr13', 'master');
  const ord13 = commit({ 'feature.txt': 'feature\n' }, 'a feature');
  const bare = path.join(root, 'origin.git');
  git(root, 'clone', '-q', '--bare', work, bare);
  const heads = { 11: fix11, 12: fix12, 13: ord13 };
  for (const [n, sha] of Object.entries(heads)) git(bare, 'update-ref', `refs/pull/${n}/head`, sha);
  let workspaces = 0;
  return {
    root, bin, bare, heads, broken,
    addPr: (number, files, message) => {
      git(work, 'checkout', '-q', '-B', `pr${number}`, broken);
      heads[number] = commit(files, message);
      git(bare, 'fetch', '-q', work, `${heads[number]}:refs/pull/${number}/head`);
    },
    master: () => git(bare, 'rev-parse', 'refs/heads/master'),
    newWorkspace: () => path.join(root, `workspace-${++workspaces}`),
    // Squash-merge `sha` onto master the way GitHub does: the merge's tree, one
    // parent. Throws on a conflict.
    squash: (sha, message) => {
      const tree = git(bare, 'merge-tree', '--write-tree', 'refs/heads/master', sha);
      const commit = git(bare, 'commit-tree', tree, '-p', 'refs/heads/master', '-m', message);
      git(bare, 'update-ref', 'refs/heads/master', commit);
      return commit;
    },
    file: (name) => git(bare, 'show', `refs/heads/master:${name}`),
  };
}

// ---- A fake GitHub backed by that origin ---------------------------------------

const GREEN = ['Build', 'Lint'].map((name) => ({ __typename: 'CheckRun', name, status: 'COMPLETED', conclusion: 'SUCCESS' }));
const FIX = ['fix-master'];

function fakeGithub(origin, openPrs, { buildRed = false, broken = true } = {}) {
  const prs = openPrs.map(([number, labels = []]) => ({
    number, headRefOid: origin.heads[number], headRefName: `branch-${number}`, isCrossRepository: false,
    isDraft: false, mergeable: 'MERGEABLE', labels: { nodes: labels.map((name) => ({ name })) },
    commits: { nodes: [{ commit: { statusCheckRollup: { contexts: { nodes: GREEN } } } }] },
  }));
  const issue = broken ? { number: 900, title: `${merger.BROKEN_TITLE_PREFIX} ${origin.broken.slice(0, 10)}`, state: 'open' } : null;
  // `refuse`: PR numbers whose merge GitHub refuses (a ruleset, say) though
  // selection saw nothing wrong.
  const g = { prs, issue, merged: [], dispatched: [], comments: [], opened: [], refuse: new Set() };
  const open = () => prs.filter((p) => !g.merged.includes(p.number));
  const rateLimit = { cost: 1, remaining: 4999, resetAt: 'later' };
  g.graphql = async (query) => {
    if (query.includes('pullRequests(')) {
      return { rateLimit, repository: { pullRequests: { pageInfo: { hasNextPage: false }, nodes: structuredClone(open()) } } };
    }
    const repository = {};
    for (const [, n] of query.matchAll(/p(\d+): pullRequest/g)) {
      repository[`p${n}`] = query.includes('files(')
        ? { files: { pageInfo: { hasNextPage: false }, nodes: [{ path: 'big.txt' }] } }
        : query.includes('reviewThreads')
          ? { reviewThreads: { pageInfo: { hasNextPage: false }, nodes: [] } }
          : { mergeable: 'MERGEABLE' };
    }
    return { rateLimit, repository };
  };
  g.rest = {
    search: { issuesAndPullRequests: async () => ({ data: { items: issue && issue.state === 'open' ? [issue] : [] } }) },
    issues: {
      createComment: async ({ issue_number, body }) => { g.comments.push({ issue_number, body }); },
      update: async ({ state }) => { issue.state = state; },
      create: async (args) => { g.opened.push(args); },
    },
    repos: {
      getBranch: async ({ branch }) => ({ data: branch === 'master' ? { commit: { sha: origin.master() } } : { protected: true } }),
    },
    actions: {
      listWorkflowRuns: async ({ workflow_id }) => ({ data: { workflow_runs: workflow_id === 'pr.yml' ? [] : [{
        id: 1, status: 'completed', head_sha: origin.master(), html_url: 'run',
        conclusion: buildRed && workflow_id === 'build.yml' ? 'failure' : 'success',
      }] } }),
      listJobsForWorkflowRun: async () => ({ data: { jobs: [] } }),
      createWorkflowDispatch: async ({ workflow_id }) => { g.dispatched.push(workflow_id); },
    },
    pulls: {
      list: async () => ({ data: [] }),
      merge: async ({ pull_number, sha }) => {
        const pr = open().find((p) => p.number === pull_number);
        if (!pr || pr.headRefOid !== sha) throw Object.assign(new Error('Head branch was modified'), { status: 409 });
        if (g.refuse.has(pull_number)) throw Object.assign(new Error('Repository rule violations found'), { status: 405 });
        const squashed = origin.squash(sha, `PR #${pull_number}`);
        g.merged.push(pull_number);
        return { data: { sha: squashed } };
      },
    },
  };
  return g;
}

// ---- Running the job -----------------------------------------------------------

const AsyncFunction = (async () => {}).constructor;
const CONTEXT = { repo: { owner: 'o', repo: 'r' }, eventName: 'schedule', runId: 1, serverUrl: 'https://github.com', payload: {} };

// `after` maps a step id to a hook run once that step finishes, given the job:
// the world moving under the job between two of its steps.
async function runJob(origin, github, { after = {} } = {}) {
  const state = {};
  const steps = new Proxy({}, { get: (_, id) => state[id] || { outcome: '', outputs: lenient({}) } });
  const job = { state, ran: [], warnings: [], notices: [], logs: {}, temps: {}, workspace: null };
  let failed = false;
  for (const step of parseSteps(YML)) {
    const id = step.id || step.name;
    if (failed || (step.if && !evaluate(step.if, steps))) {
      state[id] = { outcome: 'skipped', outputs: lenient({}) };
      continue;
    }
    job.ran.push(id);
    const env = Object.fromEntries(Object.entries(step.env).map(([k, v]) => [k, interpolate(v, steps)]));
    let result;
    if (/^actions\/checkout@/.test(step.uses || '')) {
      // actions/checkout's default: depth 1.
      job.workspace = origin.newWorkspace();
      git(origin.root, 'clone', '-q', '--depth', '1', '--branch', 'master', `file://${origin.bare}`, job.workspace);
      if (step.with['persist-credentials'] !== 'false') {
        // v6 keeps the token in a file outside the repository, included for the
        // repository's gitdir and for every linked worktree's (git-auth-helper.ts).
        const creds = path.join(origin.root, `credentials-${path.basename(job.workspace)}.config`);
        fs.writeFileSync(creds, `[http "https://github.com/"]\n\textraheader = ${TOKEN_HEADER}\n`);
        git(job.workspace, 'config', `includeIf.gitdir:${job.workspace}/.git.path`, creds);
        git(job.workspace, 'config', `includeIf.gitdir:${job.workspace}/.git/worktrees/*.path`, creds);
      }
      result = { outcome: 'success', outputs: {} };
    } else if (/^actions\/setup-go@/.test(step.uses || '')) {
      result = { outcome: 'success', outputs: {} };
    } else if (/^actions\/github-script@/.test(step.uses || '')) {
      result = await runScript(step.with.script, env, github, job);
    } else if (step.run) {
      assert.ok(job.workspace, `"${step.name}" would run outside a checkout`);
      result = runShell(step.run, env, job.workspace, origin);
      job.logs[id] = result.log;
      job.temps[id] = result.runnerTemp;
    } else {
      throw new Error(`no handler for step "${step.name}"`);
    }
    state[id] = { outcome: result.outcome, outputs: lenient(result.outputs) };
    if (result.outcome !== 'success' && step['continue-on-error'] !== 'true') failed = true;
    if (after[id]) await after[id](job);
  }
  return job;
}

function fakeCore(job, outputs) {
  return {
    setOutput: (k, v) => { outputs[k] = String(v); },
    notice: (m) => job.notices.push(m),
    warning: (m) => job.warnings.push(m),
    info: () => {},
    summary: { addHeading() { return this; }, addRaw() { return this; }, addList() { return this; }, async write() {} },
  };
}

async function runScript(script, env, github, job) {
  const outputs = {};
  const core = fakeCore(job, outputs);
  const saved = Object.fromEntries(Object.keys(env).map((k) => [k, process.env[k]]));
  Object.assign(process.env, env);
  try {
    // github-script resolves a relative require against the workspace, which on
    // the runner is this repository's checkout.
    await new AsyncFunction('github', 'core', 'context', 'require', script)(
      github, core, CONTEXT, (p) => require(path.join(ROOT, p)),
    );
  } finally {
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
  return { outcome: 'success', outputs };
}

let shells = 0;
function runShell(script, env, cwd, origin) {
  const output = path.join(origin.root, `github-output-${++shells}`);
  const runnerTemp = fs.mkdtempSync(path.join(origin.root, 'runner-temp-'));
  fs.writeFileSync(output, '');
  // The runner's shell for `run:`: bash --noprofile --norc -eo pipefail.
  const res = spawnSync('bash', ['--noprofile', '--norc', '-eo', 'pipefail', '-c', script], {
    cwd, encoding: 'utf8',
    env: { ...GIT_ENV, ...env, PATH: `${origin.bin}:${process.env.PATH}`, GITHUB_OUTPUT: output, RUNNER_TEMP: runnerTemp },
  });
  const outputs = Object.fromEntries(
    fs.readFileSync(output, 'utf8').split('\n').filter(Boolean).map((l) => [l.slice(0, l.indexOf('=')), l.slice(l.indexOf('=') + 1)]),
  );
  return { outcome: res.status === 0 ? 'success' : 'failure', outputs, log: res.stdout + res.stderr, runnerTemp };
}

// One scenario: a fresh origin, the open PRs, the job run once.
async function halted(openPrs, opts = {}) {
  const origin = makeOrigin();
  const github = fakeGithub(origin, openPrs, opts);
  const job = await runJob(origin, github, opts);
  return { origin, github, job };
}
const outcome = (job, id) => (job.state[id] || { outcome: `no step ${id}` }).outcome;
const output = (job, id, key) => (job.state[id] ? job.state[id].outputs[key] : `no step ${id}`);

// ---- The property --------------------------------------------------------------

test('scenario sanity: master fails the composition check, #11 passes it alone, #12 fixes it', async () => {
  const origin = makeOrigin();
  assert.equal(origin.file('big.txt').split('\n').length, 4);
  assert.equal(git(origin.bare, 'show', `${origin.heads[11]}:big.txt`).split('\n').length, 2);
  assert.equal(git(origin.bare, 'show', `${origin.heads[12]}:big.txt`).split('\n').length, 2);
});

test('a halted run lands a green fix-master PR once its merge onto master passes the composition check', async () => {
  const { origin, github, job } = await halted([[12, FIX]]);
  assert.equal(outcome(job, 'recheck'), 'failure', 'master fails the re-check');
  assert.equal(output(job, 'settle', 'halted'), 'true', 'so the run is halted');
  assert.deepEqual(github.merged, [12]);
  assert.equal(outcome(job, 'fixcheck'), 'success', job.logs.fixcheck);
  assert.equal(origin.file('big.txt').split('\n').length, 2, 'master now carries the fix');
  // GITHUB_TOKEN merges fire no push: the new head is verified like any batch.
  assert.deepEqual(github.dispatched, merger.MASTER_WORKFLOWS);
  assert.equal(outcome(job, 'compose'), 'success', 'the post-merge check passes on the new head');
  assert.deepEqual(github.opened, [], 'nothing reports master broken');

  // The issue closes the existing way: the next run's re-check finds master green.
  assert.equal(github.issue.state, 'open');
  const next = await runJob(origin, github);
  assert.equal(outcome(next, 'recheck'), 'success');
  assert.equal(github.issue.state, 'closed');
  assert.ok(next.ran.includes('merge') && !next.ran.includes('fixcheck'), 'merging resumed normally');
});

test('a halted run does not land a fix-master PR whose merge onto master fails the composition check', async () => {
  const { origin, github, job } = await halted([[11, FIX]]);
  // #11's head alone passes the lint (scenario sanity above); its merge does not.
  assert.equal(outcome(job, 'fixcheck'), 'failure', 'its merge result was checked, and failed');
  assert.ok(!job.ran.includes('merge'), 'the merge step does not run');
  assert.deepEqual(github.merged, []);
  assert.equal(origin.master(), origin.broken, 'master is untouched');
  assert.equal(github.issue.state, 'open');
});

test('a halted run with only ordinary PRs merges nothing and checks nothing', async () => {
  const { origin, github, job } = await halted([[13]]);
  assert.equal(output(job, 'settle', 'halted'), 'true');
  assert.ok(!job.ran.includes('fixcheck'), 'no candidate, so no merge result is built');
  assert.ok(!job.ran.includes('merge'));
  assert.deepEqual(github.merged, []);
  assert.equal(origin.master(), origin.broken);
});

test('a failing fix-master candidate does not block a passing one, and ordinary PRs stay halted', async () => {
  const { github, job } = await halted([[11, FIX], [12, FIX], [13]]);
  assert.deepEqual(github.merged, [12]);
  assert.match(job.logs.fixcheck, /#11 does not pass the composition check/);
});

test('the fix is checked in a scratch tree: the checkout the merge step loads its script from is untouched', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[11, FIX], [12, FIX]]);
  let checkout = null;
  const job = await runJob(origin, github, {
    after: { fixcheck: (j) => { checkout = { head: git(j.workspace, 'rev-parse', 'HEAD'), status: git(j.workspace, 'status', '--porcelain', '-uall') }; } },
  });
  // Both candidates were merged and checked (#11 failed, #12 passed), yet the
  // checkout is still master, clean.
  assert.deepEqual(checkout, { head: origin.broken, status: '' });
  assert.equal(outcome(job, 'fixcheck'), 'success', job.logs.fixcheck);
});

test('a fix is not merged if master moved after it was checked', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX]]);
  // A hand merge lands between the check and the merge step.
  const job = await runJob(origin, github, { after: { fixcheck: () => { origin.squash(origin.heads[13], 'hand merge'); } } });
  assert.equal(outcome(job, 'fixcheck'), 'success');
  assert.deepEqual(github.merged, []);
  assert.ok(job.warnings.some((w) => /master moved/.test(w)), job.warnings.join('\n'));
});

test('a fix is not merged if master moved while the merge step re-read the PRs', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX]]);
  // The merge step re-selects before it lands; a hand merge lands mid-read.
  const moveMasterDuringNextPrRead = () => {
    const read = github.graphql;
    github.graphql = async (query, vars) => {
      if (query.includes('pullRequests(')) {
        github.graphql = read;
        origin.squash(origin.heads[13], 'hand merge');
      }
      return read(query, vars);
    };
  };
  const job = await runJob(origin, github, { after: { fixcheck: moveMasterDuringNextPrRead } });
  assert.equal(outcome(job, 'fixcheck'), 'success');
  assert.deepEqual(github.merged, []);
  assert.ok(job.warnings.some((w) => /master moved/.test(w)), job.warnings.join('\n'));
});

test('a fix is not merged if it was held after it was checked', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX]]);
  const job = await runJob(origin, github, {
    after: { fixcheck: () => { github.prs[0].labels.nodes.push({ name: 'hold' }); } },
  });
  assert.equal(outcome(job, 'fixcheck'), 'success');
  assert.deepEqual(github.merged, []);
  assert.ok(job.warnings.some((w) => /label hold/.test(w)), job.warnings.join('\n'));
});

test('mergeHaltFix itself refuses an unchecked fix and any PR that is not fix-master', async () => {
  assert.equal(typeof merger.mergeHaltFix, 'function');
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX], [13]]);
  const call = async (passed, number) => {
    const outputs = {};
    const core = fakeCore({ warnings: [], notices: [] }, outputs);
    const args = { passed, fixes: `${number}:${origin.heads[number]}`, base: origin.broken };
    await merger.mergeHaltFix({ github, core, context: CONTEXT, ...args });
    return outputs.merged;
  };
  assert.equal(await call(false, 12), 'false', 'no passing check');
  assert.equal(await call(true, 13), 'false', 'not fix-master');
  assert.deepEqual(github.merged, []);
  assert.equal(await call(true, 12), 'true');
  assert.deepEqual(github.merged, [12]);
});

test('a fix is not merged if its head moved after it was checked', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX]]);
  // A push lands after the check: still labelled, still green, never checked.
  const job = await runJob(origin, github, { after: { fixcheck: () => { github.prs[0].headRefOid = origin.heads[13]; } } });
  assert.equal(outcome(job, 'fixcheck'), 'success');
  assert.deepEqual(github.merged, [], 'an unchecked head was merged');
  assert.ok(job.warnings.some((w) => /head moved/.test(w)), job.warnings.join('\n'));
});

test('a checked fix whose merge GitHub refuses does not starve the next one', async () => {
  const origin = makeOrigin();
  origin.addPr(15, { 'big.txt': lines(3) }, 'another fix');
  const github = fakeGithub(origin, [[12, FIX], [15, FIX]]);
  github.refuse.add(12);
  const job = await runJob(origin, github);
  assert.deepEqual(github.merged, [15]);
  assert.ok(job.warnings.some((w) => /#12: merge refused/.test(w)), job.warnings.join('\n'));
});

test("a fix-master PR cannot pass the check by editing the lint script: master's lints the merge tree", async () => {
  const origin = makeOrigin();
  // big.txt stays at 4 lines; the PR's own copy of the lint passes anything.
  origin.addPr(16, { 'scripts/lint-file-length.sh': '#!/usr/bin/env bash\nexit 0\n' }, 'a lint that passes everything');
  const github = fakeGithub(origin, [[16, FIX]]);
  const job = await runJob(origin, github);
  assert.deepEqual(github.merged, []);
  assert.equal(outcome(job, 'fixcheck'), 'failure', job.logs.fixcheck);
});

// The #7154 security review's proof of concept: a real fix whose own lint script
// is a payload. It reads the job token through checkout's persisted credentials
// (included in every linked worktree), writes the checkout the merge step loads
// auto-merge.js from, then exits 1 so it never merges and leaves master clean.
const PAYLOAD = `#!/bin/sh
main=$(cd "$(git rev-parse --git-common-dir)/.." && pwd)
git config --get http.https://github.com/.extraheader > "$main/../STOLEN" || true
echo pwned > "$main/PWNED"
exit 1
`;
test("a fix-master PR's own lint script never runs before it merges, and no step can read a token", async () => {
  const origin = makeOrigin();
  origin.addPr(14, { 'big.txt': lines(2), 'scripts/lint-file-length.sh': PAYLOAD }, 'a real fix, plus a payload');
  const github = fakeGithub(origin, [[14, FIX]]);
  let beforeMerge = null;
  const job = await runJob(origin, github, {
    after: {
      fixcheck: (j) => {
        const scratch = path.join(j.temps.fixcheck, 'fix-14');
        const read = spawnSync('git', ['config', '--get', 'http.https://github.com/.extraheader'], { cwd: scratch, env: GIT_ENV, encoding: 'utf8' });
        beforeMerge = {
          payloadRan: fs.existsSync(path.join(origin.root, 'STOLEN')) || fs.existsSync(path.join(j.workspace, 'PWNED')),
          tokenInScratchTree: read.stdout.trim(),
        };
      },
    },
  });
  assert.deepEqual(beforeMerge, { payloadRan: false, tokenInScratchTree: '' });
  // Judged by master's lint, the real fix passes and lands.
  assert.deepEqual(github.merged, [14], job.logs.fixcheck);
  // Once merged, the post-merge check runs master's new script: that exposure
  // predates this check and is #7165's. Even there no token is readable.
  const stolen = path.join(origin.root, 'STOLEN');
  assert.equal(fs.existsSync(stolen) ? fs.readFileSync(stolen, 'utf8').trim() : '', '');
});

test('unchanged: with no halt, a red master still lands only fix-master PRs, with no composition pre-check', async () => {
  const { github, job } = await halted([[12, FIX], [13]], { broken: false, buildRed: true });
  assert.ok(!job.ran.includes('recheck') && !job.ran.includes('fixcheck'));
  assert.deepEqual(github.merged, [12]);
});

test('the fix check runs the same composition commands as the re-check and the post-merge check', () => {
  const steps = Object.fromEntries(parseSteps(YML).map((s) => [s.id, s]));
  assert.ok(steps.fixcheck, 'auto-merge.yml has a fixcheck step');
  const commands = (run) => run.split('\n').map((l) => l.trim()).filter(Boolean);
  assert.deepEqual(commands(steps.recheck.run), ['go build ./...', 'go vet ./...', 'scripts/lint-file-length.sh']);
  assert.deepEqual(commands(steps.compose.run), commands(steps.recheck.run));
  for (const command of commands(steps.recheck.run)) assert.ok(steps.fixcheck.run.includes(command), command);
});
