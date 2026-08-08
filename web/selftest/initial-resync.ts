import type { Page } from "@playwright/test";

/**
 * Opens a fresh SPA page only after the events socket's first-open resync has been
 * accepted and applied. A response count is insufficient: an event crossing an
 * in-flight Snapshot makes the application discard it and schedule another fenced
 * request. The marker is persistent, so waiting after `open` cannot miss a fast
 * resync that settled while the seed Snapshot was rendering the rail.
 */
export async function openAfterInitialResync(page: Page, open: () => Promise<void>): Promise<void> {
  await open();
  await page.locator("#app[data-af-resync-settled]").waitFor({ state: "attached" });
}
