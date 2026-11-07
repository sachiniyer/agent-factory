import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";

// #6937 (Refs #4217): in WebKit, pressing the × of a tab that is left-clipped by
// the scrolled strip can ACTIVATE the tab instead of opening the Delete tab
// confirm — the shared teardown (seeded-tabs.ts) trips on it and times out.
//
// Mechanism, confirmed by instrumenting a live repro on this lane: the press on
// the × moves DOM focus to the tab button (the ×'s nearest focusable ancestor —
// a touch tap reaches the same code through its compat mousedown). WebKit
// reveals the focused element with a scroll that is armed by an Element.focus()
// call but resolves its target at FIRE time
// (LocalFrameView::scrollToFocusedElementInternal reads
// document->focusedElement() when the 0s timer runs) — and the app's own
// refocus()/term.focus() churn on every attach/reconcile means such an arm is
// routinely in flight when the press lands. The press hands focus to the
// clipped button between down and up, the armed reveal scrolls the strip
// mid-gesture, the release lands on the label, and the click's common ancestor
// resolves to activation.
//
// To keep the race deterministic the spec re-arms the reveal itself: inside the
// press's mousedown it runs the same blur→focus sequence refocus() runs on the
// xterm textarea, so the armed scroll is guaranteed to be pending when the
// press hands focus into the strip. Without the arm the reproduction is
// probabilistic (~50%); with it every press on master scrolls and activates.
//
// The product fix must hold for the PRESS: the × owns the gesture no matter how
// the strip moves under it, in WebKit and Chromium, for mouse and touch.

const title = "clipped-close";

test.use({ browserName: "webkit", hasTouch: true, launchOptions: { args: [] } });

test.afterEach(() => {
  // A spec-owned session must not leak into the shared roster.
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  if (!afBin || !mockRepo) {
    return;
  }
  try {
    execFileSync(afBin, ["sessions", "kill", title, "--repo", mockRepo], { stdio: "ignore" });
  } catch {
    // already gone
  }
});

test.beforeEach(async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });
  // The same-name fence (#4217): kill returns before the record dies, so a
  // retried create can land while the old one still holds this title — wait for
  // the Snapshot plane to show zero records with it first.
  try {
    af("sessions", "kill", title, "--repo", mockRepo as string);
  } catch {
    // not present
  }
  await expect
    .poll(async () => {
      const snap = await (await request.post("/v1/Snapshot", { data: {} })).json();
      return snap.data.instances.filter((x: { title: string }) => x.title === title).length;
    }, { timeout: 15_000 })
    .toBe(0);
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  // 14 shell tabs + the agent tab: enough scrollWidth that the strip overflows
  // under every environment's font metrics (a bare 10 fit inside CI's strip).
  for (let i = 0; i < 14; i++) {
    af("sessions", "tab-create", "--repo", mockRepo as string, title, "--kind", "shell");
  }

  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const s = snap.data.instances.find((x: { title: string }) => x.title === title);
  await page.goto(`/#/session/${encodeURIComponent(s.id)}`);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(15);
});

interface Target {
  index: number;
  label: string;
  x: number;
  y: number;
}

// Scroll the strip fully right and return the tab that is clipped by the left
// edge while its × is still entirely hittable — the geometry of both #4217
// sightings. scrollWidth - clientWidth is the strip's real max, which is where
// the press-focus reveal has distance to travel; the measured reveal delta is
// roughly clip - 48px, so the clip must be >= ~70px for the release to slide
// off the ~44px × hitbox. The boundary tab's clip depends on how scrollWidth
// and clientWidth happen to align, so a small viewport-width sweep finds a
// width whose boundary lands inside that window (over one tab pitch, ~35% of
// widths qualify — the sweep is bounded, not probabilistic).
const clippedCloseTarget = async (page: Page): Promise<Target> => {
  // A ~170px sweep spans a full tab pitch, so SOME width always lands the
  // boundary inside the window — bounded, never probabilistic.
  for (const width of [1280, 1264, 1248, 1232, 1216, 1200, 1184, 1168, 1152, 1136, 1120, 1104]) {
    await page.setViewportSize({ width, height: 800 });
    const target = await page.evaluate(async () => {
      const bar = document.querySelector(".af-tabbar") as HTMLElement;
      bar.scrollLeft = bar.scrollWidth - bar.clientWidth;
      await new Promise((resolve) => setTimeout(resolve, 60));
      const b = bar.getBoundingClientRect();
      for (const [index, tab] of [...bar.querySelectorAll<HTMLElement>(".af-tab")].entries()) {
        const close = tab.querySelector(".af-tab-close");
        if (!close) {
          continue;
        }
        const r = tab.getBoundingClientRect();
        const c = close.getBoundingClientRect();
        const clip = b.left - r.left;
        if (clip >= 75 && c.left >= b.left && c.right <= b.right) {
          return {
            index,
            label: tab.querySelector(".af-tab-label")?.textContent ?? "",
            x: (c.left + c.right) / 2,
            y: (c.top + c.bottom) / 2,
          };
        }
      }
      return null;
    });
    if (target) {
      return target as Target;
    }
  }
  throw new Error("no viewport width produced a left-clipped tab with a hittable × at max scroll");
};

