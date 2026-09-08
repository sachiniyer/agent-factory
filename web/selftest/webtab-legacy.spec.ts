import { expect, test } from "@playwright/test";

// Keep the real daemon fixture, but project an old daemon's raw URL and absent
// web_proxied field. Creation through today's API would canonicalize the URL.
for (const host of ["127.1", "0177.0.0.1", "2130706433"]) {
  test(`web tab blocks legacy shorthand ${host} without navigation`, async ({ page }) => {
    const sessionTitle = process.env.AF_WEB_SESSION_WEB ?? "probe-web";
    await page.routeWebSocket((url) => url.pathname === "/v1/events", () => {});
    await page.route("**/v1/Snapshot", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      const instance = body.data.instances.find((s: { title: string }) => s.title === sessionTitle);
      expect(instance).toBeTruthy();
      // Other webtab specs close the real preview tab. Add a snapshot-only
      // legacy tab so this case cannot depend on their mutation order.
      instance.tabs.push({ id: "legacy-shorthand", name: "legacy", kind: 3, url: `http://${host}:3000` });
      await route.fulfill({ response, json: body });
    });
    const unsafeRequests: string[] = [];
    await page.route("http://127.0.0.1:3000/**", async (route) => {
      unsafeRequests.push(route.request().url());
      await route.abort();
    });
    if (process.env.AF_MOCK_REPO) {
      await page.addInitScript((repo) => localStorage.setItem("af-project", repo), process.env.AF_MOCK_REPO);
    }
    await page.goto("/");
    await page.locator(".af-rail-list .af-row", { hasText: sessionTitle }).click();
    await page.locator(".af-tabbar .af-tab", { hasText: "legacy" }).click();
    const pane = page.locator(".af-term-host .af-webpane");
    await expect(pane.locator(".af-webpane-dead")).toContainText(`Cannot open web target ${host} safely`);
    await expect(pane.locator("iframe")).toHaveCount(0);
    await expect(pane.locator("a")).toHaveCount(0);
    expect(unsafeRequests).toEqual([]);
  });
}
