import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";

// #6408: at 1024px and at 844x390 (phone landscape) the pane-header tab strip
// clips tabs mid-word with nothing to say more exist — a user concludes the
// session has one tab. The strip already scrolls (overflow-x: auto), so every
// tab is reachable; what it owes the user is the SIGNAL: a fade at whichever
// edge has off-screen tabs. (.af-tabbar-more-right when scrollWidth runs past
// the right edge, .af-tabbar-more-left once the user has scrolled away from
// the left.) The "+N more" counter and the strip's own row are the later P18
// redesign (#6436) — this is the minimal cue.

const title = "strip-overflow";

async function scrollState(page: Page): Promise<{ moreLeft: boolean; moreRight: boolean; overflows: boolean }> {
  return page.evaluate(() => {
    const bar = document.querySelector(".af-tabbar") as HTMLElement;
    return {
      moreLeft: bar.classList.contains("af-tabbar-more-left"),
      moreRight: bar.classList.contains("af-tabbar-more-right"),
      overflows: bar.scrollWidth > bar.clientWidth + 1,
    };
  });
}

test("#6408: the tab strip signals off-screen tabs at 1024px and 844x390", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  for (let i = 0; i < 14; i++) af("sessions", "tab-create", "--repo", mockRepo as string, title, "--kind", "shell");

  await page.setViewportSize({ width: 1024, height: 768 });
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const s = snap.data.instances.find((x: { title: string }) => x.title === title);
  await page.goto(`/#/session/${encodeURIComponent(s.id)}`);
  const bar = page.locator(".af-tabbar");
  await expect(bar.locator(".af-tab")).toHaveCount(15);

  // The issue's layout: tabs do not fit at 1024. The strip must SAY so at the
  // right edge — not just clip the next tab mid-word.
  expect((await scrollState(page)).overflows, "the repro overflows the strip").toBe(true);
  await expect(bar, "an overflowing strip signals at the right edge").toHaveClass(/af-tabbar-more-right/);

  // Reachable means reachable: the last tab scrolls fully into the strip's
  // visible box, and the left-edge cue appears once content exists off the left.
  await bar.evaluate((el) => (el.scrollLeft = el.scrollWidth));
  await expect(bar, "scrolled to the end, the strip signals content off the left").toHaveClass(/af-tabbar-more-left/);
  await expect(bar, "at the right edge there is no more content to signal").not.toHaveClass(/af-tabbar-more-right/);
  const last = bar.locator(".af-tab").last();
  const inside = await page.evaluate(() => {
    const barEl = document.querySelector(".af-tabbar") as HTMLElement;
    const tabEl = [...document.querySelectorAll(".af-tabbar .af-tab")].pop() as HTMLElement;
    const b = barEl.getBoundingClientRect();
    const r = tabEl.getBoundingClientRect();
    return r.left >= b.left - 1 && r.right <= b.right + 1;
  });
  await expect(last).toBeAttached();
  expect(inside, "the last tab is fully visible after scrolling to it").toBe(true);

  // The issue's second repro: phone landscape. Same strip, same duty.
  await page.setViewportSize({ width: 844, height: 390 });
  await expect(bar, "844x390 still signals the off-screen tabs").toHaveClass(/af-tabbar-more-left|af-tabbar-more-right/);

  // The cue must not cry wolf: scrolled back to the start there is nothing
  // off the left to signal, and the right-edge cue tracks the truth at
  // whatever width the strip has — on exactly while it still overflows.
  await bar.evaluate((el) => (el.scrollLeft = 0));
  await expect(bar, "back at the start, no off-screen content off the left").not.toHaveClass(/af-tabbar-more-left/);
  await expect
    .poll(async () => {
      const s = await scrollState(page);
      return s.moreRight === s.overflows;
    }, { message: "the right-edge cue is on iff content is off the right edge" })
    .toBe(true);
});
