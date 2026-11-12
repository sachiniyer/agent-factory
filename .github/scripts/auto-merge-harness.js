// The harness behind the auto-merge workflow tests. It runs auto-merge.yml itself,
// not a model of it. Every step's `if:` and `${{ }}` env is evaluated from the
// file, every github-script step runs its real script against a fake GitHub, and
// every `run:` step runs its real shell. The fake GitHub is backed by a scratch
// git origin (real git, real squash merges), and `go` is a stub, so a
// composition check really runs, on whatever tree the step under test hands it.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync, spawnSync } = require('node:child_process');
const test = require('node:test');

const ROOT = path.join(__dirname, '..', '..');
const YML = fs.readFileSync(path.join(ROOT, '.github', 'workflows', 'auto-merge.yml'), 'utf8');
const merger = require('./auto-merge.js');

// ---- auto-merge.yml's jobs and steps ---------------------------------------------

// A job's steps as {name, id, if, uses, run, env, with, 'continue-on-error'}.
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

// The workflow's jobs in file order, as {id, name, if, needs, permissions,
// outputs, text, steps}. `permissions` is a map, or the inline value as written
// (`{}`). `text` is the job's raw YAML, for structural checks.
function parseJobs(yaml) {
  const body = yaml.slice(yaml.indexOf('\njobs:\n') + '\njobs:\n'.length);
  const starts = [...body.matchAll(/^ {2}([\w-]+):\s*$/gm)];
  return starts.map((start, i) => {
    const text = body.slice(start.index, i + 1 < starts.length ? starts[i + 1].index : body.length);
    const job = { id: start[1], name: '', if: '', needs: [], permissions: {}, outputs: {}, text, steps: parseSteps(text) };
    let map = null;
    for (const line of text.split('\n').slice(1)) {
      const top = line.match(/^ {4}([\w-]+):\s*(.*)$/);
      if (top) {
        const [, key, value] = top;
        map = null;
        if (key === 'needs') job.needs = value.replace(/^\[|\]$/g, '').split(',').map((n) => n.trim()).filter(Boolean);
        else if ((key === 'permissions' || key === 'outputs') && value === '') map = job[key];
        else if (key !== 'steps') job[key] = value;
        continue;
      }
      const entry = map && line.match(/^ {6}([\w-]+):\s*(.*)$/);
      if (entry) map[entry[1]] = entry[2];
    }
    return job;
  });
}

// The expressions in this file (==, !=, &&, ||, !, parentheses, 'strings',
// always(), cancelled()) are valid JavaScript. Anything unset reads as '', as
// GitHub's null does. `ctx` carries the steps and needs contexts, the commit
// the workflow file was read from, and the ref the run is on.
const lenient = (obj) => new Proxy(obj, { get: (t, k) => (k in t ? t[k] : '') });
function evaluate(expr, ctx = {}) {
  const fn = new Function('steps', 'needs', 'secrets', 'vars', 'github', 'always', 'cancelled', `return (${expr});`);
  const github = lenient({ token: 'github-token', workflow_sha: ctx.workflowSha || '', ref: ctx.ref || '' });
  return fn(ctx.steps || lenient({}), ctx.needs || lenient({}), lenient({}), lenient({}), github, () => true, () => false);
}
const interpolate = (value, ctx) =>
  value.replace(/\$\{\{\s*([\s\S]*?)\s*\}\}/g, (_, expr) => String(evaluate(expr, ctx)));
// An `if:`, bare or wrapped in ${{ }}. Empty is true.
const condition = (expr, ctx) => !expr || Boolean(evaluate(expr.replace(/^\$\{\{\s*([\s\S]*?)\s*\}\}$/, '$1'), ctx));

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

