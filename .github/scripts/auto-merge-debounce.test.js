const assert = require('node:assert/strict');
const test = require('node:test');
const { shouldDebounce } = require('./auto-merge.js');

const SELF = 40664944064;
const base = { eventName: 'workflow_run', runId: SELF };
const run = (id, over = {}) => ({ id, status: 'completed', conclusion: 'success', run_started_at: '2026-10-28T09:30:53Z', ...over });

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
