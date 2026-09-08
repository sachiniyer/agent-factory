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
      assert.ok(fn, "an uncertain ticket must have an armed reconciliation timer");
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
  test(`${name} retains the restore fence until the row advances`, async () => {
    const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain);
    await assert.rejects(pending.run("session", async () => { throw error; }, true)!, error);
    assert.equal(pending.has("session"), true);
    pending.observe([{ id: "session", restoreEligible: true }]);
    assert.equal(pending.has("session"), true);
    pending.observe([{ id: "session", restoreEligible: false, restoreInFlight: true }], {
      kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
    });
    assert.equal(pending.has("session"), true);
    pending.observe([{ id: "session", restoreEligible: true }], {
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
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain);
  let reject!: (error: unknown) => void;
  const request = pending.run("session", () => new Promise<void>((_, fail) => { reject = fail; }), true);
  pending.observe([{ id: "session", restoreEligible: true }]);
  reject(new ApiError(0, "connection lost"));
  await assert.rejects(request!, /connection lost/);
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: false, restoreInFlight: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
});

test("a queued uncertain restore ignores an eligible Snapshot until busy then restorable", async () => {
  let now = 1_000;
  const pending = new PendingRestores(() => {}, isMutationOutcomeUncertain, () => false, () => now);
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  pending.reset();
  assert.equal(pending.has("session"), true);
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true, "the daemon may still be queued on its operation lock");
  pending.observe([{ id: "session", restoreEligible: false, restoreInFlight: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  now += 1;
  pending.observe([{ id: "session", restoreEligible: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
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

test("wall-clock suspension cannot expire a monotonic admission deadline", async t => {
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
  assert.equal(pending.has("session"), true, "wall time alone cannot advance the daemon's lock deadline");

  monotonicNow += 30_000 + RESTORE_ADMISSION_MARGIN_MS + 1;
  pending.observe(rows, {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), false);
});

test("an uncertain restore distinguishes the restoring projection from a settled live row", async () => {
  let now = 1_000;
  const timer = fakeRestoreTimer();
  const pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => now, () => {}, timer.schedule, timer.cancel,
  );
  pending.observe([{ id: "session", restoreEligible: true, restoreInFlight: false }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);

  pending.observe([{ id: "session", restoreEligible: false, restoreInFlight: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  assert.equal(pending.has("session"), true, "lifecycle None while OpRestoring remains fenced");
  assert.equal(timer.armed(), true);

  now += 1;
  pending.observe([{ id: "session", restoreEligible: false, restoreInFlight: false }], {
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

test("early release and reset cancel an uncertain ticket's reconciliation timer", async () => {
  const timer = fakeRestoreTimer();
  const rows = [{ id: "session", restoreEligible: true }];
  const pending = new PendingRestores(
    () => {}, isMutationOutcomeUncertain, () => false, () => 1_000, () => {}, timer.schedule, timer.cancel,
  );
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000 });
  await assert.rejects(pending.run("session", async () => { throw new ApiError(0, "lost reply"); }, true)!);
  assert.equal(timer.armed(), true);
  pending.observe([{ id: "session", restoreEligible: false, restoreInFlight: true }], {
    kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000,
  });
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot(), operationLockTimeoutMs: 30_000 });
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
