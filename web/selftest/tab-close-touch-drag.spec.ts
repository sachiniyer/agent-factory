// Touch long-press on a tab's × must not arm the drag (#6937 parity with #2899).
//
// The bar's touch pointerdown (attachTabTouchDrag) resolves its press target with
// closest(".af-tab"), and .af-tab-close is a child of the tab button — so without
// excluding the ×, a touch press on it arms the 500 ms long-press pick-up exactly
// like a label press. The held gesture then suppresses strip scroll and, on
// release, commits a reorder or a pane split/replace, bypassing the close confirm
// entirely.
//
// The mouse path is already protected: the ×'s mousedown preventDefault suppresses
// dragstart. These specs pin the touch parity: a press that begins on the × never
// lifts the tab into a drag, so the close flow owns the gesture — while a press on
// the label still arms the drag (the un-dragged reference behavior of #2899).
//
// They dispatch real PointerEvents the same way tab-touch-drag.spec.ts does:
// pointerdown on the ×, wait out the hold, then drive pointermove/pointerup on the
// bar. They run only inside the sanctioned container (make web-selftest-container).

import { test, expect, type APIRequestContext, type Page } from "@playwright/test";

const POINTER_ID = 43;

/** A non-agent tab in the rename/reorder session, which has [agent, alpha, beta, gamma]. */
const MOVED_TAB = 1;

async function openOrderSession(page: Page, request: APIRequestContext): Promise<void> {
  await page.setViewportSize({ width: 390, height: 844 });
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find(
    (s: { title: string }) => s.title === (process.env.AF_WEB_SESSION_ORDER ?? "probe-order"),
  );
  expect(session?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  // The session-first (phone) composition reparents the tab bar into the "More app
  // controls" disclosure, which is collapsed by default — so the bar (and its tabs)
  // are display:none until that disclosure is opened. Open it to make the ×
  // long-pressable, the same way tab-touch-drag.spec.ts does.
  const controls = page.getByRole("button", { name: "More app controls", exact: true });
  await controls.click();
  await expect(controls).toHaveAttribute("aria-expanded", "true");
  await expect(page.locator(`.af-tab[data-tab-index="${MOVED_TAB}"]`)).toBeVisible();
}

async function tabLabels(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll<HTMLElement>(".af-tabbar .af-tab"))
      .map((t) => t.getAttribute("data-tab-index") + ":" + (t.textContent ?? "").trim()),
  );
}

/** Dispatch a touch pointerdown on the × of the given tab and return its center. */
async function pressClose(page: Page, tabIndex: number): Promise<{ x: number; y: number }> {
  const close = page.locator(`.af-tab[data-tab-index="${tabIndex}"] .af-tab-close`);
  await expect(close).toBeVisible();
  return close.evaluate((el, pointerId) => {
    const r = el.getBoundingClientRect();
    el.dispatchEvent(new PointerEvent("pointerdown", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch",
      clientX: r.left + r.width / 2, clientY: r.top + r.height / 2,
    }));
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  }, POINTER_ID);
}

async function movePointer(page: Page, x: number, y: number): Promise<void> {
  await page.locator(".af-tabbar").evaluate((bar, { x, y, pointerId }) => {
    bar.dispatchEvent(new PointerEvent("pointermove", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch", clientX: x, clientY: y,
    }));
  }, { x, y, pointerId: POINTER_ID });
}

async function releasePointer(page: Page, x: number, y: number): Promise<void> {
  await page.locator(".af-tabbar").evaluate((bar, { x, y, pointerId }) => {
    bar.dispatchEvent(new PointerEvent("pointerup", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch", clientX: x, clientY: y,
    }));
  }, { x, y, pointerId: POINTER_ID });
}

test("a touch long-press on the × does not arm the tab drag", async ({ page, request }) => {
  await openOrderSession(page, request);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(4);

  const before = await tabLabels(page);
  await pressClose(page, MOVED_TAB);

  // Wait past the 500 ms hold threshold (TAB_PRESS_LIMITS.holdMs). With the bug,
  // the body gains af-dragging-tab at this point (pickUp ran); with the fix the
  // ×'s press never armed the drag, so the class must not appear.
  await page.waitForTimeout(700);
  await expect(page.locator("body")).not.toHaveClass(/af-dragging-tab/);
  await expect(page.locator(".af-tabbar")).not.toHaveClass(/af-tabbar-dragging/);
  await expect(page.locator(".af-tab-insert")).toBeHidden();

  // Move and release over a different gap in the bar — with the bug this reorders
  // the tab; with the fix the drag was never armed so the roster is unchanged.
  const on = await page.locator(".af-tabbar").evaluate((bar) => {
    const r = bar.getBoundingClientRect();
    return { x: r.left + r.width * 0.8, y: r.top + r.height / 2 };
  });
  await movePointer(page, on.x, on.y);
  await releasePointer(page, on.x, on.y);

  await expect(page.locator("body")).not.toHaveClass(/af-dragging-tab/);
  expect(await tabLabels(page)).toEqual(before);
});

