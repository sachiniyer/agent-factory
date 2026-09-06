import { expect, test, type WebSocketRoute } from "@playwright/test";
import type { SessionData } from "../src/types.js";

test("#3914: unchanged and reordered snapshots retain the focused row and its open menu", async ({ page, request }) => {
  const response = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
  expect(response.ok()).toBe(true);
  const original = await response.json();
  const sessions = original.data.instances as SessionData[];
  const a = sessions.find(session => session.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  const b = sessions.find(session => session.title === (process.env.AF_WEB_SESSION_B ?? "probe-b"));
  expect(a?.id).toBeTruthy();
  expect(b?.id).toBeTruthy();
  expect(a?.worktree?.repo_path).toBe(b?.worktree?.repo_path);

  // Keep real identities/capabilities, but freeze unrelated daemon churn and
  // control the sort timestamps. Reversing the API array alone does not reorder
  // the rail: its comparator sorts live sessions by created_at.
  const earlier = { ...a!, created_at: "2000-01-01T00:00:00Z" };
  const focused = { ...b!, created_at: "2001-01-01T00:00:00Z" };
  let projection = [earlier, focused];
  let snapshots = 0;
  await page.route("**/v1/Snapshot", route => {
    snapshots++;
    return route.fulfill({ json: { ...original, data: { ...original.data, instances: projection } } });
  });
  let events: WebSocketRoute | undefined;
  await page.routeWebSocket(url => url.pathname === "/v1/events", socket => {
    events = socket;
    // Opening/reopening still exercises the client's real resync path.
  });
  await page.goto(`/#/session/${encodeURIComponent(a!.id!)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  const rows = page.locator(".af-rail-list .af-row");
  await expect(rows).toHaveCount(2);
  await expect(rows.first().locator(".af-row-title")).toHaveText(a!.title);
  const row = rows.filter({ has: page.locator(".af-row-title", { hasText: b!.title }) });
  const trigger = row.getByRole("button", { name: `Actions for ${b!.title}`, exact: true });
  await row.hover();
  await trigger.click();
  const menu = row.locator(".af-term-menu");
  const control = menu.locator(".af-rail-kill");
  await control.focus();
  await expect(control).toBeFocused();
  const retainedRow = await row.elementHandle();
  const retainedMenu = await menu.elementHandle();
  const retainedControl = await control.elementHandle();
  expect(retainedRow).not.toBeNull();
  expect(retainedMenu).not.toBeNull();
  expect(retainedControl).not.toBeNull();

  const resync = async () => {
    const before = snapshots;
    await page.locator("#app").evaluate(root => root.removeAttribute("data-af-resync-settled"));
    expect(events).toBeDefined();
    events!.close();
    await expect.poll(() => snapshots).toBeGreaterThan(before);
    await page.locator("#app[data-af-resync-settled]").waitFor();
  };
  const expectRetainedFocus = async () => {
    expect(await retainedRow!.evaluate(node => node.isConnected)).toBe(true);
    expect(await retainedMenu!.evaluate(node => node.isConnected)).toBe(true);
    expect(await retainedControl!.evaluate(node => node === document.activeElement)).toBe(true);
    await expect(menu).toBeVisible();
    await expect(trigger).toHaveAttribute("aria-expanded", "true");
  };

  await resync();
  await expectRetainedFocus();
  // Only the OTHER row changes. The focused row must move before it while
  // retaining the same child controls, open disclosure, and keyboard focus.
  projection = [{ ...earlier, created_at: "2002-01-01T00:00:00Z" }, focused];
  await resync();
  await expect(rows.first().locator(".af-row-title")).toHaveText(b!.title);
  await expectRetainedFocus();
});
