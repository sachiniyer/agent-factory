import { execFileSync } from "node:child_process";
import { expect, test } from "@playwright/test";

// The focused split pane draws its ring as an overlay above the terminal: xterm
// painted over an outline, so the ring showed only around the pane head.
test("split panes: the focused pane's ring is an overlay, not an outline", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const title = "ring-probe";
  execFileSync(afBin as string, ["sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude"], {
    stdio: ["ignore", "pipe", "pipe"],
  });
  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  await page.goto(`/#/session/${session.id}`);
  await expect(page.locator(".af-tab-label", { hasText: /^Agent$/ })).toBeVisible();
  await page.getByRole("button", { name: "Terminal" }).first().click();
  await expect(page.locator(".af-tab")).toHaveCount(2, { timeout: 15_000 });
  const pane = page.locator(".af-pane").first();
  const box = (await pane.boundingBox())!;
  await page.locator('.af-tab[data-tab-index="0"]').dragTo(pane, { targetPosition: { x: 8, y: box.height / 2 } });
  await expect(page.locator(".af-pane")).toHaveCount(2);
  await page.locator(".af-pane-host .xterm").first().click();
  const ring = await page.locator(".af-pane-focused").first().evaluate((el) => {
    const after = getComputedStyle(el, "::after");
    return { border: after.borderTopWidth, position: after.position, events: after.pointerEvents, outline: getComputedStyle(el).outlineStyle };
  });
  expect(ring.position).toBe("absolute");
  expect(ring.events).toBe("none");
  expect(ring.border).not.toBe("0px");
  expect(ring.outline, "an outline is hidden by pane content").toBe("none");
});
