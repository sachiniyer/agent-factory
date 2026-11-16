const assert = require('node:assert/strict');
const test = require('node:test');
// The bound defaults to ten minutes here so the fail-first run of these tests
// against the unbound script fails by assertion, not a RangeError on Date(NaN).
const { shouldDebounce, checkDebounce, PENDING_RUN_MAX_AGE_MS = 10 * 60 * 1000 } = require('./auto-merge.js');

const SELF = 40664944064;
const base = { eventName: 'workflow_run', runId: SELF };
const run = (id, over = {}) => ({ id, status: 'completed', conclusion: 'success', readState: true, run_started_at: '2026-10-28T09:30:53Z', ...over });

test('the 09:30:53 sequence: Build completed 09:31:44, trigger at 09:31:46 -> NOT debounced', () => {
  const verdict = shouldDebounce({
    ...base,
    triggerUpdatedAt: '2026-10-28T09:31:44Z',
    // the running self row plus the older run that said "not yet verified"
    runs: [run(SELF, { status: 'in_progress', conclusion: null, run_started_at: '2026-10-28T09:31:46Z' }), run(40664823334)],
  });
  assert.equal(verdict.skip, false);
});

test('a trigger whose triggering run completed BEFORE another merger run started is debounced', () => {
  const verdict = shouldDebounce({
    ...base,
    triggerUpdatedAt: '2026-10-28T09:30:00Z',
    runs: [run(SELF, { status: 'in_progress', conclusion: null }), run(40664823334, { run_started_at: '2026-10-28T09:30:53Z' })],
  });
  assert.equal(verdict.skip, true);
  assert.match(verdict.reason, /40664823334/);
});

test('a newer queued merger run means debounce', () => {
  for (const status of ['queued', 'waiting', 'pending']) {
    const verdict = shouldDebounce({
      ...base,
      now: Date.parse('2026-10-28T09:32:00Z'),
      triggerUpdatedAt: '2026-10-28T09:31:44Z',
      runs: [run(40664999999, { status, conclusion: null, run_started_at: null, created_at: '2026-10-28T09:31:45Z' }), run(40664823334)],
    });
    assert.equal(verdict.skip, true, status);
  }
});

test('the evaluating run never defers to itself; cancelled runs never count', () => {
  const runs = [
    run(SELF, { status: 'in_progress', conclusion: null, run_started_at: '2026-10-28T09:40:00Z' }),
    run(1, { conclusion: 'cancelled', run_started_at: '2026-10-28T09:40:00Z' }),
    run(2, { status: 'queued', conclusion: 'cancelled' }),
  ];
  assert.equal(shouldDebounce({ ...base, triggerUpdatedAt: '2026-10-28T09:31:44Z', runs }).skip, false);
});

test('schedule/dispatch and a missing trigger timestamp never debounce', () => {
  const runs = [run(2, { run_started_at: '2026-10-28T09:40:00Z' })];
  assert.equal(shouldDebounce({ eventName: 'schedule', runId: SELF, runs, triggerUpdatedAt: undefined }).skip, false);
  assert.equal(shouldDebounce({ eventName: 'workflow_dispatch', runId: SELF, runs }).skip, false);
  assert.equal(shouldDebounce({ ...base, runs, triggerUpdatedAt: undefined }).skip, false);
});

test('a merger run that started exactly at the trigger time has not necessarily seen it: not debounced', () => {
  const verdict = shouldDebounce({ ...base, triggerUpdatedAt: '2026-10-28T09:30:53Z', runs: [run(2, { run_started_at: '2026-10-28T09:30:53Z' })] });
  assert.equal(verdict.skip, false);
});

test('#5786: a completed run that was itself debounced never counts as having read state', () => {
  const verdict = shouldDebounce({
    ...base,
    triggerUpdatedAt: '2026-10-28T09:30:00Z',
    runs: [run(SELF, { status: 'in_progress', conclusion: null }), run(7, { readState: false, run_started_at: '2026-10-28T09:40:00Z' })],
  });
  assert.equal(verdict.skip, false);
  // Control: the same run, having read state, does count.
  assert.equal(shouldDebounce({ ...base, triggerUpdatedAt: '2026-10-28T09:30:00Z',
    runs: [run(7, { run_started_at: '2026-10-28T09:40:00Z' })] }).skip, true);
});

