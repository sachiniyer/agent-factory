import { expect, test, type Page } from "@playwright/test";

// #6563: once the fit hides every sibling, a retained multi-pane layout shows
// one visible pane — and BOTH close routes vanished with it: the phone menu's
// "Hide pane" gated on the visible set (shownTabs.length < 2) and the per-pane
// head holding the close button, removed when multi collapsed to one. The
// notice still says "resize or close a pane", but a touch user had no way to.
// Close availability must follow the RETAINED pane count, not the visible one.

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

test("#6563: a touch user can still close a pane when the fit shows only one", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await page.setViewportSize({ width: 1280, height: 800 });

  // probe-a's tab roster is shared with the suite — count relative to it.
  const tabs = page.locator(".af-tabbar .af-tab");
  const base = await tabs.count();
  await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  await expect(tabs).toHaveCount(base + 1);
  await dragTabToPane(page, base, 0, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);

  // Narrow until the fit can show only one pane — the retained layout still
  // has two leaves, so a close affordance must remain reachable. On phone
  // that route is the session-actions menu (pane-heads are always hidden
  // there); the per-pane head close is its own gate, same bug.
  await page.setViewportSize({ width: 390, height: 800 });
  await expect(page.locator(".af-split-narrow")).toContainText("1 pane hidden");

  // On phone the term-head — and the "More session actions" trigger inside it
  // — is display:none under af-session-first; the session menu panel is
  // reparented into "More app controls", which is the touch route.
  await page.getByRole("button", { name: "More app controls", exact: true, includeHidden: true }).click();
  const hideButton = page.locator(".af-phone-pane-close");
  await expect(hideButton, "the menu's Hide pane survives at one visible pane").toBeVisible();
  await hideButton.click();

  // The leaf collapses out of the retained tree: one pane, no notice, and the
  // closed pane's tab is back to being a strip-only tab (still in the bar).
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-split-narrow")).toHaveCount(0);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(base + 1);
});
