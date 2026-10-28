import type { BrowserContext } from "@playwright/test";

/** Stop page-owned polling handlers before their context closes (#4080). */
export async function stopPolledRoutes(context: BrowserContext): Promise<void> {
  // Removing a route alone leaves its in-flight callbacks alive. Ignore their
  // teardown errors without waiting forever for a stalled upstream fetch.
  await Promise.all(context.pages().map(page => page.unrouteAll({ behavior: "ignoreErrors" })));
}
