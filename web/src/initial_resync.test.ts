import { test } from "node:test";
import assert from "node:assert/strict";

import type { Page, Response } from "@playwright/test";
import { openAfterInitialResync } from "../selftest/initial-resync.js";

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function response(path: string, method = "POST", status = 200): Response {
  return {
    url: () => `http://af.test${path}`,
    request: () => ({ method: () => method }),
    status: () => status,
    finished: async () => null,
  } as unknown as Response;
}

test("#3081 setup stays closed until the event stream's first resync is consumed", async () => {
  let matches!: (candidate: Response) => boolean | Promise<boolean>;
  let evaluateCalls = 0;
  const matched = deferred<Response>();
  const page = {
    waitForResponse: (predicate: (candidate: Response) => boolean | Promise<boolean>) => {
      matches = predicate;
      return matched.promise;
    },
    evaluate: async () => {
      evaluateCalls += 1;
    },
  } as unknown as Page;
  const opened = deferred<void>();
  let settled = false;
  const opening = openAfterInitialResync(page, () => opened.promise).then(() => {
    settled = true;
  });

  await Promise.resolve();
  opened.resolve(undefined);
  await Promise.resolve();
  await Promise.resolve();

  assert.equal(settled, false, "the seed Snapshot alone must not open the mutation window");
  assert.equal(await matches(response("/v1/Snapshot", "GET")), false, "only the real POST is counted");
  assert.equal(await matches(response("/v1/Snapshot", "POST", 503)), false, "a failed resync closes no gap");
  assert.equal(await matches(response("/v1/ListTasks")), false, "another RPC is not a Snapshot");
  assert.equal(await matches(response("/v1/Snapshot")), false, "the seed Snapshot is response one");
  const initialResync = response("/v1/Snapshot");
  assert.equal(await matches(initialResync), true, "the event stream's first-open resync is response two");
  matched.resolve(initialResync);
  await opening;

  assert.equal(settled, true);
  assert.equal(evaluateCalls, 1, "one browser turn lets the resync body reach applySessions");
});
