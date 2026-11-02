import { expect, test, type Page } from "@playwright/test";

// #6556: the ".af-split-narrow" notice was written inside the rebuild gate,
// so it only ever described the fit at the moment the visible tree changed.
// Two lapses follow: a split dropped into an already-narrow host hides the
// new leaf but never says so, and the hidden COUNT can change while the
// visible tree stays identical (a hidden leaf closes) leaving stale text.
// The notice must track fit.hidden on every pass.
//
// probe-a's tab roster is shared with the whole suite — earlier specs leave
// tabs behind — so every count here is taken relative to what the test finds.

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

/** Clicks "new shell" and waits for the roster to grow by exactly one. */
async function newShellTab(page: Page): Promise<void> {
  const tabs = page.locator(".af-tabbar .af-tab");
  const have = await tabs.count();
  await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  await expect(tabs).toHaveCount(have + 1);
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
  const base = await page.locator(".af-tabbar .af-tab").count();
  await newShellTab(page);
  await dragTabToPane(page, base, 0, "right");
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
  const base = await page.locator(".af-tabbar .af-tab").count();
  await newShellTab(page);
  await newShellTab(page);
  await dragTabToPane(page, base, 0, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);
  await dragTabToPane(page, base + 1, 1, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(3);

  // Pin focus on the LAST pane so the hidden set is deterministic: after the
  // shrink the hidden leaves are exactly the two I did not focus — the
  // original pane and the leaf bound to my first new shell (tab `base`).
  await page.locator(".af-term-host .af-pane").nth(2).click();
  // 900px keeps the desktop rail AND the tab bar — under the 768px breakpoint
  // af-session-first hides .af-term-head, and the close affordance below
  // would have no button to click. ~580px of host fits one 320px pane.
  await page.setViewportSize({ width: 900, height: 800 });
  await expect(page.locator(".af-split-narrow")).toContainText("2 panes hidden");
  await expect(page.locator(".af-term-host .af-pane:visible")).toHaveCount(1);

  // Close one of the HIDDEN panes' tabs — the shell I added at index `base`,
  // whose leaf is among the hidden pair. Closing it leaves the visible tree
  // ({one leaf}) untouched, but the count must drop — a notice still reading
  // "2 panes hidden" is the bug.
  await page.locator(".af-tabbar .af-tab").nth(base).locator(".af-tab-close").click();
  await page.getByRole("button", { name: "Delete tab", exact: true }).click();
  await expect(page.locator(".af-split-narrow")).toContainText("1 pane hidden");
  await expect(page.locator(".af-term-host .af-pane:visible")).toHaveCount(1);
});
