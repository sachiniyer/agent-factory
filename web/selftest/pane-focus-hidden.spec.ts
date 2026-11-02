import { expect, test, type Page } from "@playwright/test";

// #6562: Alt+j/k onto a pane the fit hid restores it HIGHLIGHTED but without
// the keyboard. cyclePane calls focusPane(next) then refocus() synchronously;
// the hidden pane's textarea is display:none so focus() silently no-ops, and
// the queued refit — which actually un-hides the pane — never retries the
// handoff. Result: keystrokes land on document.body while the focus ring says
// otherwise. The pending focus intent must survive to the post-refit refocus.

const sessionA = process.env.AF_WEB_SESSION_A ?? "probe-a";

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

test("#6562: Alt+j onto a fit-hidden pane restores it WITH the keyboard", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await page.setViewportSize({ width: 1280, height: 800 });

  await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(2);
  await dragTabToPane(page, 1, 0, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);

  // Put the keyboard in the FIRST pane's terminal, then narrow until the
  // second leaf is the one hidden.
  await page.locator(".af-pane-host .xterm").first().click();
  await expect(page.locator(".af-pane-focused .xterm-helper-textarea")).toBeFocused();
  await page.setViewportSize({ width: 700, height: 800 });
  await expect(page.locator(".af-split-narrow")).toContainText("1 pane hidden");

  // Alt+j cycles onto the hidden leaf: the refit un-hides it (highlight moves)
  // — and the keyboard must move with it, not sit on document.body.
  await page.keyboard.press("Alt+j");
  await expect(page.locator(".af-pane-focused .xterm-helper-textarea"), "the restored pane holds the keyboard").toBeFocused();
});
