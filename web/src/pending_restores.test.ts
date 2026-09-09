import assert from "node:assert/strict";
import { test } from "node:test";
import { ApiError, isMutationCommittedError, isMutationOutcomeUncertain, restoreSession } from "./api.js";
import {
  PendingRestores,
  RESTORE_ADMISSION_MARGIN_MS,
  RESTORE_RECONCILE_RETRY_MIN_MS,
} from "./pending_restores.js";

function fakeRestoreTimer() {
  let callback: (() => void) | null = null;
  let delay = -1;
  let cancellations = 0;
  return {
    schedule(fn: () => void, delayMs: number): ReturnType<typeof globalThis.setTimeout> {
      callback = fn;
      delay = delayMs;
      return 1 as unknown as ReturnType<typeof globalThis.setTimeout>;
    },
    cancel(): void {
      callback = null;
      cancellations++;
    },
    fire(): void {
      const fn = callback;
      callback = null;
      assert.ok(fn, "a pending restore must have an armed reconciliation timer");
      fn();
    },
    delay: () => delay,
    cancellations: () => cancellations,
    armed: () => callback !== null,
  };
}

test("two Restore clicks send one request without a failure modal; the advanced row releases the fence", async t => {
  let release!: () => void;
  const response = new Promise<void>(resolve => { release = resolve; });
  let requests = 0;
  let failureModals = 0;
  let progressModals = 0;
  let visiblePending: ReadonlySet<string> = new Set();
  t.mock.method(globalThis, "fetch", async (url: string) => {
    assert.match(url, /\/RestoreSession$/);
    requests++;
    await response;
    return new Response(JSON.stringify({ data: {} }));
  });
  const pending = new PendingRestores(ids => { visiblePending = ids; });
  const click = () => pending.run("session", () => {
    progressModals++;
    return restoreSession("session", "title", "").catch(() => { failureModals++; });
  }, true);
  const first = click();
  assert.equal(click(), null);
  assert.equal(requests, 1);
  assert.equal(progressModals, 1);
  assert.equal(failureModals, 0);
  assert.equal(visiblePending.has("session"), true);
  release();
  await first;
  assert.equal(pending.has("session"), true);
  assert.equal(click(), null);
  pending.observe([{ id: "session", restoreEligible: true }]);
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: false }], { kind: "updated", id: "session" });
  assert.equal(pending.has("session"), false);
  assert.equal(visiblePending.size, 0);
  await click();
  assert.equal(requests, 2);
  assert.equal(failureModals, 0);
});

test("definitive restore refusal releases only its session fence", async () => {
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain);
  let release!: () => void;
  const other = pending.run("other", () => new Promise<void>(resolve => { release = resolve; }), true);
  await assert.rejects(pending.run("failed", async () => { throw new ApiError(409, "refused", "", true); }, true)!, /refused/);
  assert.equal(pending.has("failed"), false);
  assert.equal(pending.has("other"), true);
  release();
  await other;
});

test("reset preserves an in-flight restore until a post-response Snapshot", async () => {
  const pending = new PendingRestores(() => {});
  let releaseOld!: () => void;
  let releaseNew!: () => void;
  const old = pending.run("session", () => new Promise<void>(resolve => { releaseOld = resolve; }), true);
  pending.reset();
  const current = pending.run("session", () => new Promise<void>(resolve => { releaseNew = resolve; }), true);
  releaseOld();
  await old;
  assert.equal(pending.has("session"), true);
  releaseNew();
  await current;
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: false }]);
  assert.equal(pending.has("session"), false);
});


test("successful restore survives failed resync and releases on absence or reset", async () => {
  const pending = new PendingRestores(() => {});
  await pending.run("session", async () => {}, true);
  // A failed Snapshot never calls observe: the archived projection remains fenced.
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: true }]);
  assert.equal(pending.has("session"), true);
  pending.observe([]);
  assert.equal(pending.has("session"), false);
  pending.reset();
  await pending.run("session", async () => {}, true);
  assert.equal(pending.has("session"), true);
  pending.reset();
  assert.equal(pending.has("session"), false);
});

