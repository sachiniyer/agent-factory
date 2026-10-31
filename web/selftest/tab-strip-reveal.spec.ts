import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";

// #6420: a tab that becomes active (new tab, digit key) while the strip is scrolled
// elsewhere must be revealed, and a plain re-render must not undo a deliberate scroll.
const active = (page: Page) => page.evaluate(() => {
  const bar = document.querySelector(".af-tabbar") as HTMLElement;
  const act = document.querySelector(".af-tabbar .af-tab-active") as HTMLElement;
  const b = bar.getBoundingClientRect(), a = act.getBoundingClientRect();
  return { inside: a.left >= b.left - 1 && a.right <= b.right + 1, scroll: bar.scrollLeft };
});

test("tab strip: the active tab is revealed after a new tab or a digit key", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });
  const title = "strip-reveal";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  for (let i = 0; i < 14; i++) af("sessions", "tab-create", "--repo", mockRepo as string, title, "--kind", "shell");

  await page.setViewportSize({ width: 1280, height: 800 });
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const s = snap.data.instances.find((x: { title: string }) => x.title === title);
  await page.goto(`/#/session/${encodeURIComponent(s.id)}`);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(15);
  await expect.poll(async () => (await active(page)).inside).toBe(true);

  await page.locator(".af-term-new-shell").first().click();
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(16);
  await expect.poll(async () => (await active(page)).inside, { message: "the new tab is revealed" }).toBe(true);

  await page.keyboard.press("Control+]");
  await page.keyboard.press("9");
  await expect.poll(async () => (await active(page)).inside, { message: "digit 9 reveals tab 9" }).toBe(true);
  await page.keyboard.press("1");
  await expect.poll(async () => (await active(page)).inside, { message: "digit 1 scrolls back" }).toBe(true);

  // A deliberate scroll away from the active tab survives a re-render.
  await page.evaluate(() => { (document.querySelector(".af-tabbar") as HTMLElement).scrollLeft = 900; });
  await page.locator(".af-tabbar .af-tab").nth(9).click({ trial: true }).catch(() => {});
  const before = (await active(page)).scroll;
  await page.waitForTimeout(1500);
  expect((await active(page)).scroll).toBe(before);
});
