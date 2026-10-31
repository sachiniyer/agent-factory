// The working row's loader glyph (#6284 theme C, fork ec5c51732f), against the REAL seeded
// daemon with one synthetic Running row added to its Snapshot: it turns by default and is
// still under prefers-reduced-motion, and a row that is not working never animates.
// Screenshots of the row in both modes land in the test output directory.

import { expect, test, type Page } from "@playwright/test";

const SESSION_A = process.env.AF_WEB_SESSION_A ?? "probe-a";

async function openWithWorkingRow(page: Page, reduced: boolean): Promise<void> {
  await page.emulateMedia({ reducedMotion: reduced ? "reduce" : "no-preference" });
  await page.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const list: Array<Record<string, unknown> & { title: string }> = body?.data?.instances ?? [];
    const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
    const synth = (title: string, liveness: number, idleReason = "") => ({
      ...proto, id: `synth-${title}`, title, branch: `synth-${title}`, liveness, in_flight_op: 0, idle_reason: idleReason,
    });
    list.push(synth("spin-working", 1), synth("spin-idle", 2, "settled-after-pane-change"), synth("spin-lost", 3, "process-exited"));
    if (body?.data) body.data.instances = list;
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
}

const rowOf = (page: Page, title: string) => page.locator(".af-rail-list .af-row", { hasText: title });
const glyph = (page: Page, title: string) => rowOf(page, title).locator(".af-dot .af-icon");
const transform = (page: Page, title: string) => glyph(page, title).evaluate((el) => getComputedStyle(el).transform);

for (const reduced of [false, true]) {
  test(`working glyph ${reduced ? "is still under reduced motion" : "turns"}`, async ({ page }, info) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await openWithWorkingRow(page, reduced);
    await expect(rowOf(page, "spin-working")).toBeVisible({ timeout: 15_000 });
    const dot = rowOf(page, "spin-working").locator(".af-dot");
    await expect(dot.locator('.af-icon[data-icon="loader-circle"]')).toHaveCount(1);
    const name = await glyph(page, "spin-working").evaluate((el) => getComputedStyle(el).animationName);
    expect(name).toBe(reduced ? "none" : "af-working-spin");
    const a = await transform(page, "spin-working");
    await page.waitForTimeout(250);
    const b = await transform(page, "spin-working");
    if (reduced) expect(b, "no rotation under reduced motion").toBe(a);
    else expect(b, "the glyph has turned").not.toBe(a);
    // Rows that are not working never animate, in either mode.
    await expect(glyph(page, "spin-lost")).toHaveCount(1);
    expect(await glyph(page, "spin-lost").evaluate((el) => getComputedStyle(el).animationName)).toBe("none");
    await expect(rowOf(page, "spin-idle").locator(".af-dot")).toHaveCount(0);
    await rowOf(page, "spin-working").screenshot({ path: info.outputPath(`working-row-${reduced ? "reduced" : "motion"}.png`) });
  });
}