test("a touch long-press on the × released over a pane does not split or replace", async ({ page, request }) => {
  await openOrderSession(page, request);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(4);

  const before = await tabLabels(page);
  await pressClose(page, MOVED_TAB);

  await page.waitForTimeout(700);
  await expect(page.locator("body")).not.toHaveClass(/af-dragging-tab/);

  // Move over a pane and release — with the bug this hands the payload to
  // dropTabOnPaneAt (split or pane replacement); with the fix the drag was never
  // armed so nothing happens and the roster is unchanged.
  const pane = await page.locator(".af-term-host .af-pane").first().evaluate((el) => {
    const r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  });
  await movePointer(page, pane.x, pane.y);
  await releasePointer(page, pane.x, pane.y);

  expect(await tabLabels(page)).toEqual(before);
});

test("a touch long-press on the × still opens the Delete-tab confirm", async ({ page, request }) => {
  await openOrderSession(page, request);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(4);

  const point = await pressClose(page, MOVED_TAB);

  // Wait past the hold: the drag must NOT arm, and the close flow must still own
  // the gesture. A compat click synthesized on the release resolves to the ×'s
  // own click handler (or the #6937 window guard's closeTabByIdentity), opening
  // the Delete-tab confirm.
  await page.waitForTimeout(700);
  await expect(page.locator("body")).not.toHaveClass(/af-dragging-tab/);

  // Synthesize the lift's compat click on the × — the close flow's trigger.
  await page.locator(`.af-tab[data-tab-index="${MOVED_TAB}"] .af-tab-close`).evaluate((el, { p, pointerId }) => {
    const base = { bubbles: true, cancelable: true, clientX: p.x, clientY: p.y, button: 0 };
    el.dispatchEvent(new PointerEvent("pointerup", {
      ...base, pointerId, pointerType: "touch", isPrimary: true,
    }));
    el.dispatchEvent(new MouseEvent("mousedown", base));
    el.dispatchEvent(new MouseEvent("mouseup", base));
    el.dispatchEvent(new MouseEvent("click", base));
  }, { p: point, pointerId: POINTER_ID });

  // The Delete-tab confirm must open for the pressed tab — never a reorder/split.
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible({ timeout: 5_000 });
  await expect(modal.getByRole("button", { name: "Delete tab", exact: true })).toBeVisible();
  // Dismiss without confirming — do not mutate the shared probe-order session.
  await page.keyboard.press("Escape");
  await expect(modal).toHaveCount(0);
});

test("a touch long-press on the tab label still arms the drag (regression)", async ({ page, request }) => {
  await openOrderSession(page, request);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(4);

  // Press on the tab label (not the ×) — the drag must still arm, exactly as
  // tab-touch-drag.spec.ts verifies. This confirms the × exclusion did not
  // accidentally suppress the label's own long-press pickup.
  const tab = page.locator(`.af-tab[data-tab-index="${MOVED_TAB}"]`);
  await tab.evaluate((el, pointerId) => {
    const r = el.getBoundingClientRect();
    // Press on the label area, avoiding the × which is at the right edge.
    el.dispatchEvent(new PointerEvent("pointerdown", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch",
      clientX: r.left + r.width * 0.3, clientY: r.top + r.height / 2,
    }));
  }, POINTER_ID);

  await expect(page.locator("body")).toHaveClass(/af-dragging-tab/, { timeout: 2_000 });

  // Cancel the drag to avoid persisting a reorder of the shared probe-order session.
  await page.locator(".af-tabbar").evaluate((bar, pointerId) => {
    bar.dispatchEvent(new PointerEvent("pointercancel", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch",
    }));
  }, POINTER_ID);
  await expect(page.locator("body")).not.toHaveClass(/af-dragging-tab/);
});