// master:  base (big.txt 2 lines, and the merger's own scripts, which a
//          github-script step requires from its job's checkout)
//          -> broken batch (4 lines: the lint fails)
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
  // A body of { symlink: target } commits a symlink.
  const commit = (files, message) => {
    for (const [file, body] of Object.entries(files)) {
      const at = path.join(work, file);
      fs.mkdirSync(path.dirname(at), { recursive: true });
      fs.rmSync(at, { force: true });
      if (body.symlink) fs.symlinkSync(body.symlink, at);
      else fs.writeFileSync(at, body, { mode: file.endsWith('.sh') ? 0o755 : 0o644 });
    }
    git(work, 'add', '-A');
    git(work, 'commit', '-q', '-m', message);
    return git(work, 'rev-parse', 'HEAD');
  };
  const own = (name) => fs.readFileSync(path.join(ROOT, '.github', 'scripts', name), 'utf8');
  commit({
    'big.txt': lines(2), 'scripts/lint-file-length.sh': STUB_LINT,
    '.github/scripts/auto-merge.js': own('auto-merge.js'), '.github/scripts/macos-verdict.js': own('macos-verdict.js'),
  }, 'base');
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

// Runs the whole workflow once, jobs in file order (each job's needs come
// before it). `ran` lists the step ids that ran; a job's own result says
// whether it ran. A job runs when its `if:` holds and, unless that `if:` calls a
// status function, every job it needs succeeded, as GitHub's implicit success()
// does. Each job starts with no checkout, as on a fresh runner.
// `after` maps a step id to a hook run once that step finishes, given the run:
// the world moving between two steps. An id that names a job and no step runs
// after that job. `fail(job, step)` makes a step fail without running, as a
// network fault would; `crash(job)` ends a job with no outputs at all, as a lost
// runner or a timeout would. `ref` is the ref the run is on: master for the
// schedule and workflow_run, whatever `--ref` names for a dispatch.
async function runWorkflow(origin, github, { after = {}, fail = () => false, crash = () => false, ref = 'refs/heads/master' } = {}) {
  const run = { state: {}, jobs: {}, ran: [], warnings: [], notices: [], logs: {}, temps: {}, workspaces: {}, workspace: null };
  const needs = new Proxy({}, { get: (_, id) => run.jobs[id] || { result: '', outputs: lenient({}) } });
  // A scheduled run reads the workflow file from master as it is when it starts.
  const workflowSha = origin.master();
  for (const job of parseJobs(YML)) {
    for (const need of job.needs) assert.ok(run.jobs[need], `job ${job.id} needs ${need}, which comes after it`);
    const statusCheck = /\b(always|cancelled|success|failure)\s*\(/.test(job.if);
    const needsMet = statusCheck || job.needs.every((need) => run.jobs[need].result === 'success');
    if (!needsMet || !condition(job.if, { needs, workflowSha, ref })) {
      run.jobs[job.id] = { result: 'skipped', outputs: lenient({}) };
      continue;
    }
    if (crash(job)) {
      run.jobs[job.id] = { result: 'failure', outputs: lenient({}) };
      continue;
    }
    run.workspace = null;
    const state = {};
    const steps = new Proxy({}, { get: (_, id) => state[id] || { outcome: '', outputs: lenient({}) } });
    const ctx = { steps, needs, workflowSha, ref };
    let failed = false;
    for (const step of job.steps) {
      const id = step.id || step.name;
      if (failed || !condition(step.if, ctx)) {
        state[id] = run.state[id] = { outcome: 'skipped', outputs: lenient({}) };
        continue;
      }
      run.ran.push(id);
      const env = Object.fromEntries(Object.entries(step.env).map(([k, v]) => [k, interpolate(v, ctx)]));
      let result;
      if (fail(job, step)) {
        result = { outcome: 'failure', outputs: {} };
      } else if (/^actions\/checkout@/.test(step.uses || '')) {
        // actions/checkout's default: depth 1, at the ref asked for.
        const ref = interpolate(step.with.ref || 'master', ctx);
        run.workspace = origin.newWorkspace();
        git(origin.root, 'clone', '-q', '--depth', '1', '--branch', 'master', `file://${origin.bare}`, run.workspace);
        if (ref !== 'master') {
          git(run.workspace, 'fetch', '-q', '--depth', '1', 'origin', ref);
          git(run.workspace, 'checkout', '-q', '--detach', ref);
        }
        if (step.with['persist-credentials'] !== 'false') {
          // v6 keeps the token in a file outside the repository, included for the
          // repository's gitdir and for every linked worktree's (git-auth-helper.ts).
          const creds = path.join(origin.root, `credentials-${path.basename(run.workspace)}.config`);
          fs.writeFileSync(creds, `[http "https://github.com/"]\n\textraheader = ${TOKEN_HEADER}\n`);
          git(run.workspace, 'config', `includeIf.gitdir:${run.workspace}/.git.path`, creds);
          git(run.workspace, 'config', `includeIf.gitdir:${run.workspace}/.git/worktrees/*.path`, creds);
        }
        result = { outcome: 'success', outputs: { commit: git(run.workspace, 'rev-parse', 'HEAD') } };
      } else if (/^actions\/setup-go@/.test(step.uses || '')) {
        result = { outcome: 'success', outputs: {} };
      } else if (/^actions\/github-script@/.test(step.uses || '')) {
        assert.ok(run.workspace, `"${step.name}" would require its script outside a checkout`);
        result = await runScript(step.with.script, env, github, run);
      } else if (step.run) {
        assert.ok(run.workspace, `"${step.name}" would run outside a checkout`);
        result = runShell(step.run, env, run.workspace, origin);
        run.logs[id] = result.log;
        run.temps[id] = result.runnerTemp;
      } else {
        throw new Error(`no handler for step "${step.name}"`);
      }
      state[id] = run.state[id] = { outcome: result.outcome, outputs: lenient(result.outputs) };
      if (result.outcome !== 'success' && step['continue-on-error'] !== 'true') failed = true;
      if (after[id]) await after[id](run);
    }
    const outputs = Object.fromEntries(Object.entries(job.outputs).map(([k, v]) => [k, interpolate(v, ctx)]));
    run.jobs[job.id] = { result: failed ? 'failure' : 'success', outputs: lenient(outputs) };
    run.workspaces[job.id] = run.workspace;
    if (after[job.id] && !job.steps.some((step) => step.id === job.id)) await after[job.id](run);
  }
  return run;
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
  // Each github-script step is its own node process: nothing it requires is
  // cached from an earlier step.
  for (const cached of Object.keys(require.cache)) {
    if (job.workspace && cached.startsWith(job.workspace + path.sep)) delete require.cache[cached];
  }
  const saved = Object.fromEntries(Object.keys(env).map((k) => [k, process.env[k]]));
  Object.assign(process.env, env);
  try {
    // github-script resolves a relative require against the workspace: the job's
    // own checkout, whatever an earlier step of the job left in it.
    await new AsyncFunction('github', 'core', 'context', 'require', script)(
      github, core, CONTEXT, (p) => require(path.join(job.workspace, p)),
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

// One scenario: a fresh origin, the open PRs, the workflow run once.
async function halted(openPrs, opts = {}) {
  const origin = makeOrigin();
  const github = fakeGithub(origin, openPrs, opts);
  const job = await runWorkflow(origin, github, opts);
  return { origin, github, job };
}
// A step's outcome and outputs, and a job's result and outputs.
const outcome = (run, id) => (run.state[id] || { outcome: `no step ${id}` }).outcome;
const output = (run, id, key) => (run.state[id] ? run.state[id].outputs[key] : `no step ${id}`);
const jobResult = (run, id) => (run.jobs[id] || { result: `no job ${id}` }).result;
const jobOutput = (run, id, key) => (run.jobs[id] ? run.jobs[id].outputs[key] : `no job ${id}`);

module.exports = {
  ROOT, YML, merger, parseSteps, parseJobs, evaluate, interpolate, condition, lenient,
  GIT_ENV, git, lines, TOKEN_HEADER, makeOrigin, GREEN, FIX, fakeGithub,
  CONTEXT, runWorkflow, fakeCore, runScript, runShell, halted, outcome, output, jobResult, jobOutput,
};
