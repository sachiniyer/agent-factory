import type { Page } from "@playwright/test";

/**
 * Opens a fresh SPA page only after both startup Snapshots have settled:
 *
 *  1. the seed Snapshot used to authenticate and populate the rail; and
 *  2. the events socket's first-open resync, which closes the seed-to-subscribe gap.
 *
 * The second response is registered before `open` starts, so a fast loopback daemon
 * cannot win the observation race. `finished()` waits for the body on the wire; the
 * no-op evaluation then crosses a browser task boundary, after the fetch promise's
 * body/applySessions microtasks have drained.
 */
export async function openAfterInitialResync(page: Page, open: () => Promise<void>): Promise<void> {
  let successfulSnapshots = 0;
  const initialResync = page.waitForResponse((response) => {
    let path: string;
    try {
      path = new URL(response.url()).pathname;
    } catch {
      return false;
    }
    if (path !== "/v1/Snapshot" || response.request().method() !== "POST" || response.status() !== 200) {
      return false;
    }
    successfulSnapshots += 1;
    return successfulSnapshots === 2;
  });

  await open();
  const response = await initialResync;
  await response.finished();
  await page.evaluate(() => undefined);
}