// Re-arm WebKit's deferred focus-reveal inside the next mousedown's dispatch —
// the same blur→focus(xterm textarea) sequence the app's own refocus() runs on
// every attach/reconcile, landing the arm in the press's task so it is pending
// when the press hands focus to the clipped tab button.
const armFocusReveal = (page: Page) =>
  page.evaluate(() => {
    document.addEventListener(
      "mousedown",
      () => {
        (document.activeElement as HTMLElement | null)?.blur?.();
        (document.querySelector(".xterm-helper-textarea") as HTMLElement | null)?.focus();
      },
      { capture: true, once: true },
    );
  });

// The Delete confirm for THIS tab must open — not a neighbour's, not activation.
const expectDeleteConfirm = async (page: Page, target: Target) => {
  const modal = page.locator(".af-modal-card");
  await expect(
    modal.locator(".af-modal-title"),
    "pressing the × must open the Delete tab confirm for the pressed tab — never activate it",
  ).toHaveText(`Delete tab “${target.label}” from session “${title}”?`);
  await expect(modal.getByRole("button", { name: "Delete tab", exact: true })).toBeVisible();
};

test("#6937: mouse press on a left-clipped tab's × opens Delete tab", async ({ page }) => {
  const target = await clippedCloseTarget(page);
  const activeBefore = await page.locator(".af-tab.af-tab-active").evaluate((t) => [...t.parentElement!.children].indexOf(t));
  await armFocusReveal(page);

  await page.mouse.move(target.x, target.y);
  await page.mouse.down();
  // Hold long enough for the armed 0s reveal to fire inside the gesture — on
  // master this scrolls the strip and the release lands on the tab's label.
  await page.waitForTimeout(200);
  await page.mouse.up();

  await expectDeleteConfirm(page, target);
  // The press that began on the × must never have handed DOM focus to a tab
  // button — that focus move is what makes the tab the reveal's target.
  expect(await page.evaluate(() => document.activeElement?.className ?? "")).not.toContain("af-tab");
  // Activation must be exactly where the press found it.
  const activeAfter = await page.locator(".af-tab.af-tab-active").evaluate((t) => [...t.parentElement!.children].indexOf(t));
  expect(activeAfter, "the press must not have re-activated a different tab").toBe(activeBefore);
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-modal-card")).toHaveCount(0);
});

test("#6937: touch tap on a left-clipped tab's × opens Delete tab", async ({ page }) => {
  const target = await clippedCloseTarget(page);
  const activeBefore = await page.locator(".af-tab.af-tab-active").evaluate((t) => [...t.parentElement!.children].indexOf(t));
  // The tap's compat mousedown is what takes focus, so the same arm applies —
  // it is installed now and fires on that compat dispatch inside the tap.
  await armFocusReveal(page);

  await page.touchscreen.tap(target.x, target.y);

  await expectDeleteConfirm(page, target);
  const activeAfter = await page.locator(".af-tab.af-tab-active").evaluate((t) => [...t.parentElement!.children].indexOf(t));
  expect(activeAfter, "the tap must not have re-activated a different tab").toBe(activeBefore);
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-modal-card")).toHaveCount(0);
});