test("successful restore retries reconciliation after its first Snapshot fails", async () => {
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  let reconciliations = 0;
  let pending!: PendingRestores;
  pending = new PendingRestores(
    () => {}, () => false, () => false, () => 1_000,
    () => {
      reconciliations++;
      if (reconciliations === 1) return; // The immediate post-success Snapshot failed.
      pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
    },
    timer.schedule,
    timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });

  await pending.run("session", async () => {}, true);
  assert.equal(pending.has("session"), true);
  assert.equal(reconciliations, 1);
  assert.equal(timer.armed(), true, "settled tickets must retry without an events socket");
  assert.equal(timer.delay(), RESTORE_RECONCILE_RETRY_MIN_MS);

  timer.fire();
  assert.equal(reconciliations, 2);
  assert.equal(pending.has("session"), false);
  assert.equal(timer.armed(), false);
});

test("a settled restore does not overlap a slow reconciliation", async () => {
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  let reconciliations = 0;
  let finishReconciliation!: () => void;
  const pending = new PendingRestores(
    () => {}, () => false, () => false, () => 1_000,
    () => {
      reconciliations++;
      return new Promise<void>(resolve => { finishReconciliation = resolve; });
    },
    timer.schedule,
    timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });

  await pending.run("session", async () => {}, true);
  assert.equal(reconciliations, 1, "success must start its confirming reconciliation");
  assert.equal(timer.armed(), false, "a slow Snapshot must not race a retry timer");

  // The accepted response commits its Snapshot before the resync promise settles.
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  finishReconciliation();
  await Promise.resolve();
  assert.equal(pending.has("session"), false);
  assert.equal(reconciliations, 1);
  assert.equal(timer.armed(), false);
});

test("a failed settled reconciliation arms the next backoff attempt", async () => {
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  let reconciliations = 0;
  const pending = new PendingRestores(
    () => {}, () => false, () => false, () => 1_000,
    async () => { reconciliations++; }, // requestResync swallows a failed Snapshot.
    timer.schedule,
    timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });

  await pending.run("session", async () => {}, true);
  await Promise.resolve();
  assert.equal(reconciliations, 1);
  assert.equal(timer.armed(), true, "a failed reconciliation must arm another attempt");
  assert.equal(timer.delay(), RESTORE_RECONCILE_RETRY_MIN_MS);

  timer.fire();
  await Promise.resolve();
  assert.equal(reconciliations, 2);
  assert.equal(timer.armed(), true);
});

test("prompt confirmation of a successful restore cancels its reconciliation retry", async () => {
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  const pending = new PendingRestores(
    () => {}, () => false, () => false, () => 1_000, () => {}, timer.schedule, timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });

  await pending.run("session", async () => {}, true);
  assert.equal(timer.armed(), true);
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  assert.equal(pending.has("session"), false);
  assert.equal(timer.armed(), false);
  assert.equal(timer.cancellations(), 1);
});

test("row advancement before the response releases only after success", async () => {
  const pending = new PendingRestores(() => {});
  let release!: () => void;
  const request = pending.run("session", () => new Promise<void>(resolve => { release = resolve; }), true);
  pending.observe([{ id: "session", restoreEligible: false }]);
  assert.equal(pending.has("session"), true);
  release();
  await request;
  assert.equal(pending.has("session"), false);
});

