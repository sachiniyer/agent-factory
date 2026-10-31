// pt-automation play-test (not committed), pass 2: forms, menus, keyboard, motion, zoom.
import { test, type Page } from "@playwright/test";
import * as fs from "fs";

const OUT = "test-results/pt-shots";
async function snap(page: Page, name: string) {
  fs.mkdirSync(OUT, { recursive: true });
  await page.screenshot({ path: `${OUT}/${name}.png` });
}
const VPS: [string, number, number][] = [["320x568", 320, 568], ["390x844", 390, 844], ["844x390", 844, 390], ["768x1024", 768, 1024], ["1280x800", 1280, 800], ["1920x1080", 1920, 1080]];

async function toTasks(page: Page) {
  await page.goto("/");
  await page.locator(".af-app").waitFor();
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  await page.waitForTimeout(600);
}

for (const scheme of ["light", "dark"] as const) {
  for (const [vp, w, h] of VPS) {
    test(`forms ${vp} ${scheme}`, async ({ browser }) => {
      const ctx = await browser.newContext({ viewport: { width: w, height: h }, colorScheme: scheme, hasTouch: w < 800 });
      const page = await ctx.newPage();
      await toTasks(page);
      await page.getByRole("button", { name: /Add/ }).first().click();
      await page.waitForTimeout(500);
      await snap(page, `${vp}-${scheme}-task-form`);
      await page.keyboard.press("Escape");
      await page.waitForTimeout(300);
      const more = page.getByRole("button", { name: /More|…|\.\.\./ }).first();
      if (await more.isVisible().catch(() => false)) {
        await more.click(); await page.waitForTimeout(300); await snap(page, `${vp}-${scheme}-task-menu`);
        await page.keyboard.press("Escape");
      } else {
        await page.getByRole("button", { name: /Edit/ }).first().click(); await page.waitForTimeout(400);
        await snap(page, `${vp}-${scheme}-task-edit`);
      }
      await ctx.close();
    });
  }
}

test("keyboard-only tab order on tasks 1280x800", async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  await toTasks(page);
  const seq: string[] = [];
  for (let i = 0; i < 14; i++) {
    await page.keyboard.press("Tab");
    seq.push(await page.evaluate(() => { const e = document.activeElement as HTMLElement; return `${e?.tagName}|${e?.getAttribute("aria-label") ?? e?.textContent?.trim().slice(0, 30)}`; }));
    if (i === 5 || i === 9) await snap(page, `1280x800-light-tasks-tab${i}`);
  }
  fs.mkdirSync(OUT, { recursive: true });
  fs.writeFileSync(`${OUT}/tab-order.txt`, seq.join("\n"));
  await ctx.close();
});

test("zoom 200% equivalent (640x400) + reduced motion", async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 640, height: 400 }, reducedMotion: "reduce" });
  const page = await ctx.newPage();
  await toTasks(page);
  await snap(page, "zoom200-1280x800-tasks");
  await page.getByRole("button", { name: /Add/ }).first().click(); await page.waitForTimeout(400);
  await snap(page, "zoom200-1280x800-task-form");
  await ctx.close();
});

test("run-now feedback + remove confirm 1280x800", async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  await toTasks(page);
  await page.getByRole("button", { name: /Trigger|Run/ }).first().click().catch(() => {});
  await page.waitForTimeout(250); await snap(page, "run-now-250ms");
  await page.waitForTimeout(2500); await snap(page, "run-now-2750ms");
  await page.getByRole("button", { name: /Remove|Delete/ }).first().click().catch(() => {});
  await page.waitForTimeout(400); await snap(page, "remove-confirm");
  await ctx.close();
});

test("config + docs-ish views 1280x800", async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  await page.goto("/"); await page.locator(".af-app").waitFor();
  await page.locator('.af-viewtab[data-view="config"]').click(); await page.waitForTimeout(800);
  await snap(page, "1280x800-config");
  await ctx.close();
});
