import { expect, test, type Page } from "@playwright/test";
import { seededTabs } from "./seeded-tabs.js";

// #6576: the #6562 pendingRefocus outlives an explicit detach. Alt+j onto a
// fit-hidden pane queues the keyboard handoff for the refit frame; a Ctrl+]
// inside that window runs focusRail() → blur() — but blur() never cancelled
// the queued intent, so the refit frame re-focused the restored terminal and
// undid the detach. Keystrokes then reach the agent while the user thinks
// they are on the rail — the silent wrong-target class the mechanism exists
// to prevent. Any explicit user focus action in that window must cancel the
// pending refocus: deferred focus never overrides a later user choice.

const sessionA = process.env.AF_WEB_SESSION_A ?? "probe-a";
// probe-a's roster is shared with the suite — close what this test adds (#4217).
const roster = seededTabs();

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

test("#6576: Ctrl+] inside the queued refit frame cancels the pending refocus", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === sessionA);
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await page.setViewportSize({ width: 1280, height: 800 });

  // probe-a's tab roster is shared with the suite — count relative to it.
  // Two shells: a new shell is SELECTED on create, so the second stays shown
  // in the pane while the FIRST is dragged — dragging a shown tab would hit
  // the dedupe/companion paths in applyTabDrop instead of a real split.
  const tabs = page.locator(".af-tabbar .af-tab");
  const base = await roster.enter(page);
  await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  await expect(tabs).toHaveCount(base + 1);
  await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
  await expect(tabs).toHaveCount(base + 2);
  await dragTabToPane(page, base, 0, "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);

  // Put the keyboard in the FIRST pane's terminal, then narrow until the
  // second leaf is the one hidden (~580px of host at 900px fits one 320px
  // pane; the >768px width keeps the desktop rail so the host stays narrow).
  await page.locator(".af-pane-host .xterm").first().click();
  await expect(page.locator(".af-pane-focused .xterm-helper-textarea")).toBeFocused();
  await page.setViewportSize({ width: 900, height: 800 });
  await expect(page.locator(".af-split-narrow")).toContainText("1 pane hidden");

  // Dispatch both chords synchronously so the detach lands INSIDE the frame
  // in which the refocus is queued — real key presses are a protocol round
  // trip each, and the frame could run between them and make the window moot.
  // Alt+j cycles onto the hidden leaf (queues pendingRefocus + the refit);
  // Ctrl+] is the detach (focusRail → blur). The app's document-level handler
  // has no isTrusted guard, so synthetic events exercise the real path.
  await page.evaluate(() => {
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "j", altKey: true, bubbles: true, cancelable: true }));
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "]", ctrlKey: true, bubbles: true, cancelable: true }));
  });

  // The refit still ran — cycling focus protects the second leaf, so the fit
  // un-hides it (the first leaf takes its place in the hidden set). Only the
  // deferred FOCUS is cancelled; the layout pass the focus move scheduled is
  // not. Visible + focused = the frame fired.
  const restored = page.locator(".af-term-host .af-pane").nth(1);
  await expect(restored).toHaveClass(/af-pane-focused/);
  await expect(restored).toBeVisible();

  // The detach holds: the refit frame must NOT have re-focused the restored
  // pane's terminal after the explicit blur. Under the bug the textarea is
  // focused again here — pendingRefocus fired after blur() ran.
  await expect(restored.locator(".xterm-helper-textarea")).not.toBeFocused();
});