for (const [name, error] of [
  ["committed warning", new ApiError(200, "committed", "mutation_committed")],
  ["transport failure", new ApiError(0, "connection lost")],
] as const) {
  test(`${name} retains the restore fence until a positively settled row`, async () => {
    let now = 1_000;
    const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
    await assert.rejects(pending.run("session", async () => { throw error; }, true)!, error);
    assert.equal(pending.has("session"), true);
    pending.observe([{ id: "session", restoreEligible: true }]);
    assert.equal(pending.has("session"), true);
    pending.observe([{ id: "session", restoreEligible: false, restoreSettled: false }], {
      kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
    });
    assert.equal(pending.has("session"), true);
    pending.observe([{ id: "session", restoreEligible: true }], {
      kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
    });
    assert.equal(pending.has("session"), true, "a busy-to-restorable cycle may belong to another attempt");
    now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
    pending.observe([{ id: "session", restoreEligible: false, restoreSettled: true }], {
      kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
    });
    assert.equal(pending.has("session"), false);
  });
}

for (const status of ["lost", "dead"]) {
  test(`${status} stays fenced after success until its lifecycle state changes`, async () => {
    const pending = new PendingRestores(() => {});
    pending.observe([{ id: "session", restoreEligible: true }]);
    await pending.run("session", async () => {}, true);
    assert.equal(pending.has("session"), true);
    pending.observe([{ id: "session", restoreEligible: true }]);
    assert.equal(pending.has("session"), true);
    pending.observe([{ id: "session", restoreEligible: false }]);
    assert.equal(pending.has("session"), false);
  });
}

test("Dead to Lost normalization does not settle an uncertain restore", async () => {
  let now = 1_000;
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
  let reject!: (error: unknown) => void;
  const request = pending.run("session", () => new Promise<void>((_, fail) => { reject = fail; }), true);
  pending.observe([{ id: "session", restoreEligible: true }]);
  reject(new ApiError(0, "connection lost"));
  await assert.rejects(request!, /connection lost/);
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: false }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true);
  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
});

test("a queued uncertain restore ignores busy-to-restorable projections until its admission deadline", async () => {
  let now = 1_000;
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  pending.reset();
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true, "the daemon may still be queued on its operation lock");
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: false }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  now += 1;
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true, "an uncorrelated busy cycle cannot shorten B's deadline");
  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
});

test("a competing restore's busy gap cannot release the current uncertain attempt", async () => {
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => 1_000);
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost B reply"); }, true)!);

  // Attempt A owns the daemon lock while this browser's attempt B is queued.
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: false }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  // Capture A's brief restorable projection after A fails, but hold its delivery.
  const gapSnapshot = pending.beginSnapshot();
  // B then raises its own fence. The delayed A projection cannot describe B.
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: false }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: gapSnapshot, operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true, "another attempt's busy cycle cannot release B's fence");
});

test("a competing restore's settled row cannot release a queued uncertain attempt early", async () => {
  let now = 1_000;
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost B reply"); }, true)!);

  // Attempt A owns the daemon lock while this browser's attempt B is queued.
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: false }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  // Recover can clear A's restore fence before A releases the operation lock.
  // This settled projection therefore cannot be correlated with queued attempt B.
  now += 1;
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true, "attempt A's settled row cannot release B's fence");
  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS;
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false, "B can no longer be queued after its admission deadline");
});

test("a never-admitted uncertain restore releases after the daemon admission bound", async () => {
  let now = 1_000;
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
  const stale = pending.beginSnapshot();
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  pending.reset();
  const rows = [{ id: "session", restoreEligible: true }];
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true);
  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe(rows, { kind: "snapshot", generation: stale, operationLockTimeoutMs: 30_000 });
  assert.equal(pending.has("session"), true, "elapsed time cannot make a pre-uncertainty Snapshot causal");
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
});

test("an older-daemon Snapshot without an admission bound still releases the ticket", async () => {
  let now = 1_000;
  const rows = [{ id: "session", restoreEligible: true }];
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  assert.equal(pending.has("session"), true);

  // Pre-projection daemons bounded manual restore admission at 30 seconds.
  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  assert.equal(pending.has("session"), false, "version skew must not strand the restore fence");
});

