import { test, expect, type APIRequestContext } from "@playwright/test";
import { quietSession } from "./quiet-session";

// #6405: on a phone with the rail drawer CLOSED, another session's attention
// request must be visible — a count on the drawer toggle and the state in words —
// without hover. Real daemon, the same SetAttention route `af sessions attention
// --self` calls.

const SELECTED = process.env.AF_WEB_SESSION_B ?? "probe-b";
// The session that raises the request is a quiet one: on the shared `cat` session a
// stray Running edge could clear it between the raise and the badge assertions
// (#4217, #6465). The viewed session (SELECTED) is never the one with the request.
const quiet = quietSession("probe-quiet-badge");
const OTHER = quiet.name;

async function id(request: APIRequestContext, title: string) {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const s = snapshot.data.instances.find((i: { title: string }) => i.title === title);
  expect(s?.id, `seeded session ${title}`).toBeTruthy();
  return s.id as string;
}

test.afterEach(async ({ request }) => {
  await request.post("/v1/SetAttention", { data: { id: await id(request, OTHER), clear: true } });
});

for (const [width, height] of [[390, 844], [320, 568]]) {
  test(`#6405 another session's attention shows on the drawer toggle, in words, at ${width}x${height}`, async ({ browser, request }, testInfo) => {
    const ctx = await browser.newContext({ viewport: { width, height }, hasTouch: true, isMobile: true });
    const page = await ctx.newPage();
    try {
      await page.goto(`/#/session/${encodeURIComponent(await id(request, SELECTED))}`);
      await expect(page.locator(".af-term-title")).toHaveText(SELECTED);
      await page.waitForTimeout(2000);
      const toggle = page.getByRole("button", { name: /^Toggle sessions/ });
      const badge = page.locator(".af-nav-badge");
      await expect(badge, "no request, no badge").toBeHidden();
      await expect(toggle).toHaveAttribute("aria-label", "Toggle sessions");

      const res = await (await request.post("/v1/SetAttention", { data: { id: await id(request, OTHER), reason: "pick A or B" } })).json();
      expect(res.error).toBeFalsy();

      // Drawer closed: the count and the words are on the toggle.
      await expect(badge).toBeVisible();
      await expect(badge).toHaveText("1");
      await expect(toggle).toHaveAttribute("aria-label", "Toggle sessions, 1 session needs attention");
      // 44px touch target survives the badge.
      const box = (await toggle.boundingBox())!;
      expect(box.width).toBeGreaterThanOrEqual(44);
      expect(box.height).toBeGreaterThanOrEqual(44);
      await page.screenshot({ path: testInfo.outputPath(`badge-closed-${width}.png`) });

      // One tap opens the drawer; the same state is in visible words, not hover.
      await toggle.tap();
      await expect(page.locator(".af-rail-attention")).toBeVisible();
      await expect(page.locator(".af-rail-attention")).toHaveText("1 session needs attention");
      await page.screenshot({ path: testInfo.outputPath(`badge-open-${width}.png`) });
      await page.locator(".af-nav-scrim").tap({ position: { x: width - 8, y: height / 2 } });

      // Cleared: the badge goes away.
      await request.post("/v1/SetAttention", { data: { id: await id(request, OTHER), clear: true } });
      await expect(badge).toBeHidden();
      await expect(toggle).toHaveAttribute("aria-label", "Toggle sessions");
    } finally {
      await ctx.close();
    }
  });
}
