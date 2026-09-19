// Touch tab-drag feedback: the insertion indicator mirrors the release (#2899).
//
// The `pointermove` handler in `attachTabTouchDrag` decides what feedback to show
// while a finger drags a picked-up tab. It must agree with `pointerup` about which
// region is "the bar": over the bar → insertion gap; over a pane → split zone; over
// neither (the header/appbar/empty space) → nothing, because a release there is a
// CANCEL. The bug showed the bar's insertion indicator for ANY non-pane location, so
// it tracked a finger that had slipped off the bar onto the appbar right up to a
// release that did nothing.
//
// These specs dispatch real PointerEvents the same way `new-tab-picker.spec.ts`'s
// "phone touch pane drop" case does: pointerdown on a tab, wait out the hold for the
// `af-dragging-tab` pick-up, then drive pointermove/pointerup on the captured bar.
// They run only inside the sanctioned container (`make web-selftest-container`).

import { test, expect, type APIRequestContext, type Page } from "@playwright/test";

const POINTER_ID = 42;

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
  // are display:none until that disclosure is opened. Open it to make the bar
  // long-pressable, the same way the existing phone touch-drag test does.
  const controls = page.getByRole("button", { name: "More app controls", exact: true });
  await controls.click();
  await expect(controls).toHaveAttribute("aria-expanded", "true");
  await expect(page.locator(`.af-tab[data-tab-index="${MOVED_TAB}"]`)).toBeVisible();
}

/** Long-press a tab past the hold so `pickUp` runs. Returns the pointerdown point. */
async function pickUpTab(page: Page, tabIndex: number): Promise<{ x: number; y: number }> {
  const tab = page.locator(`.af-tab[data-tab-index="${tabIndex}"]`);
  await tab.scrollIntoViewIfNeeded();
  const point = await tab.evaluate((el, pointerId) => {
    const r = el.getBoundingClientRect();
    el.dispatchEvent(new PointerEvent("pointerdown", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch",
      clientX: r.left + r.width / 2, clientY: r.top + r.height / 2,
    }));
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  }, POINTER_ID);
  // The hold (TAB_PRESS_LIMITS.holdMs = 500ms) fires the pick-up.
  await expect(page.locator("body")).toHaveClass(/af-dragging-tab/, { timeout: 2_000 });
  return point;
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

/** A point that is over the appbar/header row (above the bar), NOT over the bar or a
 *  pane. Kept within the bar's horizontal range so the bug's `insertionIndexAt` would
 *  otherwise resolve to a real gap — the report's "finger slipped vertically off the
 *  bar" case. Returns the point and the elementFromPoint hit for an assertion. */
async function appBarPoint(page: Page): Promise<{ x: number; y: number; hitInBar: boolean; hitInPane: boolean }> {
  return page.locator(".af-tabbar").evaluate((bar) => {
    const r = bar.getBoundingClientRect();
    const x = r.left + r.width / 2;
    const y = Math.max(r.top - 40, 6);
    const hit = document.elementFromPoint(x, y);
    return {
      x, y,
      hitInBar: !!hit && bar.contains(hit),
      hitInPane: !!hit && !!hit.closest(".af-pane"),
    };
  });
}

/** A point guaranteed to be inside the bar's VISIBLE rect, so it hit-tests to the bar
 *  even when the bar is horizontally scrolled and the chosen tab is off-screen. The
 *  bar's own element owns the empty space around its tabs, so a point at `fracX` of the
 *  bar's width is over the bar (not a pane, not the appbar) for any `fracX` in (0,1).
 *  Returns the point and whether elementFromPoint confirmed it is in the bar. */
async function barInteriorPoint(page: Page, fracX: number): Promise<{ x: number; y: number; hitInBar: boolean }> {
  return page.locator(".af-tabbar").evaluate((bar, fracX) => {
    const r = bar.getBoundingClientRect();
    const x = r.left + r.width * fracX;
    const y = r.top + r.height / 2;
    const hit = document.elementFromPoint(x, y);
    return { x, y, hitInBar: !!hit && bar.contains(hit) };
  }, fracX);
}

async function panePoint(page: Page): Promise<{ x: number; y: number; hasPane: boolean }> {
  return page.locator(".af-term-host .af-pane").first().evaluate((el) => {
    const r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2, hasPane: r.width > 0 && r.height > 0 };
  });
}

async function tabLabels(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll<HTMLElement>(".af-tabbar .af-tab"))
      .map((t) => t.getAttribute("data-tab-index") + ":" + (t.textContent ?? "").trim()),
  );
}