test("deadline expiry uses Snapshot issuance time rather than delayed application time", async () => {
  let now = 1_000;
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  const pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => now, () => {}, timer.schedule, timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000 });
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS - 1;
  const issuedBeforeDeadline = pending.beginSnapshot();
  now += 5_000; // Slow task/project loading delays the commit of the captured rows.
  pending.observe(rows, { kind: "snapshot", generation: issuedBeforeDeadline, operationLockTimeoutMs: 30_000 });
  assert.equal(pending.has("session"), true, "late processing cannot make an early Snapshot causal");

  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
});

test("wall-clock jumps do not affect the legacy performance-clock fallback", async t => {
  let wallNow = 1_000;
  let monotonicNow = 1_000;
  t.mock.method(Date, "now", () => wallNow);
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  const pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => monotonicNow, () => {}, timer.schedule, timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000 });
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

  wallNow += 60_000;
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true, "wall time alone cannot advance the fallback deadline");

  monotonicNow += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
});

test("a remote browser running ahead cannot expire the daemon admission fence", async () => {
  let browserNow = 1_000;
  let daemonNow = 1_000;
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  const pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => browserNow, () => {}, timer.schedule, timer.cancel,
  );
  const snapshot = () => ({
    kind: "snapshot" as const,
    generation: pending.beginSnapshot(),
    operationLockTimeoutMs: 30_000,
    operationClockMs: daemonNow,
  });
  pending.observe(rows, snapshot());
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

  browserNow += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  daemonNow += 1;
  pending.observe(rows, snapshot());
  assert.equal(pending.has("session"), true, "browser-ahead time cannot release a daemon-side fence");

  daemonNow += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe(rows, snapshot());
  assert.equal(pending.has("session"), false, "daemon elapsed time eventually releases the fence");
});

test("an admitted restore stays fenced until its lock ownership reaches the projection", async () => {
  let daemonNow = 1_000;
  const timer = fakeRestoreTimer();
  const pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => 1_000, () => {}, timer.schedule, timer.cancel,
  );
  const snapshot = () => ({
    kind: "snapshot" as const,
    generation: pending.beginSnapshot(),
    operationLockTimeoutMs: 30_000,
    operationClockMs: daemonNow,
  });
  const row = (operationLockHeld: boolean, restoreEligible: boolean, restoreSettled = false) => [{
    id: "session", restoreEligible, restoreSettled, operationLockHeld,
  }];
  pending.observe(row(false, true), snapshot());
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

  daemonNow += 1;
  pending.observe(row(true, true), snapshot()); // Admitted, but OpRestoring is not projected yet.
  daemonNow += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe(row(true, true), snapshot());
  assert.equal(pending.has("session"), true, "deadline alone cannot release an admitted restore");

  pending.observe(row(true, false), snapshot());
  assert.equal(pending.has("session"), true, "the projected restore remains fenced while it owns the lock");
  pending.observe(row(false, false, true), snapshot());
  assert.equal(pending.has("session"), false, "the settled projection and free lock release the fence");
});

for (const state of ["OpArchiving", "startup-unknown"] as const) {
  test(`an uncertain restore stays fenced through the ${state} no-action projection`, async () => {
    const timer = fakeRestoreTimer();
    const pending = new PendingRestores(
      () => {}, isMutationOutcomeUncertain, () => false, () => 1_000, () => {}, timer.schedule, timer.cancel,
    );
    pending.observe([{ id: "session", restoreEligible: true }], {
      kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
    });
    await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

    // Both projections have LifecycleActionNone for reasons other than a settled
    // restore. Absence of positive completion evidence must fail closed.
    pending.observe([{ id: "session", restoreEligible: false }], {
      kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
    });
    assert.equal(pending.has("session"), true);
    assert.equal(timer.armed(), true);
  });
}

test("an uncertain restore distinguishes the restoring projection from a settled live row", async () => {
  let now = 1_000;
  const timer = fakeRestoreTimer();
  const pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => now, () => {}, timer.schedule, timer.cancel,
  );
  pending.observe([{ id: "session", restoreEligible: true, restoreSettled: false }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: false }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true, "lifecycle None while OpRestoring remains fenced");
  assert.equal(timer.armed(), true);

  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false, "an archivable live row proves the restore settled");
  assert.equal(timer.armed(), false, "completion cancels reconciliation retries");
});