// Codex's interleaving, end to end through checkDebounce with a jobs fake:
// A holds the group queued for a runner; B (trigger completed 10:00) waits
// pending. A starts at 10:05 and sees B pending -> defers to B. B then runs and
// sees A completed, started after B's trigger — but A's merge step was skipped.
test('#5786: A defers to pending B, then B must NOT defer to A — the signal is not lost', async () => {
  const A = 101, B = 102;
  const jobsByRun = {
    [A]: [{ steps: [{ name: 'Debounce workflow_run triggers', conclusion: 'success' }, { name: 'Merge ready PRs', conclusion: 'skipped' }] }],
  };
  const make = (runId, triggerAt, runs, now) => {
    const outputs = {};
    return {
      outputs,
      args: {
        github: {
          paginate: async (_fn, params) => jobsByRun[params.run_id] || [],
          rest: { actions: {
            listWorkflowRuns: async () => ({ data: { workflow_runs: runs } }),
            listJobsForWorkflowRun: () => {},
          } },
        },
        core: { notice() {}, setOutput: (k, v) => { outputs[k] = v; } },
        context: { eventName: 'workflow_run', runId, repo: { owner: 'o', repo: 'r' },
          payload: { workflow_run: { updated_at: triggerAt } } },
        now,
      },
    };
  };
  // A's turn: B is pending.
  const a = make(A, '2026-10-28T09:59:00Z', [
    { id: B, status: 'pending', conclusion: null, created_at: '2026-10-28T10:00:00Z' },
    { id: A, status: 'in_progress', conclusion: null, run_started_at: '2026-10-28T10:05:00Z' },
  ], Date.parse('2026-10-28T10:05:00Z'));
  await checkDebounce(a.args);
  assert.equal(a.outputs.skip, 'true', 'A defers to the pending B');
  // B's turn: A finished (success) having started after B's trigger.
  const b = make(B, '2026-10-28T10:00:00Z', [
    { id: B, status: 'in_progress', conclusion: null, run_started_at: '2026-10-28T10:06:00Z' },
    { id: A, status: 'completed', conclusion: 'success', run_started_at: '2026-10-28T10:05:00Z' },
  ], Date.parse('2026-10-28T10:06:00Z'));
  await checkDebounce(b.args);
  assert.equal(b.outputs.skip, 'false', 'B must proceed: A read nothing');
});

test('#5786: a run whose merge step ran counts; a jobs-read failure proceeds rather than skips', async () => {
  const mk = (jobs) => {
    const outputs = {};
    return { outputs, args: {
      github: { paginate: async () => { if (jobs instanceof Error) throw jobs; return jobs; },
        rest: { actions: { listWorkflowRuns: async () => ({ data: { workflow_runs: [
          { id: 1, status: 'in_progress', conclusion: null, run_started_at: '2026-10-28T10:06:00Z' },
          { id: 2, status: 'completed', conclusion: 'success', run_started_at: '2026-10-28T10:05:00Z' },
        ] } }), listJobsForWorkflowRun: () => {} } } },
      core: { notice() {}, setOutput: (k, v) => { outputs[k] = v; } },
      context: { eventName: 'workflow_run', runId: 1, repo: { owner: 'o', repo: 'r' },
        payload: { workflow_run: { updated_at: '2026-10-28T10:00:00Z' } } },
    } };
  };
  const ran = mk([{ steps: [{ name: 'Merge ready PRs', conclusion: 'success' }] }]);
  await checkDebounce(ran.args);
  assert.equal(ran.outputs.skip, 'true');
  const boom = mk(new Error('502'));
  await checkDebounce(boom.args);
  assert.equal(boom.outputs.skip, 'false');
});

