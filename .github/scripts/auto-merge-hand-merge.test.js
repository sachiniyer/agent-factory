// A file that a job holding a write token loads changes only through a
// maintainer's hand merge, as .github/workflows/ always has (#7194). The
// merger's own scripts run in `land` with the write token from the next run on,
// so a PR that edits one and lands unreviewed runs its code with that token:
// #7165 kept PR-controlled code away from the token within a run, and this
// keeps it away across runs.
//
// The set is derived from the workflow files, not listed: every .github/ file
// that a job holding a write token or a secret names, and everything those
// files require. The behavioural tests run auto-merge.yml through
// auto-merge-harness.js.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const {
  ROOT, merger, lines, makeOrigin, FIX, fakeGithub, runWorkflow, jobOutput,
} = require('./auto-merge-harness.js');
const gate = require('./auto-gate.js');

const WORKFLOWS = path.join(ROOT, '.github', 'workflows');
const read = (file) => fs.readFileSync(path.join(ROOT, file), 'utf8');
// Full-line comments name files that nothing loads (".github/auto-merge.md").
const code = (text) => text.split('\n').filter((line) => !/^\s*#/.test(line)).join('\n');

// The permissions a mapping sets at `indent`, as written (a block's lines
// joined, or the inline value), or null when it sets none there.
function permissionsAt(text, indent) {
  const pad = ' '.repeat(indent);
  const key = text.match(new RegExp(`^${pad}permissions:[ \\t]*(.*)$`, 'm'));
  if (!key) return null;
  if (key[1].trim()) return key[1].trim();
  const block = [];
  for (const line of text.slice(key.index + key[0].length + 1).split('\n')) {
    if (!line.startsWith(`${pad}  `)) break;
    block.push(line.trim());
  }
  return block.join('\n');
}

// A job holds a write token when the permissions in force grant any write: its
// own, else the workflow's, else the repository default, which may be write.
// It holds a secret when it names one.
function holdsToken(workflowText, jobText) {
  const own = permissionsAt(jobText, 4);
  const perms = own !== null ? own : permissionsAt(workflowText, 0);
  return perms === null || /\bwrite\b|write-all/.test(perms) || /\bsecrets\./.test(code(jobText));
}

// The .github/ files a text names: a directory stands for every file in it.
function named(text) {
  const found = [];
  for (const [mention] of code(text).matchAll(/\.github\/[\w./-]*[\w-]/g)) {
    const at = path.join(ROOT, mention);
    if (!fs.existsSync(at)) continue;
    if (fs.statSync(at).isDirectory()) {
      for (const entry of fs.readdirSync(at, { recursive: true })) {
        const file = path.posix.join(mention, entry);
        if (fs.statSync(path.join(ROOT, file)).isFile()) found.push(file);
      }
    } else {
      found.push(mention);
    }
  }
  return found;
}

// What a loaded file loads in turn: a script's relative requires, wherever they
// lead (a require outside .github/ is still loaded with the token), and the
// .github/ files a shell script names.
function loadsOf(file) {
  if (file.endsWith('.sh')) return named(read(file));
  if (!/\.[cm]?js$/.test(file)) return [];
  return [...read(file).matchAll(/require\(\s*["'](\.{1,2}\/[^"']+)["']\s*\)/g)].map(([, rel]) => {
    const target = path.posix.normalize(path.posix.join(path.posix.dirname(file), rel));
    return fs.existsSync(path.join(ROOT, target)) ? target : `${target}.js`;
  });
}

// Every file loaded with a write token or a secret, mapped to the first
// workflow job that loads it.
function writeTokenLoads() {
  const loads = new Map();
  for (const workflow of fs.readdirSync(WORKFLOWS).filter((f) => /\.ya?ml$/.test(f)).sort()) {
    const text = fs.readFileSync(path.join(WORKFLOWS, workflow), 'utf8');
    const jobsAt = text.search(/^jobs:[ \t]*$/m);
    assert.ok(jobsAt >= 0, `${workflow}: no jobs: key`);
    const head = text.slice(0, jobsAt);
    const body = text.slice(jobsAt).split('\n').slice(1).join('\n');
    const starts = [...body.matchAll(/^ {2}([\w-]+):[ \t]*$/gm)];
    starts.forEach((start, i) => {
      const job = body.slice(start.index, i + 1 < starts.length ? starts[i + 1].index : body.length);
      if (!holdsToken(head, job)) return;
      const queue = named(job);
      while (queue.length) {
        const file = queue.shift();
        if (loads.has(file)) continue;
        loads.set(file, `${workflow} ${start[1]}`);
        queue.push(...loadsOf(file));
      }
    });
  }
  return loads;
}

const files = (...paths) => ({ pageInfo: { hasNextPage: false }, nodes: paths.map((p) => ({ path: p })) });

test('every .github/ file a job holding a write token or a secret loads needs a hand merge', () => {
  const loads = writeTokenLoads();
  // Anti-vacuity: a derivation that stopped seeing jobs or requires would pass.
  // These are the merger and what it requires, the gate and what it requires,
  // and a release script that runs with contents: write.
  for (const file of [
    '.github/scripts/auto-merge.js', '.github/scripts/macos-verdict.js', '.github/scripts/auto-gate.js',
    '.github/scripts/codex-outage.js', '.github/scripts/go-inert.js', '.github/scripts/release-bump.sh',
  ]) {
    assert.ok(loads.has(file), `the derivation no longer sees ${file} loaded with a token`);
  }
  const unguarded = [...loads].filter(([file]) => !merger.touchesWorkflows(files(file)));
  assert.deepEqual(unguarded.map(([file, job]) => `${file} (${job})`), [],
    'auto-merge.js would land a PR that changes these');
  assert.equal(typeof gate.handMergePaths, 'function', 'auto-gate.js has no hand-merge predicate');
  const ungated = [...loads].filter(([file]) => !gate.handMergePaths([{ filename: file }]).length);
  assert.deepEqual(ungated.map(([file, job]) => `${file} (${job})`), [],
    'Auto Gate would auto-merge a PR that changes these');
});

test('a PR touching neither workflows nor scripts still auto-merges', () => {
  assert.equal(merger.touchesWorkflows(files('main.go', 'docs/auto-merge.md', '.github/auto-merge.md')), false);
  assert.equal(merger.touchesWorkflows(files('scripts/lint-file-length.sh', 'web/src/app.ts')), false);
});

// ---- What running it does -------------------------------------------------------

const tamper = (name) => `// tampered by the PR\n${read(`.github/scripts/${name}`)}`;
const skipped = (run, number) => run.infos.filter((line) => line.startsWith(`#${number}: skip (`));

test("a green PR that edits the merger's own scripts is skipped for a hand merge, not merged", async () => {
  const origin = makeOrigin();
  origin.squash(origin.heads[12], 'the fix, hand-merged'); // master passes
  origin.addPr(19, { '.github/scripts/auto-merge.js': tamper('auto-merge.js') }, 'edits the merger');
  origin.addPr(21, { '.github/scripts/macos-verdict.js': tamper('macos-verdict.js') }, 'edits what the merger requires');
  const github = fakeGithub(origin, [[13], [19], [21]], { broken: false });
  const run = await runWorkflow(origin, github);
  assert.deepEqual(github.merged, [13]);
  for (const number of [19, 21]) {
    assert.deepEqual(skipped(run, number), [`#${number}: skip (${merger.WORKFLOW_SKIP_REASON})`]);
  }
  assert.match(merger.WORKFLOW_SKIP_REASON, /maintainer merge/);
});

test('a halted master never lands a fix-master PR that edits the merger, though its merge passes the check', async () => {
  const origin = makeOrigin(); // master broken, issue open
  origin.addPr(20, { 'big.txt': lines(2), '.github/scripts/macos-verdict.js': tamper('macos-verdict.js') },
    'a real fix that also edits a script the merger loads');
  const github = fakeGithub(origin, [[20, FIX]]);
  const run = await runWorkflow(origin, github);
  assert.deepEqual(github.merged, []);
  assert.equal(github.issue.state, 'open');
  assert.equal(jobOutput(run, 'gate', 'candidates'), '', 'the fix check was handed a script-editing PR');
});

// The read-only gate cannot see whether AUTO_MERGE_TOKEN is set, and should
// not: a job receives only the secrets it names. So its pick applies the
// GITHUB_TOKEN rule whatever the token, and a halted master lands no fix that
// edits workflows or scripts even when the PAT could merge one.
test('with AUTO_MERGE_TOKEN set, a halted master still lands no fix that edits workflows or scripts', async () => {
  for (const edit of ['.github/workflows/build.yml', '.github/scripts/auto-merge.js']) {
    const origin = makeOrigin();
    origin.addPr(20, { 'big.txt': lines(2), [edit]: '# edited by the PR\n' }, `a real fix that also edits ${edit}`);
    const github = fakeGithub(origin, [[20, FIX]]);
    const run = await runWorkflow(origin, github, { secrets: { AUTO_MERGE_TOKEN: 'a-pat' } });
    assert.deepEqual(github.merged, [], edit);
    assert.equal(jobOutput(run, 'gate', 'candidates'), '', `${edit}: the gate picked it`);
  }
});