test("an early Snapshot schedules reconciliation at the uncertain admission deadline", async () => {
  let now = 1_000;
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  let reconciliations = 0;
  let pending!: PendingRestores;
  pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => now,
    () => {
      reconciliations++;
      pending.observe(rows, {
        kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
      });
    },
    timer.schedule,
    timer.cancel,
  );
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  assert.equal(timer.armed(), false, "no daemon admission bound has been observed yet");
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000 });
  assert.equal(pending.has("session"), true);
  assert.equal(timer.delay(), 30_000 + RESTORE_ADMISSION_MARGIN_MS);

  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  timer.fire();
  assert.equal(reconciliations, 1);
  assert.equal(pending.has("session"), false);
});

test("an admitted restore backs off reconciliation after its admission deadline", async () => {
  let daemonNow = 1_000;
  const timer = fakeRestoreTimer();
  let rows = [{ id: "session", restoreEligible: true, operationLockHeld: false }];
  let reconciliations = 0;
  let pending!: PendingRestores;
  const snapshot = () => ({
    kind: "snapshot" as const,
    generation: pending.beginSnapshot(),
    operationLockTimeoutMs: 30_000,
    operationClockMs: daemonNow,
  });
  pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => 1_000,
    () => {
      reconciliations++;
      pending.observe(rows, snapshot());
    },
    timer.schedule,
    timer.cancel,
  );
  pending.observe(rows, snapshot());
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

  timer.fire(); // Establish the first causal daemon-clock reading.
  assert.equal(timer.delay(), 30_000 + RESTORE_ADMISSION_MARGIN_MS);
  daemonNow += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  rows = [{ id: "session", restoreEligible: false, operationLockHeld: true }];
  timer.fire();
  assert.equal(reconciliations, 2);
  assert.equal(pending.has("session"), true);
  assert.equal(timer.delay(), RESTORE_RECONCILE_RETRY_MIN_MS,
    "an admitted long-running restore must use the retry backoff, not a zero-delay loop");
});

test("a failed deadline resync retries until an accepted Snapshot releases the ticket", async () => {
  let now = 1_000;
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  let reconciliations = 0;
  let pending!: PendingRestores;
  pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => now,
    () => {
      reconciliations++;
      if (reconciliations === 1) return; // The best-effort REST resync failed.
      pending.observe(rows, {
        kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
      });
    },
    timer.schedule,
    timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000 });
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  timer.fire();
  assert.equal(reconciliations, 1);
  assert.equal(pending.has("session"), true);
  assert.equal(timer.armed(), true);
  assert.equal(timer.delay(), RESTORE_RECONCILE_RETRY_MIN_MS);

  now += RESTORE_RECONCILE_RETRY_MIN_MS;
  timer.fire();
  assert.equal(reconciliations, 2);
  assert.equal(pending.has("session"), false);
  assert.equal(timer.armed(), false, "release must leave no reconciliation retry behind");
});

