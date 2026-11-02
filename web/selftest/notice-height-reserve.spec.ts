import { expect, test, type Page } from "@playwright/test";

// #6561: the hidden-pane notice mounts as a real flex row AFTER the fit has
// measured the full host height — so its own height is never part of the
// budget. With a stacked (column) layout whose survivors only just clear the
// 80px floor, mounting the ~30px notice shrinks them below it: panes the fit
// certified healthy render as slivers. The notice's height must be reserved
// before the fit decides how many leaves stay visible.
const PANE_MIN_HEIGHT_PX = 80;

const sessionA = process.env.AF_WEB_SESSION_A ?? "probe-a";

async function dragTabToPane(page: Page, tabIndex: number, paneIndex: number, edge: "top" | "bottom"): Promise<void> {
  await page.evaluate(
    ({ tabIndex, paneIndex, edge }) => {
      const tab = document.querySelectorAll<HTMLElement>(".af-tabbar .af-tab")[tabIndex];
      const pane = document.querySelectorAll<HTMLElement>(".af-term-host .af-pane")[paneIndex];
      if (!tab || !pane) {
        throw new Error(`drag source ${tabIndex} or target pane ${paneIndex} not found`);
      }
      const dt = new DataTransfer();
      tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));
      const r = pane.getBoundingClientRect();
      const init = {
        bubbles: true,
        cancelable: true,
        dataTransfer: dt,
        clientX: r.left + r.width / 2,
        clientY: edge === "top" ? r.top + 6 : r.bottom - 6,
      };
      pane.dispatchEvent(new DragEvent("dragenter", init));
      pane.dispatchEvent(new DragEvent("dragover", init));
      pane.dispatchEvent(new DragEvent("drop", init));
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
    },
    { tabIndex, paneIndex, edge },
  );
}

async function paneHeights(page: Page): Promise<number[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>(".af-term-host .af-pane")]
      .filter((p) => p.getClientRects().length > 0)
      .map((p) => Math.round(p.getBoundingClientRect().height)),
  );
}

test("#6561: the notice's height is reserved before the fit", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await page.setViewportSize({ width: 1280, height: 700 });

  // Aim the host at ~345px: tall enough that the naive fit keeps three stacked
  // panes, short enough that the ~30px notice pushes them under the floor.
  // The host height tracks the viewport, so one delta resize lands it.
  const host = page.locator(".af-term-host");
  const hostH0 = (await host.boundingBox())!.height;
  await page.setViewportSize({ width: 1280, height: Math.round(700 + (345 - hostH0)) });
  const hostH = (await host.boundingBox())!.height;
  expect(hostH, `host should sit in the repro band, got ${hostH}px`).toBeGreaterThan(325);
  expect(hostH, `host should sit in the repro band, got ${hostH}px`).toBeLessThan(370);

  // Four stacked panes → nested column chain [a/(b/(c/d))]: each drop stacks a
  // new pane under the last one. At this height the bottom pair slivers, so
  // the fit hides a leaf and the notice mounts — then the survivors must still
  // clear the floor in the space that remains. probe-a's tab roster is shared
  // with the suite — count relative to it. Four shells: the last-created stays
  // SELECTED in the top pane (a new shell is always selected on create), so
  // the three drags all move genuinely unshown tabs — dropping a shown tab on
  // another pane trips the dedupe move, not a split.
  const tabs = page.locator(".af-tabbar .af-tab");
  const base = await tabs.count();
  for (let i = 0; i < 4; i++) {
    await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  }
  await expect(tabs).toHaveCount(base + 4);
  await dragTabToPane(page, base, 0, "bottom");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);
  await dragTabToPane(page, base + 1, 1, "bottom");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(3);
  await dragTabToPane(page, base + 2, 2, "bottom");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(4);
  await expect(page.locator(".af-split-narrow")).toContainText("hidden");

  const heights = await paneHeights(page);
  expect(
    heights.every((h) => h >= PANE_MIN_HEIGHT_PX - 1),
    `with the notice mounted the visible panes render at ${heights.join(", ")}px; the fit must not have spent the notice's height`,
  ).toBe(true);
});
