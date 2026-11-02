import { expect, test, type Page } from "@playwright/test";

// #6555: the #6429 fit pass rebuilt the split DOM with replaceChildren on
// every resize that crossed the hidden/visible threshold, and re-inserting
// a detached <iframe> destroys its browsing context — a Web/VS Code pane
// reloaded and lost its in-page state on a plain window resize, even when
// the pane stayed visible the whole time. A pane that stays open must never
// lose its document across a refit: hidden leaves hide in place with CSS
// rather than leaving the DOM.

const sessionWeb = process.env.AF_WEB_SESSION_WEB ?? "probe-web";

/** Same shared-DataTransfer dispatch as split-min-width.spec.ts/web-driver.spec.ts. */
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

test("#6555: a web pane keeps its document across a hide/restore refit", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionWeb);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  // Two side-by-side panes fit the ~1100px host; 900px forces exactly one hidden.
  await page.setViewportSize({ width: 1400, height: 900 });

  // Open the seeded local web tab — the pane mounts an iframe.
  await page.locator(".af-tabbar .af-tab", { hasText: "preview" }).click();
  const iframe = page.locator(".af-term-host iframe.af-webframe");
  await expect(iframe).toHaveCount(1);

  // Split the Agent tab in beside it: [preview | Agent].
  await dragTabToPane(page, 0, 0, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);

  // Keep the web pane the protected/focused leaf — it stays visible through
  // the shrink, which is the strongest case: a pane that never even hid must
  // still hold its state.
  await page.locator(".af-pane-label", { hasText: "preview" }).click();

  const frameHandle = await iframe.elementHandle();
  const frame = await frameHandle?.contentFrame();
  expect(frame).toBeTruthy();
  await frame!.evaluate(() => {
    (window as unknown as { __refitProbe: number }).__refitProbe = 42;
  });

  // Shrink below the two-pane floor: one leaf hides (the notice says so).
  await page.setViewportSize({ width: 900, height: 900 });
  await expect(page.locator(".af-split-narrow")).toContainText("1 pane hidden");
  // Grow back: the hidden leaf restores.
  await page.setViewportSize({ width: 1400, height: 900 });
  await expect(page.locator(".af-split-narrow")).toHaveCount(0);
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);

  // The same iframe element, and the same document inside it — a detach or
  // reparent would have reloaded the frame and dropped the marker.
  await expect(iframe).toHaveCount(1);
  const marker = await frame!.evaluate(() => (window as unknown as { __refitProbe?: number }).__refitProbe);
  expect(marker).toBe(42);
});