test("deadline release and reset cancel an uncertain ticket's reconciliation timer", async () => {
  let now = 1_000;
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  const pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => now, () => {}, timer.schedule, timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000 });
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  assert.equal(timer.armed(), true);
  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe([{ id: "session", restoreEligible: false, restoreSettled: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
  assert.equal(timer.armed(), false);
  assert.equal(timer.cancellations(), 1);

  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  assert.equal(timer.armed(), true);
  pending.reset();
  assert.equal(timer.armed(), false);
  assert.equal(timer.cancellations(), 2);
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  assert.equal(timer.armed(), true, "the surviving ticket must rearm from the replacement connection's Snapshot");
  pending.reset();
  assert.equal(timer.cancellations(), 3);
});

test("a delayed still-eligible update cannot settle a successful Lost restore", async () => {
  const pending = new PendingRestores(() => {});
  const lost = [{ id: "session", restoreEligible: true }];
  pending.observe(lost, { kind: "snapshot", generation: 0 });
  await pending.run("session", async () => {}, true);
  pending.observe(lost); // Repainting the pre-response cache is not fresh evidence.
  pending.observe(lost, { kind: "snapshot", generation: 0 }); // Reusing its stamp is not a new observation either.
  assert.equal(pending.has("session"), true);
  pending.observe(lost, { kind: "updated", id: "another-session" });
  assert.equal(pending.has("session"), true);
  pending.observe(lost, { kind: "updated", id: "session" });
  assert.equal(pending.has("session"), true);
  pending.observe(lost, { kind: "snapshot", generation: pending.beginSnapshot() }); // A fenced follow-up Snapshot can settle it.
  assert.equal(pending.has("session"), false);
});

test("a post-response Snapshot releases success; uncertainty waits out admission", async () => {
  let now = 1_000;
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
  const rows = ["success", "uncertain"].map(id => ({ id, restoreEligible: true }));
  pending.observe(rows, { kind: "snapshot", generation: 0 });
  await pending.run("success", async () => {}, true);
  await assert.rejects(pending.run("uncertain", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("success"), false);
  assert.equal(pending.has("uncertain"), true);
  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("uncertain"), false);
});

test("a committed warning also settles on a fresh restorable projection", async () => {
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, isMutationCommittedError);
  const rows = [{ id: "session", restoreEligible: true }];
  pending.observe(rows, { kind: "snapshot", generation: 0 });
  await assert.rejects(pending.run("session", async () => {
    throw new ApiError(200, "completed with warning", "mutation_committed");
  }, true)!);
  pending.observe(rows);
  assert.equal(pending.has("session"), true);
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  assert.equal(pending.has("session"), false);
});


test("a Snapshot issued before success cannot settle even when delivered afterward", async () => {
  const pending = new PendingRestores(() => {});
  const rows = [{ id: "session", restoreEligible: true }];
  let release!: () => void;
  const request = pending.run("session", () => new Promise<void>(resolve => { release = resolve; }), true);
  const stale = pending.beginSnapshot();
  release();
  await request;
  pending.observe(rows, { kind: "snapshot", generation: stale });
  assert.equal(pending.has("session"), true);
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  assert.equal(pending.has("session"), false);
});

for (const beforeResponse of [false, true]) {
  test(`identity-only restored event waits for a causal Snapshot (before response=${beforeResponse})`, async () => {
    const pending = new PendingRestores(() => {});
    const rows = [{ id: "session", restoreEligible: true }];
    let release!: () => void;
    const request = pending.run("session", () => new Promise<void>(resolve => { release = resolve; }), true);
    if (!beforeResponse) { release(); await request; }
    pending.observe(rows, { kind: "restored", id: "other-session" });
    assert.equal(pending.has("session"), true);
    pending.observe(rows, { kind: "restored", id: "session" });
    if (beforeResponse) {
      assert.equal(pending.has("session"), true);
      release();
      await request;
    }
    assert.equal(pending.has("session"), true);
    pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
    assert.equal(pending.has("session"), false);
  });
}


test("a delayed restored event from attempt A cannot complete uncertain attempt B", async () => {
  let now = 1_000;
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
  const rows = [{ id: "session", restoreEligible: true }];
  await pending.run("session", async () => {}, true);
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  let reject!: (error: unknown) => void;
  const next = pending.run("session", () => new Promise<void>((_, fail) => { reject = fail; }), true);
  pending.observe(rows, { kind: "restored", id: "session" });
  reject(new ApiError(0, "lost B reply"));
  await assert.rejects(next!);
  // An identity-only event has no attempt marker and may belong to attempt A.
  pending.observe(rows, { kind: "restored", id: "session" });
  assert.equal(pending.has("session"), true);
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true);
  now += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
});