test("A: over the appbar the insertion indicator is hidden and the release cancels", async ({ page, request }) => {
  await openOrderSession(page, request);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(4);

  const before = await tabLabels(page);
  await pickUpTab(page, MOVED_TAB);
  // Pick-up draws the marker at the source tab's own gap — the baseline cue.
  await expect(page.locator(".af-tab-insert")).toBeVisible();

  const off = await appBarPoint(page);
  expect(off.hitInBar, "the off-bar point must not hit-test to the bar").toBe(false);
  expect(off.hitInPane, "the off-bar point must not hit-test to a pane").toBe(false);
  await movePointer(page, off.x, off.y);
  // THE BUG: this was visible while the finger was over the appbar. After the fix it
  // is hidden, mirroring the cancel a release there performs.
  await expect(page.locator(".af-tab-insert")).toBeHidden();

  await releasePointer(page, off.x, off.y);
  await expect(page.locator("body")).not.toHaveClass(/af-dragging-tab/);
  // release() hides the indicator on lift regardless of where the finger lands.
  await expect(page.locator(".af-tab-insert")).toBeHidden();
  // A release over the appbar is a CANCEL: the roster is unchanged.
  expect(await tabLabels(page)).toEqual(before);
});

test("B: over the bar the insertion indicator is shown", async ({ page, request }) => {
  await openOrderSession(page, request);
  await pickUpTab(page, MOVED_TAB);
  await expect(page.locator(".af-tab-insert")).toBeVisible();

  // A point inside the bar's visible rect, to the right of the source tab's gap, so
  // the indicator is drawn at a different gap. Using the bar's own rect (not a tab's)
  // avoids choosing a tab that is horizontally scrolled off the 390px bar.
  const on = await barInteriorPoint(page, 0.8);
  expect(on.hitInBar, "the on-bar point must hit-test to the bar").toBe(true);
  await movePointer(page, on.x, on.y);
  await expect(page.locator(".af-tab-insert")).toBeVisible();
  await releasePointer(page, on.x, on.y);
  await expect(page.locator(".af-tab-insert")).toBeHidden();
});

test("D: moving off the bar and back re-shows the indicator exactly over the bar", async ({ page, request }) => {
  await openOrderSession(page, request);
  await pickUpTab(page, MOVED_TAB);
  await expect(page.locator(".af-tab-insert")).toBeVisible();

  const off = await appBarPoint(page);
  await movePointer(page, off.x, off.y);
  await expect(page.locator(".af-tab-insert")).toBeHidden();

  const on = await barInteriorPoint(page, 0.6);
  expect(on.hitInBar, "the on-bar point must hit-test to the bar").toBe(true);
  await movePointer(page, on.x, on.y);
  await expect(page.locator(".af-tab-insert")).toBeVisible();

  await releasePointer(page, on.x, on.y);
  await expect(page.locator(".af-tab-insert")).toBeHidden();
});

test("C: over a pane the pane drop zone shows and the bar indicator is hidden", async ({ page, request }) => {
  await openOrderSession(page, request);
  await pickUpTab(page, MOVED_TAB);
  await expect(page.locator(".af-tab-insert")).toBeVisible();

  const p = await panePoint(page);
  expect(p.hasPane, "a focused pane must be present to hit-test").toBe(true);
  await movePointer(page, p.x, p.y);
  // The pane owns the cue: its drop overlay shows and the bar's gap marker hides.
  await expect(page.locator(".af-drop-overlay.af-drop-show")).toBeVisible();
  await expect(page.locator(".af-tab-insert")).toBeHidden();

  await releasePointer(page, p.x, p.y);
  await expect(page.locator(".af-tab-insert")).toBeHidden();
});

test("G: a pointercancel clears the drag indicator and state", async ({ page, request }) => {
  await openOrderSession(page, request);
  await pickUpTab(page, MOVED_TAB);
  await expect(page.locator(".af-tab-insert")).toBeVisible();

  await page.locator(".af-tabbar").evaluate((bar, pointerId) => {
    bar.dispatchEvent(new PointerEvent("pointercancel", {
      bubbles: true, cancelable: true, pointerId, pointerType: "touch",
    }));
  }, POINTER_ID);
  await expect(page.locator("body")).not.toHaveClass(/af-dragging-tab/);
  await expect(page.locator(".af-tabbar")).not.toHaveClass(/af-tabbar-dragging/);
  await expect(page.locator(".af-tab-insert")).toBeHidden();
});
