import { expect, test, type Page } from "@playwright/test";

// #6429: web split panes had no minimum width — four panes at 1024px left a
// 48px sliver, and a divider drag could squeeze a pane to almost nothing.
// The TUI enforces a 40-column pane floor (ui/layout/grid.go PaneMinWidth)
// and hides what does not fit with a "too narrow" message; the web now does
// the same: no rendered pane may go under the floor, and when the minimum
// cannot be met the layout degrades visibly (a ".af-split-narrow" notice
// names the hidden count) rather than producing slivers.

const sessionA = process.env.AF_WEB_SESSION_A ?? "probe-a";
// A little under split.ts's 40-column floor, so subpixel layout never turns
// a compliant pane into a failure.
const PANE_FLOOR_PX = 300;

/** The issue's repro: drag tab index `tabIndex` onto `edge` of pane index `paneIndex`
 *  — the same shared-DataTransfer dispatch web-driver.spec.ts's dragTabToPane uses,
 *  generalized to a chosen pane so nested splits land where the issue put them. */
async function dragTabToPane(page: Page, tabIndex: number, paneIndex: number, edge: "left" | "right" | "top" | "bottom"): Promise<void> {
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
      let x = r.left + r.width / 2;
      let y = r.top + r.height / 2;
      const m = 6;
      if (edge === "left") x = r.left + m;
      else if (edge === "right") x = r.right - m;
      else if (edge === "top") y = r.top + m;
      else y = r.bottom - m;
      const init = { bubbles: true, cancelable: true, dataTransfer: dt, clientX: x, clientY: y };
      pane.dispatchEvent(new DragEvent("dragenter", init));
      pane.dispatchEvent(new DragEvent("dragover", init));
      pane.dispatchEvent(new DragEvent("drop", init));
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
    },
    { tabIndex, paneIndex, edge },
  );
}

/** The rendered width of every .af-pane on screen, narrowest first. */
async function paneWidths(page: Page): Promise<number[]> {
  const widths = await page
    .locator(".af-term-host .af-pane")
    .evaluateAll((panes) => panes.map((p) => Math.round(p.getBoundingClientRect().width)).sort((a, b) => a - b));
  return widths;
}

test("#6429: no pane renders under the floor; overflow degrades visibly, not to slivers", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  // The issue starts at 1440 — wide enough that all four panes fit during
  // setup. The default 1280 host is already under the four-pane minimum, so
  // the fit would hide one before the repro even begins.
  await page.setViewportSize({ width: 1440, height: 900 });

  // The issue's fleet: Agent plus three terminal tabs.
  const tabs = page.locator(".af-tabbar .af-tab");
  for (let want = 2; want <= 4; want++) {
    await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
    await expect(tabs).toHaveCount(want);
  }

  // ...and its layout: tab 2 to the right edge, tab 3 to the bottom edge of
  // the new pane, tab 4 to the left edge of the first — four panes.
  await dragTabToPane(page, 1, 0, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);
  await dragTabToPane(page, 2, 1, "bottom");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(3);
  await dragTabToPane(page, 3, 0, "left");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(4);

  // The issue's resize: 1440 down to 1024, where the reporter measured a
  // 48px pane. Whatever survives must clear the floor — and if four panes
  // cannot all clear it, some must hide WITH the layout saying so, instead
  // of four slivers.
  await page.setViewportSize({ width: 1024, height: 768 });
  await expect
    .poll(async () => Math.min(...(await paneWidths(page))), {
      message: "no rendered pane may sit under the usable floor at 1024px",
      timeout: 10_000,
    })
    .toBeGreaterThanOrEqual(PANE_FLOOR_PX);
  const notice = page.locator(".af-split-narrow");
  await expect(notice, "a degraded layout tells the user panes are hidden").toContainText(/panes? hidden/);

  // The drag path enforces the same floor: pull the divider as far toward
  // the left edge as the pointer can reach. The clamp must stop at the
  // minimum, not at the old 10% sliver.
  const divider = page.locator(".af-term-host .af-divider").first();
  const box = await divider.boundingBox();
  expect(box, "a visible split still has a divider to drag").toBeTruthy();
  const host = page.locator(".af-term-host");
  const hostBox = await host.boundingBox();
  expect(hostBox).toBeTruthy();
  await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
  await page.mouse.down();
  await page.mouse.move(hostBox!.x + 5, box!.y + box!.height / 2, { steps: 10 });
  await page.mouse.up();
  const after = await paneWidths(page);
  expect(
    Math.min(...after),
    `after a divider squeeze the panes render at ${after.join(", ")}px; nothing may be a sliver`,
  ).toBeGreaterThanOrEqual(PANE_FLOOR_PX);
});
