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

// Every job of every workflow, as { workflow, head, id, text }: `head` is the
// workflow's text before `jobs:`, `text` the job's own lines.
function workflowJobs() {
  const jobs = [];
  for (const workflow of fs.readdirSync(WORKFLOWS).filter((f) => /\.ya?ml$/.test(f)).sort()) {
    const text = fs.readFileSync(path.join(WORKFLOWS, workflow), 'utf8');
    const jobsAt = text.search(/^jobs:[ \t]*$/m);
    assert.ok(jobsAt >= 0, `${workflow}: no jobs: key`);
    const head = text.slice(0, jobsAt);
    const body = text.slice(jobsAt).split('\n').slice(1).join('\n');
    const starts = [...body.matchAll(/^ {2}([\w-]+):[ \t]*$/gm)];
    starts.forEach((start, i) => {
      const job = body.slice(start.index, i + 1 < starts.length ? starts[i + 1].index : body.length);
      jobs.push({ workflow, head, id: start[1], text: job });
    });
  }
  return jobs;
}

// Every file loaded with a write token or a secret, mapped to the first
// workflow job that loads it.
function writeTokenLoads() {
  const loads = new Map();
  for (const job of workflowJobs().filter((j) => holdsToken(j.head, j.text))) {
    const queue = named(job.text);
    while (queue.length) {
      const file = queue.shift();
      if (loads.has(file)) continue;
      loads.set(file, `${job.workflow} ${job.id}`);
      queue.push(...loadsOf(file));
    }
  }
  return loads;
}

// A job can merge a pull request when the permissions in force grant contents:
// write (PUT /pulls/N/merge needs nothing more) or it names a secret other than
// its own GITHUB_TOKEN. Unset permissions mean the repository default, which may
// be write.
function mergeCapable(job) {
  const own = permissionsAt(job.text, 4);
  const perms = own !== null ? own : permissionsAt(job.head, 0);
  if (perms === null || /write-all|\bcontents:\s*write\b/.test(perms)) return true;
  return [...code(job.text).matchAll(/\bsecrets\.(\w+)/g)].some(([, name]) => name !== 'GITHUB_TOKEN');
}

// The shell a job's `run:` steps execute, block scalars included.
function runScripts(text) {
  const scripts = [];
  const lines = text.split('\n');
  lines.forEach((line, i) => {
    const key = line.match(/^(\s*(?:- )?)run:[ \t]*(.*)$/);
    if (!key) return;
    if (!/^[|>][-+]?$/.test(key[2].trim())) {
      scripts.push(key[2]);
      return;
    }
    const block = [];
    for (const next of lines.slice(i + 1)) {
      if (next.trim() && next.search(/\S/) <= key[1].length) break;
      block.push(next);
    }
    scripts.push(block.join('\n'));
  });
  return scripts.join('\n');
}

