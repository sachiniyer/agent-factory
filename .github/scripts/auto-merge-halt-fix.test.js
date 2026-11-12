// A composition halt (#7151). While a "master broken by merge batch" issue is
// open and master still fails the composition check, a green fix-master PR may
// land through the merger, and only once its would-be merge result passes that
// same check. Nothing else lands, and the red-Build fix-master path is unchanged.
//
// They run auto-merge.yml itself, through auto-merge-harness.js.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const test = require('node:test');
const {
  YML, merger, parseJobs, GIT_ENV, git, lines, makeOrigin, FIX, fakeGithub,
  CONTEXT, runWorkflow, fakeCore, halted, outcome, output,
} = require('./auto-merge-harness.js');

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
  const next = await runWorkflow(origin, github);
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
  const job = await runWorkflow(origin, github, {
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
  const job = await runWorkflow(origin, github, { after: { fixcheck: () => { origin.squash(origin.heads[13], 'hand merge'); } } });
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
  const job = await runWorkflow(origin, github, { after: { fixcheck: moveMasterDuringNextPrRead } });
  assert.equal(outcome(job, 'fixcheck'), 'success');
  assert.deepEqual(github.merged, []);
  assert.ok(job.warnings.some((w) => /master moved/.test(w)), job.warnings.join('\n'));
});

test('a fix is not merged if it was held after it was checked', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX]]);
  const job = await runWorkflow(origin, github, {
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
  // `fixes` is what the check step outputs: "number:head" per passing candidate.
  const call = async (passed, number) => {
    const outputs = {};
    const job = { warnings: [], notices: [] };
    const args = { passed, fixes: `${number}:${origin.heads[number]}`, base: origin.broken };
    await merger.mergeHaltFix({ github, core: fakeCore(job, outputs), context: CONTEXT, ...args });
    return { merged: outputs.merged, warnings: job.warnings.join('\n') };
  };
  // Each refusal must come from the guard it names, not from a malformed call.
  const unchecked = await call(false, 12);
  assert.equal(unchecked.merged, 'false');
  assert.match(unchecked.warnings, /no merge result passed the composition check/);
  const ordinary = await call(true, 13);
  assert.equal(ordinary.merged, 'false');
  assert.match(ordinary.warnings, /#13 not merged \(master fails the composition check\)/);
  assert.deepEqual(github.merged, []);
  assert.equal((await call(true, 12)).merged, 'true');
  assert.deepEqual(github.merged, [12]);
});

test('a fix is not merged if its head moved after it was checked', async () => {
  const origin = makeOrigin();
  const github = fakeGithub(origin, [[12, FIX]]);
  // A push lands after the check: still labelled, still green, never checked.
  const job = await runWorkflow(origin, github, { after: { fixcheck: () => { github.prs[0].headRefOid = origin.heads[13]; } } });
  assert.equal(outcome(job, 'fixcheck'), 'success');
  assert.deepEqual(github.merged, [], 'an unchecked head was merged');
  assert.ok(job.warnings.some((w) => /head moved/.test(w)), job.warnings.join('\n'));
});

test('a checked fix whose merge GitHub refuses does not starve the next one', async () => {
  const origin = makeOrigin();
  origin.addPr(15, { 'big.txt': lines(3) }, 'another fix');
  const github = fakeGithub(origin, [[12, FIX], [15, FIX]]);
  github.refuse.add(12);
  const job = await runWorkflow(origin, github);
  assert.deepEqual(github.merged, [15]);
  assert.ok(job.warnings.some((w) => /#12: merge refused/.test(w)), job.warnings.join('\n'));
});

test("a fix-master PR cannot pass the check by editing the lint script: master's lints the merge tree", async () => {
  const origin = makeOrigin();
  // big.txt stays at 4 lines; the PR's own copy of the lint passes anything.
  origin.addPr(16, { 'scripts/lint-file-length.sh': '#!/usr/bin/env bash\nexit 0\n' }, 'a lint that passes everything');
  const github = fakeGithub(origin, [[16, FIX]]);
  const job = await runWorkflow(origin, github);
  assert.deepEqual(github.merged, []);
  assert.equal(outcome(job, 'fixcheck'), 'failure', job.logs.fixcheck);
});

test("master's lint is restored into the tree without writing through a symlink the PR planted", async () => {
  const origin = makeOrigin();
  // Stands in for a file outside the tree, such as the merge step's auto-merge.js.
  const outside = path.join(origin.root, 'outside.js');
  fs.writeFileSync(outside, 'untouched\n');
  origin.addPr(17, { 'big.txt': lines(2), 'scripts/lint-file-length.sh': { symlink: outside } }, 'a fix whose lint is a symlink');
  const github = fakeGithub(origin, [[17, FIX]]);
  const job = await runWorkflow(origin, github);
  assert.equal(fs.readFileSync(outside, 'utf8'), 'untouched\n');
  assert.deepEqual(github.merged, [17], job.logs.fixcheck);
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
  const job = await runWorkflow(origin, github, {
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
  const steps = Object.fromEntries(parseJobs(YML).flatMap((job) => job.steps).map((s) => [s.id, s]));
  assert.ok(steps.fixcheck, 'auto-merge.yml has a fixcheck step');
  const commands = (run) => run.split('\n').map((l) => l.trim()).filter(Boolean);
  assert.deepEqual(commands(steps.recheck.run), ['go build ./...', 'go vet ./...', 'scripts/lint-file-length.sh']);
  assert.deepEqual(commands(steps.compose.run), commands(steps.recheck.run));
  for (const command of commands(steps.recheck.run)) assert.ok(steps.fixcheck.run.includes(command), command);
});
