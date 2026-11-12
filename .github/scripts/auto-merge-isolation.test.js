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
  YML, parseJobs, makeOrigin, FIX, fakeGithub, runWorkflow, outcome, jobResult,
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

// Not `result`: a check step is continue-on-error, so its job succeeds whether
// or not the check did. Only the `passed` output carries the verdict.
test("the other jobs read nothing from a check but its verdict: pass or fail, the sha checked, the fixes that passed", () => {
  const checks = new Set(JOBS.filter(runsComposition).map((job) => job.id));
  const verdict = new Set(['outputs.passed', 'outputs.sha', 'outputs.fixes', 'outputs.base']);
  const reads = [];
  for (const job of JOBS.filter((j) => !runsComposition(j))) {
    for (const [, id, field] of job.text.matchAll(/needs\.([\w-]+)\.([\w.]+)/g)) {
      if (checks.has(id)) reads.push(`${job.id} <- ${id}.${field}`);
      if (checks.has(id)) assert.ok(verdict.has(field), `${job.id} reads needs.${id}.${field}`);
    }
  }
  for (const check of checks) assert.ok(reads.some((r) => r.includes(`<- ${check}.`)), `nothing acts on ${check}: ${reads}`);
});

test('every check reports a passed verdict, taken from its check step', () => {
  for (const job of JOBS.filter(runsComposition)) {
    const check = job.steps.find((step) => step.run && COMPOSITION.test(step.run));
    assert.equal(job.outputs.passed, `\${{ steps.${check.id}.outcome == 'success' }}`, `${job.id}: passed`);
  }
});

// Two layers, each of which must hold alone: the merge step's `if:`, and the
// `passed` mergeHaltFix refuses on. One alone cannot be seen from a run.
test("the fix merge is gated twice on the fix check's verdict: the step's if, and mergeHaltFix's passed", () => {
  const merge = JOBS.find((job) => job.id === 'land').steps.find((step) => step.id === 'merge');
  assert.match(merge.if, /needs\.fixcheck\.outputs\.passed == 'true'/);
  const carrier = Object.entries(merge.env).find(([, value]) => value === '${{ needs.fixcheck.outputs.passed }}');
  assert.ok(carrier, 'no env var carries the verdict to mergeHaltFix');
  assert.match(merge.with.script, new RegExp(`passed: process\\.env\\.${carrier[0]} === 'true'`));
});

test('only master runs the merger: a dispatch from any other ref would run that ref\'s auto-merge.js with the write token', () => {
  const gate = JOBS.find((job) => job.id === 'gate');
  assert.match(gate.if, /github\.ref == 'refs\/heads\/master'/);
  for (const job of JOBS.filter((j) => j.id !== 'gate')) assert.ok(job.needs.includes('gate') || job.needs.some((n) => JOBS.find((j) => j.id === n).needs.includes('gate')), `${job.id} does not wait for gate`);
});

test('the post-merge check runs whenever something merged, even if land failed afterwards', () => {
  const compose = JOBS.find((job) => job.id === 'compose');
  assert.match(compose.if, /!cancelled\(\)/);
  assert.match(compose.if, /needs\.land\.outputs\.merged == 'true'/);
});

test('the workflow-wide token can only read the repository; each job asks for its own', () => {
  assert.match(YML, /^permissions:\n {2}contents: read\n(?! )/m);
  for (const job of JOBS) assert.ok(Object.keys(job.permissions).length > 0, `${job.id} sets no permissions`);
});

// A job receives only the secrets it names, so naming one, even to test whether
// it is set, ships it to that runner (#7194). Only land merges.
test('only land names a secret', () => {
  assert.deepEqual(JOBS.filter((job) => /\bsecrets\./.test(job.text)).map((job) => job.id), ['land']);
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

// The #7172 security review's R1-R3: a missing verdict fails closed on every
// path, not only the re-check's.
test('a post-merge check that never reports (a lost runner) still reports master broken', async () => {
  const origin = makeOrigin();
  origin.squash(origin.heads[12], 'the fix, hand-merged'); // master passes
  const github = fakeGithub(origin, [[13]], { broken: false });
  const run = await runWorkflow(origin, github, { crash: (job) => job.id === 'compose' });
  assert.deepEqual(github.merged, [13]);
  assert.equal(jobResult(run, 'compose'), 'failure');
  assert.equal(github.opened.length, 1, 'a lost post-merge check left the merge unreported');
});

test('a fault in the halt check never lets a halted master merge ordinary PRs', async () => {
  const origin = makeOrigin(); // master broken, issue open
  const github = fakeGithub(origin, [[13]]);
  await runWorkflow(origin, github, { fail: (job, step) => step.id === 'halt' });
  assert.deepEqual(github.merged, [], 'merged during a halt because the halt check faulted');
});

test('a fix check that never reports (a lost runner) merges nothing', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX]]);
  await runWorkflow(origin, github, { crash: (job) => job.id === 'fixcheck' });
  assert.deepEqual(github.merged, []);
  assert.equal(github.issue.state, 'open');
});

// Code in a check job can forge that job's outputs: a runner gives it sudo. The
// jobs holding a token must act only on what they re-validate.
const forge = (stepId, outputs) => (run) => {
  Object.assign(run.state[stepId].outputs, outputs.outputs || {});
  if (outputs.outcome) run.state[stepId].outcome = outputs.outcome;
};

test('a fix list without a passing verdict merges nothing', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX]]);
  // #12 really passes, so the list is real; the check's verdict says failed.
  const run = await runWorkflow(origin, github, { after: { fixcheck: forge('fixcheck', { outcome: 'failure' }) } });
  assert.notEqual(outcome(run, 'fixcheck'), 'success');
  assert.ok(!run.ran.includes('merge'), "the merge step's if: let a list through without a verdict");
  assert.deepEqual(github.merged, [], 'a fix landed on its list alone, without the check passing');
});

test("a forged sha from the post-merge check never reaches the broken-master issue", async () => {
  const origin = makeOrigin();
  origin.squash(origin.heads[12], 'the fix, hand-merged');
  const github = fakeGithub(origin, [[13]], { broken: false });
  const forged = 'not-a-sha, then prose';
  await runWorkflow(origin, github, {
    after: { compose: forge('compose', { outcome: 'failure' }), compose_checkout: forge('compose_checkout', { outputs: { commit: forged } }) },
  });
  assert.equal(github.opened.length, 1);
  const { title, body } = github.opened[0];
  assert.ok(!title.includes('not-a-sha') && !body.includes('not-a-sha'), `forged text reached the issue: ${title}`);
  assert.ok(title.endsWith(origin.master().slice(0, 10)), `the issue names master's head instead: ${title}`);
});

test('a dispatch from a branch runs no job at all', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX], [13]], { broken: false });
  const run = await runWorkflow(origin, github, { ref: 'refs/heads/some-branch' });
  assert.deepEqual(run.ran, [], 'a step ran on a branch dispatch');
  assert.equal(jobResult(run, 'gate'), 'skipped');
  assert.deepEqual(github.merged, []);
});
