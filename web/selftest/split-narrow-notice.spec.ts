import { expect, test, type Page } from "@playwright/test";

// #6556: the ".af-split-narrow" notice was written inside the rebuild gate,
// so it only ever described the fit at the moment the visible tree changed.
// Two lapses follow: a split dropped into an already-narrow host hides the
// new leaf but never says so, and the hidden COUNT can change while the
// visible tree stays identical (a hidden leaf closes) leaving stale text.
// The notice must track fit.hidden on every pass.

const sessionA = process.env.AF_WEB_SESSION_A ?? "probe-a";

/** Same shared-DataTransfer dispatch as split-min-width.spec.ts. */
async function dragTabToPane(page: Page, tabIndex: number, paneIndex: number, edge: "left" | "right"): Promise<void> {
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
      const m = 6;
      const init = {
        bubbles: true,
        cancelable: true,
        dataTransfer: dt,
        clientX: edge === "left" ? r.left + m : r.right - m,
        clientY: r.top + r.height / 2,
      };
      pane.dispatchEvent(new DragEvent("dragenter", init));
      pane.dispatchEvent(new DragEvent("dragover", init));
      pane.dispatchEvent(new DragEvent("drop", init));
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
    },
    { tabIndex, paneIndex, edge },
  );
}

async function newShellTab(page: Page, want: number): Promise<void> {
  await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(want);
}

test("#6556: a split into an already-narrow host still names its hidden pane", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  // ~580px of host: one pane fits, a second never can.
  await page.setViewportSize({ width: 900, height: 800 });
  await expect(page.locator(".af-split-narrow")).toHaveCount(0);

  // The split happens — the new leaf is hidden immediately — and the layout
  // must SAY one pane is hidden rather than silently rendering the old tree.
  await newShellTab(page, 2);
  await dragTabToPane(page, 1, 0, "right");
  await expect(page.locator(".af-split-narrow")).toContainText("1 pane hidden");
  await expect(page.locator(".af-term-host .af-pane:visible")).toHaveCount(1);
});

test("#6556: a hidden-count change with the same visible tree rewrites the notice", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await page.setViewportSize({ width: 1400, height: 800 });

  // Three side-by-side panes, all fitting.
  for (let want = 2; want <= 3; want++) {
    await newShellTab(page, want);
  }
  await dragTabToPane(page, 1, 0, "right");
  await dragTabToPane(page, 2, 1, "right");
  await expect(page.locator(".af-term-host .af-pane:visible")).toHaveCount(3);

  // Shrink until only one pane fits: two leaves hide, the notice says so.
  await page.setViewportSize({ width: 700, height: 800 });
  await expect(page.locator(".af-split-narrow")).toContainText("2 panes hidden");
  await expect(page.locator(".af-term-host .af-pane:visible")).toHaveCount(1);

  // Close one of the HIDDEN panes' tabs: focus sat on the newest leaf, so the
  // hidden ones are Agent (index 0) and the first shell (index 1). Closing a
  // hidden leaf's tab leaves the visible tree ({one leaf}) untouched, but the
  // count must drop — a notice still reading "2 panes hidden" is the bug.
  const hiddenTab = page.locator(".af-tabbar .af-tab").nth(1);
  await hiddenTab.locator(".af-tab-close").click();
  await expect(page.locator(".af-split-narrow")).toContainText("1 pane hidden");
  await expect(page.locator(".af-term-host .af-pane:visible")).toHaveCount(1);
});
