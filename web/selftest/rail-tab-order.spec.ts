import { expect, test, type Page } from "@playwright/test";

// #6409: every rail row's actions button used to be its own Tab stop, so a
// keyboard user paid one Tab press per session to cross the rail — hundreds at
// fleet scale. The rail is now a roving tabindex: the SELECTED row is the stop,
// its in-row controls (actions trigger, backlog chip) the rest, and j/k moves
// both the selection and the DOM focus. Bounded stops however many rows there
// are, and every row's menu is still reachable: select the row, Tab, Enter.

const SYNTH_ROWS = 8;
const sessionA = process.env.AF_WEB_SESSION_A ?? "probe-a";

async function seedExtraRows(page: Page): Promise<void> {
  const response = await page.request.post("/v1/Snapshot", { data: { repo_id: "" } });
  expect(response.ok()).toBe(true);
  const original = await response.json();
  const instances = original.data.instances as Array<Record<string, unknown> & { title: string }>;
  const proto = { ...(instances.find((s) => s.title === sessionA) ?? {}) };
  // Clone the real record (same worktree/repo_path, so the rows land in the
  // scoped rail and keep its kill/lifecycle capabilities). created_at steps
  // forward so the synthetic rows sort predictably after the seeded ones.
  const base = Date.parse(String(proto.created_at ?? "2020-01-01T00:00:00Z"));
  for (let i = 1; i <= SYNTH_ROWS; i++) {
    instances.push({
      ...proto,
      id: `synth-tabstop-${i}`,
      title: `tabstop-${i}`,
      branch: `synth-tabstop-${i}`,
      created_at: new Date(base + i * 60_000).toISOString(),
    });
  }
  await page.route("**/v1/Snapshot", (route) =>
    route.fulfill({ json: { ...original, data: { ...original.data, instances } } }),
  );
}

test("#6409: the rail is a bounded number of Tab stops and every row menu stays reachable", async ({ page, request }) => {
  await seedExtraRows(page);
  // NOTE: `request` does not observe page.route, so this is the DAEMON's own
  // snapshot — enough to find probe-a's id; the rail's rendered row count comes
  // from the page.
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  const rows = page.locator(".af-rail-list .af-row");
  const rowCount = await rows.count();
  expect(rowCount, "the seeded + synthetic fleet rendered").toBeGreaterThan(SYNTH_ROWS);

  // Exactly one row (the selected one) is a stop; its controls ride along.
  // Everything else in the rail leaves the order — except a never-selectable
  // (aria-disabled) row, whose controls are its only keyboard route.
  const selectedChips = await page.locator(".af-rail-list .af-row.af-row-selected .af-backlog-session-chip").count();
  const inertControls = await page.locator(".af-rail-list .af-row[aria-disabled='true'] :is(.af-term-more, .af-backlog-session-chip)").count();
  const inertTriggers = await page.locator(".af-rail-list .af-row[aria-disabled='true'] .af-row-actions .af-term-more").count();
  const tabbable = await page.locator(".af-rail-list").evaluate((list) => ({
    rows: [...list.querySelectorAll<HTMLElement>(".af-row")].filter((r) => r.tabIndex === 0).length,
    triggers: [...list.querySelectorAll<HTMLElement>(".af-row-actions .af-term-more")].filter((t) => t.tabIndex === 0).length,
    chips: [...list.querySelectorAll<HTMLElement>(".af-backlog-session-chip")].filter((c) => c.tabIndex === 0).length,
  }));
  expect(tabbable.rows, "one rail row is the Tab stop").toBe(1);
  expect(tabbable.triggers, "one selected-row trigger plus each never-selectable row's").toBe(1 + inertTriggers);
  expect(tabbable.chips, "only the selected row's chips are stops").toBe(selectedChips);

  // The issue's repro: detach to navigation, then walk Tab from the top of the
  // page and count how many presses land inside the rail list.
  await page.keyboard.press("Control+]");
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  const stops: string[] = [];
  let railStops = 0;
  let reachedPane = false;
  for (let i = 0; i < 40 && !reachedPane; i++) {
    await page.keyboard.press("Tab");
    const stop = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      return {
        rail: !!el?.closest(".af-rail-list"),
        pane: !!el?.closest(".af-tabbar, .af-term-head, .af-main-term, .af-pane"),
        desc: el
          ? `${el.tagName.toLowerCase()} ${el.className} ${el.getAttribute("aria-label") ?? ""}`
          : "none",
      };
    });
    stops.push(stop.desc);
    if (stop.rail) railStops += 1;
    reachedPane = stop.pane;
  }
  expect(reachedPane, `Tab order never reached the pane chrome: ${stops.join(" | ")}`).toBe(true);
  expect(railStops, `${rowCount} rows must not each cost a Tab stop: ${stops.join(" | ")}`)
    .toBeLessThanOrEqual(2 + selectedChips + inertControls);

  // Focus follows the j/k selection, so Tab then Enter reaches the actions of
  // whatever row is selected — not just the first.
  const selectedRow = page.locator(".af-rail-list .af-row.af-row-selected");
  await selectedRow.focus();
  const before = await selectedRow.locator(".af-row-title").textContent();
  await page.keyboard.press("j");
  const next = page.locator(".af-rail-list .af-row.af-row-selected");
  await expect(next.locator(".af-row-title"), "j moves the rail selection").not.toHaveText(before ?? "", { timeout: 5_000 });
  await expect(next, "DOM focus follows the selection").toBeFocused();
  // In-row controls follow the row in DOM order: a backlog chip sits in the
  // title before the actions button, so Tab may land on it first.
  const trigger = next.getByRole("button", { name: /^Actions for / });
  for (let i = 0; i < 3 && !(await trigger.evaluate((el) => el === document.activeElement)); i++) {
    await page.keyboard.press("Tab");
  }
  await expect(trigger, "the selected row's actions button is reachable by Tab").toBeFocused();
  await page.keyboard.press("Enter");
  const menu = next.locator(".af-term-menu");
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(trigger, "Escape returns focus to the actions button").toBeFocused();
});

test("#6409: with nothing selected the first row holds the stop, and it roves cleanly", async ({ page }) => {
  await seedExtraRows(page);
  await page.goto("/");
  await page.locator("#app[data-af-resync-settled]").waitFor();
  // A hashless load selects nothing: the FIRST row carries the stop so the
  // listbox never leaves the Tab order.
  await expect(page.locator(".af-rail-list .af-row.af-row-selected")).toHaveCount(0);
  const first = page.locator(".af-rail-list .af-row").first();
  await expect(first).toHaveJSProperty("tabIndex", 0);
  // k from no selection picks the LAST row; the reused first-row element must
  // drop its stop, not keep a second one beside the selected row's.
  await page.keyboard.press("k");
  const selected = page.locator(".af-rail-list .af-row.af-row-selected");
  await expect(selected).toHaveJSProperty("tabIndex", 0);
  await expect(first).toHaveJSProperty("tabIndex", -1);
  const tabbable = await page.locator(".af-rail-list").evaluate((list) =>
    [...list.querySelectorAll<HTMLElement>(".af-row")].filter((r) => r.tabIndex === 0).length,
  );
  expect(tabbable, "exactly one row is a Tab stop").toBe(1);
});
