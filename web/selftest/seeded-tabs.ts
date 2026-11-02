import { expect, test, type Page } from "@playwright/test";

// Shared-session tab hygiene (#4217, seeded-roster pollution). The suite runs
// serially but the seeded sessions (probe-a, probe-b, ...) are shared: a tab a
// spec creates stays on the session for every spec that runs after it, so an
// absolute roster assertion written on a clean session rots the moment any
// spec upstream forgets to clean up — and a spec that cleans up only when it
// passes still leaks on failure.
//
// seededTabs() makes cleanup structural rather than remembered: call
// enter(page) once the session's tab bar has rendered, and the afterEach it
// registers closes every tab the test added — the same ×→Delete path the
// specs themselves use — even when the test body fails. The entry count it
// returns is also the index of the first tab the test can safely create and
// drag: added tabs append at the end of the roster.
//
//   const roster = seededTabs();
//   test("...", async ({ page }) => {
//     await page.goto(`/#/session/${id}`);
//     await page.locator("#app[data-af-resync-settled]").waitFor();
//     const base = await roster.enter(page);
//     ...
//   });
//
// The entry model is positional, matching the app's own invariant: seeded
// tabs occupy the first `base` slots and created tabs append after them, so a
// spec that reorders or closes a seeded tab must not use this helper (none
// do; the agent tab at index 0 is unclosable anyway).

export function seededTabs() {
  const entered = new Map<Page, number>();

  test.afterEach(async ({ page }) => {
    const base = entered.get(page);
    entered.delete(page);
    if (base === undefined) {
      return;
    }
    // The cleanup path is the tab bar's ×→Delete flow — which phone widths
    // hide entirely (af-session-first drops .af-term-head under 768px), so
    // normalize the viewport before closing.
    await page.setViewportSize({ width: 1280, height: 800 });
    const tabs = page.locator(".af-tabbar .af-tab");
    while ((await tabs.count()) > base) {
      const have = await tabs.count();
      await tabs.nth(base).locator(".af-tab-close").click();
      await page.getByRole("button", { name: "Delete tab", exact: true }).click();
      await expect(tabs).toHaveCount(have - 1);
    }
  });

  return {
    /** Records the roster count and returns it — the index at which the test's
     *  first created tab will land. Call after the session is rendered. */
    enter: async (page: Page): Promise<number> => {
      const base = await page.locator(".af-tabbar .af-tab").count();
      entered.set(page, base);
      return base;
    },
  };
}
