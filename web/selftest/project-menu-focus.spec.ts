import { test, expect, type WebSocketRoute } from "@playwright/test";

// #4817: the phone More panel inlines the project menu through CSS while its
// `hidden` attribute stays set, so a live session refresh used to send a keyboard
// user on a project row to the More button. The event is injected, not awaited
// from daemon timing, so the rebuild lands exactly while the row holds focus.
test("4817: a live refresh keeps focus on the phone panel's project row", async ({ page, request }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  expect(session?.id).toBeTruthy();
  let events!: WebSocketRoute;
  await page.routeWebSocket("**/v1/events*", socket => { events = socket; });
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await expect(page.locator(".af-app")).toHaveClass(/af-session-first/);

  const controls = page.getByRole("button", { name: "More app controls", exact: true });
  await controls.focus();
  await page.keyboard.press("Enter");
  await expect(controls).toHaveAttribute("aria-expanded", "true");
  const panel = page.locator(".af-appbar-tools");
  const row = panel.locator(".af-project-item-current");
  await expect(row).toBeVisible();
  await row.focus();
  await expect(row).toBeFocused();
  // Mark the focused node so the barrier below proves the menu was rebuilt.
  await row.evaluate(el => { el.dataset.af4817Stale = ""; });

  const updated = { ...session, title: "4817 refreshed projection" };
  events.send(JSON.stringify({ type: "session.updated", data: updated }));
  await expect(page.locator(".af-row-title", { hasText: updated.title })).toHaveCount(1);
  await expect(panel.locator("[data-af4817-stale]"), "the refresh replaced the focused row").toHaveCount(0);
  await expect(row, "focus stays on the rebuilt project row").toBeFocused();
  await expect(controls).not.toBeFocused();
});
