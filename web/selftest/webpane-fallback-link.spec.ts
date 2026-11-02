import { expect, test } from "@playwright/test";

// #6584: a web pane's fallback withdraws the controls it cannot use — "Open in
// a new tab" when there is no URL to open, and "open ↗"/↻ in the same branch.
// split.ts sets `.hidden` on them, but .af-webpane-fallback-link's own
// `display: inline-flex` outranks the UA [hidden] rule, so the link still
// rendered (and still opened the app root, which a URL-less tab cannot mean).
// Its siblings .af-webpane-open/.af-webpane-reload carry an explicit
// `[hidden]` guard for exactly this; the in-fallback link needs the same.

const sessionTitle = process.env.AF_WEB_SESSION_WEB ?? "probe-web";

test("a URL-less web tab hides its Open-in-a-new-tab link", async ({ page, request }) => {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) => s.title === sessionTitle);
  expect(session).toBeTruthy();
  // The seeded roster carries `nourl`: a kind-3 web tab with no URL. Find it
  // by shape rather than position — the suite shares the roster.
  const index = session.tabs.findIndex((t: { kind: number; url?: string }) => t.kind === 3 && !(t.url ?? "").trim());
  expect(index).toBeGreaterThan(0);

  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await page.locator(`.af-tabbar .af-tab[data-tab-index="${index}"]`).click();

  const fallback = page.locator(".af-term-host .af-webpane-fallback");
  await expect(fallback).toBeVisible();
  await expect(fallback.locator(".af-webpane-fallback-msg")).toHaveText("This web tab has no URL.");
  // The attribute was always set; the spec is that the link is not PAINTED.
  await expect(fallback.locator(".af-webpane-fallback-link")).toBeHidden();
  await expect(fallback.locator("a:visible")).toHaveCount(0);
});
