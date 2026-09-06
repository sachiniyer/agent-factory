import { expect, test, type APIRequestContext } from "@playwright/test";
import type { SessionData } from "../src/types.js";

async function session(request: APIRequestContext, title: string): Promise<SessionData & { id: string }> {
  const response = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
  expect(response.ok()).toBeTruthy();
  const body = await response.json();
  const found = body.data.instances.find((s: SessionData) => s.title === title);
  expect(found?.id).toBeTruthy();
  return found;
}

const fragment = (id: string) => `#/session/${encodeURIComponent(id)}`;

test("session link: cold open, selection, clipboard, and hashchange", async ({ page, context, request }, testInfo) => {
  const a = await session(request, process.env.AF_WEB_SESSION_A ?? "probe-a");
  const b = await session(request, process.env.AF_WEB_SESSION_B ?? "probe-b");
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/" + fragment(a.id));
  await expect(page.locator(".af-term-title")).toHaveText(a.title!);
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  const historyLength = await page.evaluate(() => history.length);
  await page.locator(".af-row").filter({ has: page.locator(".af-row-title", { hasText: b.title! }) }).click();
  await expect(page).toHaveURL(new RegExp(fragment(b.id) + "$"));
  expect(await page.evaluate(() => history.length)).toBe(historyLength);
  await page.getByRole("button", { name: "Copy link", exact: true }).click();
  await expect(page.locator(".af-toast")).toContainText("Link copied");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(new URL("/" + fragment(b.id), page.url()).href);
  const still = testInfo.outputPath("copy-link.png");
  await page.screenshot({ path: still });
  await testInfo.attach("Copy link control", { path: still, contentType: "image/png" });
  await page.evaluate((hash) => { location.hash = hash; }, fragment(a.id));
  await expect(page.locator(".af-term-title")).toHaveText(a.title!);
  await page.evaluate(() => { location.hash = "#/session/unknown-session-3909"; });
  await expect(page.locator(".af-toast")).toContainText("No session with that id in this daemon");
  await expect(page.locator(".af-main-empty")).toBeVisible();
  await expect(page).toHaveURL(/\/$/);
});

test("unknown cold link returns to the dashboard once", async ({ page }) => {
  await page.goto("/#/session/unknown-session-3909");
  await expect(page.locator(".af-toast")).toContainText("No session with that id in this daemon");
  await expect(page.locator(".af-main-empty")).toBeVisible();
  await expect(page).toHaveURL(/\/$/);
});

test("links switch project and reveal archived sessions", async ({ page, request }) => {
  const other = await session(request, process.env.AF_WEB_SESSION_C ?? "probe-c");
  const archived = await session(request, process.env.AF_WEB_SESSION_WEB_SHELVED ?? "probe-shelved");
  await page.goto("/" + fragment(other.id));
  await expect(page.locator(".af-term-title")).toHaveText(other.title!);
  await expect(page.locator(".af-row-selected")).toContainText(other.title!);
  await page.evaluate((hash) => { location.hash = hash; }, fragment(archived.id));
  await expect(page.locator(".af-term-title")).toHaveText(archived.title!);
  await expect(page.locator(".af-row-selected.af-row-archived")).toContainText(archived.title!);
});

test("login preserves a session link through a landing on /", async ({ page, request }) => {
  const a = await session(request, process.env.AF_WEB_SESSION_A ?? "probe-a");
  // The real daemon uses an in-page token form, not an HTTP /login redirect.
  // Force the existing harness's auth-info seam; Snapshot remains real.
  await page.route("**/v1/auth-info", (route) => route.fulfill({
    contentType: "application/json", body: JSON.stringify({ data: { auth_required: true } }),
  }));
  await page.goto("/" + fragment(a.id));
  await expect(page.locator("#af-token")).toBeVisible();
  await page.goto("/");
  await expect(page.locator("#af-token")).toBeVisible();
  await expect(page).toHaveURL(new RegExp(fragment(a.id) + "$"));
  await page.locator("#af-token").fill("selftest-token");
  await page.locator(".af-login-form button[type=submit]").click();
  await expect(page.locator(".af-term-title")).toHaveText(a.title!);
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
});