// What in a merge-capable job would run repository code outside the hand-merge
// prefixes: a toolchain, a `go` command (go test and go run run the tree's code,
// and go build makes a binary to run), a built af binary, or a repository file
// outside the prefixes named anywhere a step could run or require it.
function runsRepositoryCode(job) {
  const text = code(job.text);
  const shell = runScripts(text);
  const found = [];
  for (const [step] of text.matchAll(/uses:\s*actions\/setup-[\w-]+/g)) found.push(step);
  for (const [, command] of shell.matchAll(/(?:^|[\s;&|(`])((?:go|make|npm|npx|node|python3?|pip3?|docker)\s+\S+)/gm)) found.push(command);
  for (const [binary] of shell.matchAll(/\S*\/agent-factory\b(?![-.])\S*/g)) found.push(binary);
  for (const [token] of text.matchAll(/[\w./-]*[\w-]/g)) {
    const file = token.replace(/^\.\//, '');
    if (!file.includes('/') && !fs.existsSync(path.join(ROOT, file))) continue;
    const at = path.join(ROOT, file);
    if (!fs.existsSync(at) || !fs.statSync(at).isFile()) continue;
    if (!['.github/workflows/', '.github/scripts/'].some((prefix) => file.startsWith(prefix))) found.push(file);
  }
  return found;
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

// #7200 R2: a job that can merge a pull request must not run code a merged PR
// controls, or that code merges a PR this rule holds back. The release
// preflight ran `go test ./...` with contents: write, which would let any
// auto-merged Go change merge a held .github/scripts/ PR.
test('a job that can merge a pull request runs no repository code outside the hand-merge prefixes', () => {
  const capable = workflowJobs().filter(mergeCapable);
  // Anti-vacuity: the merger, the gate and the release publishers can merge.
  for (const [workflow, id] of [['auto-merge.yml', 'land'], ['auto-gate.yml', 'auto-gate'],
    ['auto-release.yml', 'release'], ['stable-release.yml', 'publish']]) {
    assert.ok(capable.some((job) => job.workflow === workflow && job.id === id), `${workflow} ${id} no longer reads as merge-capable`);
  }
  const offenders = capable.map((job) => [job, runsRepositoryCode(job)]).filter(([, found]) => found.length)
    .map(([job, found]) => `${job.workflow} ${job.id}: ${[...new Set(found)].join(', ')}`);
  assert.deepEqual(offenders, [], 'these jobs could merge a held PR from code a merged PR wrote');
});

test('the merge-capable lint sees a go command, a toolchain, a built binary and a script outside the prefixes', () => {
  const job = (steps) => ({ workflow: 'x.yml', id: 'j', head: 'permissions:\n  contents: write\n', text: `  j:\n    steps:\n${steps}` });
  assert.deepEqual(runsRepositoryCode(job('      - run: go test ./...\n')), ['go test']);
  assert.deepEqual(runsRepositoryCode(job('      - uses: actions/setup-go@v6\n')), ['uses: actions/setup-go']);
  assert.deepEqual(runsRepositoryCode(job('      - run: "$tmp/agent-factory" version\n')), ['"$tmp/agent-factory"']);
  assert.deepEqual(runsRepositoryCode(job('      - run: scripts/ci-apt-install.sh zsh\n')), ['scripts/ci-apt-install.sh']);
  assert.deepEqual(runsRepositoryCode(job('      - run: .github/scripts/write-sha256sums.sh dist\n      - run: gh release create v1 dist/*.tar.gz\n')), []);
  assert.equal(mergeCapable(job('')), true);
  assert.equal(mergeCapable({ ...job(''), head: 'permissions:\n  contents: read\n' }), false);
  assert.equal(mergeCapable({ ...job('        env:\n          K: ${{ secrets.DEPLOY_KEY }}\n'), head: 'permissions:\n  contents: read\n' }), true);
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

// #7200 R3: the merge pins the head the PR list read, so the file list must be
// that head's. A force-push between the two reads (clean head B for the files
// query, the script-editing head A back for the merge) once landed the edit.
// The reviewer's probe, kept as a regression test.
test('a head that moves between the PR list and the file list is not merged on the other head\'s files', async () => {
  const origin = makeOrigin();
  origin.squash(origin.heads[12], 'the fix, hand-merged'); // master passes
  origin.addPr(19, { '.github/scripts/auto-merge.js': tamper('auto-merge.js') }, 'A: edits the merger');
  const a = origin.heads[19];
  origin.addPr(98, { 'feature2.txt': 'x\n' }, 'B: clean');
  const b = origin.heads[98];
  delete origin.heads[98];
  const github = fakeGithub(origin, [[19]], { broken: false });
  const inner = github.graphql;
  github.graphql = async (query) => {
    if (!query.includes('files(')) return inner(query);
    origin.heads[19] = b; // force-pushed B before the files query...
    try { return await inner(query); } finally { origin.heads[19] = a; } // ...and A back after it
  };
  const run = await runWorkflow(origin, github);
  assert.deepEqual(github.merged, [], 'the merge landed head A on head B\'s file list');
  assert.doesNotMatch(origin.file('.github/scripts/auto-merge.js'), /tampered/);
  assert.match(skipped(run, 19).join('\n'), /head moved/);
});

// #7200 R6: GraphQL names a renamed file by its new path only, so a rename out
// of .github/scripts/ read as touching nothing there. It landed, and every
// later run's `land` failed to load the file it had moved.
test('a green PR that renames a script out of .github/scripts/ is skipped, not merged', async () => {
  const origin = makeOrigin();
  origin.squash(origin.heads[12], 'the fix, hand-merged'); // master passes
  origin.addPr(22, {
    '.github/scripts/macos-verdict.js': null, '.github/macos-verdict.js': read('.github/scripts/macos-verdict.js'),
  }, 'moves a script the merger requires');
  const github = fakeGithub(origin, [[13], [22]], { broken: false });
  const run = await runWorkflow(origin, github);
  assert.deepEqual(github.merged, [13]);
  assert.match(skipped(run, 22).join('\n'), /renam/);
  assert.ok(origin.file('.github/scripts/macos-verdict.js').length > 0);
});
