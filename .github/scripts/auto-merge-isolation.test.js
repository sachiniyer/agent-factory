// No code a PR controls runs where the merger's write token or secrets are
// (#7165). Every composition check (the post-merge check, the broken-master
// re-check and the halted fix check) runs in its own job: contents: read, no
// secrets, no persisted credentials, no Go cache saved. The jobs that hold the
// token run only auto-merge.js, from a fresh checkout of the commit the
// workflow file came from, and read nothing from a check but its verdict.
//
// The structural tests read auto-merge.yml. The behavioural ones run it,
// through auto-merge-harness.js.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const {
  YML, parseJobs, makeOrigin, fakeGithub, runWorkflow, outcome,
} = require('./auto-merge-harness.js');

const JOBS = parseJobs(YML);
const COMPOSITION = /\bgo (build|vet) \.\/\.\.\.|lint-file-length\.sh/;
const runsComposition = (job) => job.steps.some((step) => step.run && COMPOSITION.test(step.run));
const uses = (step, action) => (step.uses || '').startsWith(`actions/${action}@`);
const holdsWrite = (job) => Object.values(job.permissions).includes('write');

// ---- What the workflow file says ------------------------------------------------

test('the three composition checks are three jobs of their own', () => {
  assert.deepEqual(JOBS.filter(runsComposition).map((job) => job.id), ['recheck', 'fixcheck', 'compose']);
});

test('a composition-check job can read the repository and nothing else', () => {
  for (const job of JOBS.filter(runsComposition)) {
    assert.deepEqual({ ...job.permissions }, { contents: 'read' }, `${job.id}: permissions`);
    assert.doesNotMatch(job.text, /\bsecrets\./, `${job.id} references a secret`);
    assert.ok(!job.steps.some((step) => uses(step, 'github-script')), `${job.id} runs a script with the job token`);
    for (const step of job.steps.filter((s) => uses(s, 'checkout'))) {
      assert.equal(step.with['persist-credentials'], 'false', `${job.id}: "${step.name}" leaves the token in git config`);
    }
    for (const step of job.steps.filter((s) => uses(s, 'setup-go'))) {
      assert.equal(step.with.cache, 'false', `${job.id}: "${step.name}" saves a Go cache release builds could restore`);
    }
    assert.ok(Object.keys(job.outputs).every((key) => ['passed', 'sha', 'fixes', 'base'].includes(key)),
      `${job.id} outputs more than a verdict: ${Object.keys(job.outputs)}`);
  }
});

test("every other job runs only auto-merge.js, from a fresh checkout of the workflow's own commit", () => {
  const others = JOBS.filter((job) => !runsComposition(job));
  assert.ok(others.some(holdsWrite), 'some job holds the write token');
  for (const job of others) {
    for (const step of job.steps) {
      assert.ok(uses(step, 'checkout') || uses(step, 'github-script'), `${job.id}: "${step.name}" runs something else`);
      if (!uses(step, 'checkout')) continue;
      assert.equal(step.with.ref, '${{ github.workflow_sha }}', `${job.id}: "${step.name}" checks out a tree a check could have touched`);
      assert.equal(step.with['persist-credentials'], 'false', `${job.id}: "${step.name}" leaves the token in git config`);
    }
  }
});

test("the other jobs read nothing from a check but its verdict: pass or fail, the sha checked, the fixes that passed", () => {
  const checks = new Set(JOBS.filter(runsComposition).map((job) => job.id));
  const verdict = new Set(['outputs.passed', 'outputs.sha', 'outputs.fixes', 'outputs.base', 'result']);
  const reads = [];
  for (const job of JOBS.filter((j) => !runsComposition(j))) {
    for (const [, id, field] of job.text.matchAll(/needs\.([\w-]+)\.([\w.]+)/g)) {
      if (checks.has(id)) reads.push(`${job.id} <- ${id}.${field}`);
      if (checks.has(id)) assert.ok(verdict.has(field), `${job.id} reads needs.${id}.${field}`);
    }
  }
  for (const check of checks) assert.ok(reads.some((r) => r.includes(`<- ${check}.`)), `nothing acts on ${check}: ${reads}`);
});

test('the workflow-wide token can only read the repository; each job asks for its own', () => {
  assert.match(YML, /^permissions:\n {2}contents: read\n(?! )/m);
  for (const job of JOBS) assert.ok(Object.keys(job.permissions).length > 0, `${job.id} sets no permissions`);
});

test('step ids are unique across the workflow, so each names one step', () => {
  const ids = JOBS.flatMap((job) => job.steps.map((step) => step.id).filter(Boolean));
  assert.deepEqual(ids.filter((id, i) => ids.indexOf(id) !== i), []);
});

// ---- What running it does -------------------------------------------------------

// A PR, once merged, whose lint script (which the post-merge check runs) rewrites
// the checkout's auto-merge.js. A github-script step that later requires that
// file from that checkout runs the PR's code with the write token.
const hijack = (marker) => `#!/bin/sh
cat > .github/scripts/auto-merge.js <<'JS'
require('node:fs').writeFileSync(${JSON.stringify(marker)}, 'the PR ran with the write token');
module.exports = new Proxy({}, { get: () => async () => {} });
JS
echo pwned > PWNED
exit 1
`;

test("a merged PR's lint script, run by the post-merge check, cannot get its code run with the write token", async () => {
  const origin = makeOrigin();
  const marker = path.join(origin.root, 'HIJACKED');
  origin.addPr(18, { 'scripts/lint-file-length.sh': hijack(marker) }, 'an ordinary PR with a payload');
  const github = fakeGithub(origin, [[18]], { broken: false });
  let checkout = null;
  const run = await runWorkflow(origin, github, { after: { compose: (r) => { checkout = r.workspace; } } });
  assert.deepEqual(github.merged, [18]);
  // The payload did run, in the post-merge check, and failed it...
  assert.equal(outcome(run, 'compose'), 'failure');
  assert.ok(fs.existsSync(path.join(checkout, 'PWNED')), 'the payload never ran: the scenario proves nothing');
  // ...yet the report that follows ran the merger's own reportBroken.
  assert.ok(!fs.existsSync(marker), 'a script step required the auto-merge.js the PR wrote');
  assert.equal(github.opened.length, 1, 'the real reportBroken opened the broken-master issue');
});

test('a check that never reaches its verdict counts as a failure, not a pass', async () => {
  const runsRecheck = (job) => job.steps.some((step) => step.id === 'recheck');
  const faults = {
    // A fault before the check runs: the job still reports, and says failed.
    'a step fault': { fail: (job, step) => runsRecheck(job) && uses(step, 'setup-go') },
    // A lost runner or a timeout: the job reports nothing at all.
    'a lost runner': { crash: runsRecheck },
  };
  for (const [fault, opts] of Object.entries(faults)) {
    const origin = makeOrigin();
    origin.squash(origin.heads[12], 'the fix, hand-merged'); // master passes again
    const github = fakeGithub(origin, [[13]]);
    const run = await runWorkflow(origin, github, opts);
    assert.equal(github.issue.state, 'open', `${fault}: a missing verdict closed the broken-master issue`);
    assert.deepEqual(github.merged, [], fault);
    assert.notEqual(outcome(run, 'recheck'), 'success', fault);
  }
});
