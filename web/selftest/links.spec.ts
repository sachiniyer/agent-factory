// Links (#6284 theme C), against the REAL seeded daemon: the project switcher's rows
// are links, and terminal links open without xterm's confirm() warning. Each test
// owns its page.

import { expect, test, type WebSocketRoute } from "@playwright/test";
import { encode, ptyOutFrame } from "../src/frame.js";

const SESSION_A = process.env.AF_WEB_SESSION_A ?? "probe-a";

test("project switcher: rows are links; a plain click switches in place, a modified click opens a new tab", async ({ page, context }, info) => {
  // The route is consumed once resolved, so remember the fragment each tab opened with.
  await context.addInitScript(() => { (window as unknown as { openedWith: string }).openedWith = location.hash; });
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
  const switcher = page.locator(".af-project-switch");
  await switcher.click();
  const rows = page.locator(".af-project-menu a.af-project-item");
  await expect.poll(() => rows.count()).toBeGreaterThan(1);
  const other = rows.filter({ hasNot: page.locator(".af-project-check svg") }).first();
  await page.screenshot({ path: info.outputPath("project-menu.png") });
  const href = await other.getAttribute("href");
  expect(href).toMatch(/^#\/project\/.+/);
  const name = (await other.locator(".af-project-item-name").textContent())!.trim();

  // Ctrl-click: the browser opens the link's route in a new tab; this tab stays put.
  const before = (await page.locator(".af-project-switch-name").textContent())!.trim();
  const [popup] = await Promise.all([context.waitForEvent("page"), other.click({ modifiers: ["Control"] })]);
  await popup.waitForURL(/^http/);
  await popup.waitForLoadState("load");
  expect(await popup.evaluate(() => (window as unknown as { openedWith: string }).openedWith)).toBe(href);
  await expect(popup.locator(".af-project-switch-name")).toHaveText(name);
  // The route is consumed once resolved: the address bar no longer carries it.
  await expect.poll(() => new URL(popup.url()).hash).toBe("");
  await expect(page.locator(".af-project-switch-name")).toHaveText(before);

  // A modified click leaves the menu open (the browser opens the tab; the menu stays up
  // behind it, which is what keeps the phone panel open).
  await expect(other).toBeVisible();
  // Plain click, from a menu that is open by this step's own doing: close it, check it
  // is closed, open it fresh, then click the row.
  await switcher.click();
  await expect(other).toBeHidden();
  const pagesBefore = context.pages().length;
  await switcher.click();
  await expect(other).toBeVisible();
  await other.click();
  await expect(page.locator(".af-project-switch-name")).toHaveText(name);
  expect(context.pages().length).toBe(pagesBefore);
  expect(new URL(page.url()).hash).toBe("");
});

test("project switcher: Space activates a row on release", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
  await page.locator(".af-project-switch").click();
  const other = page.locator(".af-project-menu a.af-project-item").filter({ hasNot: page.locator(".af-project-check svg") }).first();
  const name = (await other.locator(".af-project-item-name").textContent())!.trim();
  await other.focus();
  await page.keyboard.down("Space");
  await expect(page.locator(".af-project-switch-name"), "nothing happens on press").not.toHaveText(name);
  await page.keyboard.up("Space");
  await expect(page.locator(".af-project-switch-name")).toHaveText(name);
});

test("a #/project/ link for an unknown project shows a notice and stays on the default project", async ({ page }) => {
  await page.goto("/#/project/%2Fno%2Fsuch%2Frepo");
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-toast")).toContainText("No project at that path in this daemon");
  await expect.poll(() => new URL(page.url()).hash).toBe("");
});

test("terminal links open http(s) in a new tab with no confirm() and no opener; other schemes open nothing", async ({ page, context }, info) => {
  let stream!: WebSocketRoute;
  await page.routeWebSocket((url) => url.pathname.endsWith("/stream"), (socket) => { stream = socket; });
  const dialogs: string[] = [];
  page.on("dialog", (d) => { dialogs.push(d.message()); void d.dismiss(); });
  await context.route("https://links.example.test/**", (route) => route.fulfill({ contentType: "text/html", body: "<title>landed</title>" }));

  await page.goto("/");
  await page.locator(".af-row").filter({ has: page.locator(".af-row-title", { hasText: SESSION_A }) }).click();
  const surface = page.locator(".af-term-host .xterm");
  await expect(surface).toBeVisible();
  await expect.poll(() => Boolean(stream)).toBe(true);

  const link = (url: string, text: string) => `\x1b]8;;${url}\x1b\\${text}\x1b]8;;\x1b\\`;
  const send = (s: string) => stream.send(Buffer.from(encode(ptyOutFrame(new TextEncoder().encode(s)))));
  send(`${link("https://links.example.test/pr/1", "good-link")}\r\n${link("javascript:alert(1)", "js-link")}\r\n${link("data:text/html,x", "data-link")}\r\n`);
  await expect(surface).toContainText("data-link");

  // Hover shows the real URL.
  await surface.getByText("good-link").hover();
  await expect(page.locator('[title="https://links.example.test/pr/1"]')).toHaveCount(1);

  await page.screenshot({ path: info.outputPath("terminal-link-hover.png") });
  const [popup] = await Promise.all([page.waitForEvent("popup"), surface.getByText("good-link").click()]);
  await popup.waitForLoadState();
  expect(popup.url()).toBe("https://links.example.test/pr/1");
  expect(await popup.evaluate(() => window.opener), "noopener: the new tab cannot reach af").toBeNull();

  let opened = false;
  context.on("page", () => { opened = true; });
  await surface.getByText("js-link").click();
  await surface.getByText("data-link").click();
  await page.waitForTimeout(500);
  expect(opened, "javascript: and data: links open nothing").toBe(false);
  expect(dialogs, "no confirm() warning for any link").toEqual([]);
});
