import { test } from "node:test";
import assert from "node:assert/strict";

import type { Page } from "@playwright/test";
import { openAfterInitialResync } from "../selftest/initial-resync.js";

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

test("#3081 setup stays closed until the event stream's resync is accepted", async () => {
  let settledSelector = "";
  const accepted = deferred<void>();
  const page = {
    locator: (selector: string) => {
      settledSelector = selector;
      return { waitFor: () => accepted.promise };
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

  // `open` may have observed any number of successful HTTP responses. None can
  // release the helper until the application says a fenced response reached its
  // store; an event-crossed response is deliberately discarded before that marker.
  await new Promise<void>((resolve) => setImmediate(resolve));
  assert.equal(settled, false, "a successful but discarded Snapshot must not open the mutation window");
  accepted.resolve(undefined);
  await opening;

  assert.equal(settled, true);
  assert.equal(settledSelector, "#app[data-af-resync-settled]", "the test waits on the app acceptance marker");
});