// #7427: a pending run suppresses a trigger only while it can still start.
// Run 43919350620 sat `queued` for over an hour with no jobs and could not be
// cancelled (HTTP 409), and every workflow_run-triggered merger run deferred
// to it; only manual dispatches could merge. Freshness is measured from the
// pending run's created_at against an injected `now` (epoch ms) — never the
// wall clock — so the bound is exact in these tests.

test('#7427: a pending run wedged past the bound no longer debounces', () => {
  const now = Date.parse('2026-11-16T18:40:00Z');
  for (const status of ['queued', 'waiting', 'pending', 'requested']) {
    const wedged = run(43919350620, {
      status, conclusion: null,
      run_started_at: null, created_at: '2026-11-16T17:31:59Z',
    });
    const verdict = shouldDebounce({ ...base, now, triggerUpdatedAt: '2026-11-16T18:35:00Z', runs: [wedged] });
    assert.equal(verdict.skip, false, status);
  }
});

test('#7427: a fresh pending run still debounces', () => {
  const now = Date.parse('2026-11-16T18:40:00Z');
  for (const status of ['queued', 'waiting', 'pending', 'requested']) {
    const fresh = run(43, {
      status, conclusion: null,
      run_started_at: null, created_at: '2026-11-16T18:39:58Z',
    });
    const verdict = shouldDebounce({ ...base, now, triggerUpdatedAt: '2026-11-16T18:35:00Z', runs: [fresh] });
    assert.equal(verdict.skip, true, status);
  }
});

test('#7427: exactly at the bound still debounces; one ms past it does not', () => {
  const now = Date.parse('2026-11-16T18:40:00Z');
  const atBound = run(43, {
    status: 'queued', conclusion: null,
    run_started_at: null, created_at: new Date(now - PENDING_RUN_MAX_AGE_MS).toISOString(),
  });
  const pastBound = run(44, {
    status: 'queued', conclusion: null,
    run_started_at: null, created_at: new Date(now - PENDING_RUN_MAX_AGE_MS - 1).toISOString(),
  });
  const trigger = '2026-11-16T18:35:00Z';
  assert.equal(shouldDebounce({ ...base, now, triggerUpdatedAt: trigger, runs: [atBound] }).skip, true);
  assert.equal(shouldDebounce({ ...base, now, triggerUpdatedAt: trigger, runs: [pastBound] }).skip, false);
});

test('#7427: a wedged pending run hides nothing behind it — a later qualifying run still counts', () => {
  const now = Date.parse('2026-11-16T18:40:00Z');
  const wedged = run(43919350620, {
    status: 'queued', conclusion: null,
    run_started_at: null, created_at: '2026-11-16T17:31:59Z',
  });
  const reader = run(7, { run_started_at: '2026-11-16T18:36:00Z' });
  const verdict = shouldDebounce({ ...base, now, triggerUpdatedAt: '2026-11-16T18:35:00Z', runs: [wedged, reader] });
  assert.equal(verdict.skip, true);
  assert.match(verdict.reason, /run 7 started/);
});

test('#7427: run 43919350620 — queued over an hour, uncancellable — must not debounce the trigger (end to end)', async () => {
  const outputs = {};
  await checkDebounce({
    github: {
      paginate: async () => [],
      rest: { actions: {
        listWorkflowRuns: async () => ({ data: { workflow_runs: [
          { id: SELF, status: 'in_progress', conclusion: null, run_started_at: '2026-11-16T18:38:00Z' },
          { id: 43919350620, status: 'queued', conclusion: null, run_started_at: null, created_at: '2026-11-16T17:31:59Z' },
        ] } }),
        listJobsForWorkflowRun: () => {},
      } },
    },
    core: { notice() {}, setOutput: (k, v) => { outputs[k] = v; } },
    context: { eventName: 'workflow_run', runId: SELF, repo: { owner: 'o', repo: 'r' },
      payload: { workflow_run: { updated_at: '2026-11-16T18:35:00Z' } } },
    now: Date.parse('2026-11-16T18:40:00Z'),
  });
  assert.equal(outputs.skip, 'false');
});
