// The web demo recorder (#3855 lane A) — the moving picture the README and the
// docs home page lead with, produced by `make demo-assets`.
//
// It is NOT a gate, and it must never become one. It shares the self-test's
// harness on purpose (the same container image, the same real af daemon on a
// throwaway home, the same loopback tokenless browser) so what the docs show is
// the product the self-test asserts on — but it is reached only through its own
// config (playwright.demo.config.ts), CI never runs it, and nothing here is an
// assertion about correctness. The `expect`s below are waits: they are how a
// recorder knows a beat has actually landed before it takes the picture, and
// the alternative — sleeping for a plausible duration — is what produces a
// README hero with a half-painted pane in it.
//
// The pass runs twice, once per theme, each in its own browser context:
//
//   1. dashboard        the project's sessions in the rail, the selected one's
//                       agent tab, and its PR badge
//   2. new-session      the new-session modal, filled in
//   3. agent-tab        the session that create just made, streaming
//   4. review           the branch's own diff in a tab, beside the PR link
//   5. tasks            the Tasks view
//   6. config-accounts  the Config view, at the Accounts section
//
// Video is recorded for the default-theme pass only: one take is what a hero
// needs, and a second one would double the committed media for a view the
// stills already cover.
//
// Everything the recording needs is handed in via env by
// scripts/container/web-demo-entry.sh (see playwright.demo.config.ts).

import { expect, type Browser, type Locator, type Page, test } from "@playwright/test";
import { join } from "node:path";
import { openAfterInitialResync } from "./initial-resync.js";
import { DEMO_VIEWPORT } from "./demo-viewport.js";

const SHOT_DIR = required("AF_DEMO_SHOT_DIR");
const VIDEO_DIR = required("AF_DEMO_VIDEO_DIR");
const SESSION_JSON = process.env.AF_DEMO_SESSION_JSON ?? "add-json-export";
const SESSION_USAGE = process.env.AF_DEMO_SESSION_USAGE ?? "fix-empty-add";
const SESSION_DOCS = process.env.AF_DEMO_SESSION_DOCS ?? "document-cli";
const SESSION_NEW = process.env.AF_DEMO_SESSION_NEW ?? "tidy-tests";

/** The prompt typed into the new-session modal on camera. Short enough to read
 *  in a moving frame, and it is the prompt the stand-in agent's `tests` role
 *  actually answers (scripts/container/web-demo-agent.sh). */
const NEW_SESSION_PROMPT = "Cover appending a second item, and print one line per case.";

function required(name: string): string {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} is unset — run the recorder through \`make demo-assets\`.`);
  }
  return value;
}

/** A rail row by its session title. No seeded title is a substring of another,
 *  so this matches exactly one row (the constraint web-demo-entry.sh keeps). */
function row(page: Page, title: string): Locator {
  return page.locator(".af-rail-list .af-row", { hasText: title });
}

/**
 * Waits until the pane has stopped changing, then returns.
 *
 * A pane is not finished the moment its socket opens. af sizes a tmux pane from
 * the client attached to it, and every pane here was created while the session
 * was being SEEDED, with no browser in existence — so it starts at tmux's
 * default 80 columns and is resized a beat after the browser attaches, at which
 * point tmux reflows and the stand-in repaints (see
 * scripts/container/web-demo-agent.sh). A screenshot taken between the attach
 * and that repaint catches an 80-column transcript sitting inside a much wider
 * pane: not wrong, but it reads as a broken frame, and it is the frame the
 * README leads with.
 *
 * Two identical samples `ms` apart is the general form of "that has happened",
 * and it needs no marker in the content — which matters because the content
 * differs per beat.
 */
async function settleTerminal(page: Page, ms = 1_200): Promise<void> {
  let previous: string | null = null;
  await expect
    .poll(
      async () => {
        const now = await page.evaluate(() =>
          Array.from(document.querySelectorAll(".af-term-host .xterm-rows > div"))
            .map((r) => r.textContent ?? "")
            .join("\u0000"),
        );
        const stable = previous !== null && now === previous && now.trim() !== "";
        previous = now;
        return stable;
      },
      {
        intervals: [ms],
        timeout: 45_000,
        message: "the pane must stop repainting before it is photographed",
      },
    )
    .toBe(true);
}

/**
 * Holds the frame still for `ms`.
 *
 * A fixed wait is a smell in a test and the right tool in a recorder: the video
 * is the artifact, and a beat that changes the instant it is reachable reads as
 * a jump cut. Every wait that establishes STATE below is an `expect`; these are
 * only pacing.
 */
async function beat(page: Page, ms = 1_200): Promise<void> {
  await page.waitForTimeout(ms);
}

interface Pass {
  /** Appended to each still's name: "" for the default theme, "-dark" for dark. */
  suffix: string;
  colorScheme: "light" | "dark";
  video: boolean;
  /** Whether this pass SUBMITS the new-session modal. Exactly one pass may: the
   *  second runs against the state the first left behind, and creating
   *  `tidy-tests` twice would collide on the title rather than record anything.
   *  The pass that does not submit still opens and fills the form — that beat is
   *  the picture — and then cancels out of it. */
  createsSession: boolean;
  /** Rail rows to wait for on open: the seeded three, plus the session the
   *  first pass created. */
  seededRows: number;
}

async function record(browser: Browser, pass: Pass): Promise<void> {
  const context = await browser.newContext({
    viewport: DEMO_VIEWPORT,
    // The app ships `auto`, so the theme it renders is the one the viewer's OS
    // asks for. Driving that rather than clicking the appbar toggle means both
    // passes show the DEFAULT setting, which is what a new user will see.
    colorScheme: pass.colorScheme,
    recordVideo: pass.video ? { dir: VIDEO_DIR, size: DEMO_VIEWPORT } : undefined,
  });
  const page = await context.newPage();
  const video = page.video();
  const shot = (name: string) => page.screenshot({ path: join(SHOT_DIR, `${name}${pass.suffix}.png`) });

  try {
    // --- 1. the dashboard --------------------------------------------------
    await openAfterInitialResync(page, async () => {
      await page.goto("/");
    });
    await expect(page.locator(".af-rail-list .af-row")).toHaveCount(pass.seededRows);
    await beat(page, 1_400);

    // Scan across the seeded agents before settling on one. It is the motion the
    // rail exists for — several agents, one screen, no attaching to any of them
    // — and it also gives each of those panes its first attach, which is what
    // resizes it off tmux's seeded 80 columns (see settleTerminal).
    for (const title of [SESSION_USAGE, SESSION_DOCS]) {
      await row(page, title).click();
      await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
      await expect(page.locator(".af-term-host")).toContainText("review it like any branch");
      await beat(page, 1_200);
    }

    await row(page, SESSION_JSON).click();
    await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
    // The stand-in's last line, so the pane is showing finished work rather
    // than a blank terminal that has only just attached.
    await expect(page.locator(".af-term-host")).toContainText("review it like any branch");
    // The daemon discovered the branch's PR before the recording started
    // (web-demo-entry.sh waits for the sweep), so the badge is part of beat 1.
    await expect(page.locator(".af-pr-badge")).toBeVisible();
    await settleTerminal(page);
    await beat(page, 1_200);
    await shot("dashboard");

    // --- 2. the new-session modal ------------------------------------------
    await page.locator("button.af-rail-new").click();
    const modal = page.locator(".af-modal-card");
    await expect(modal).toBeVisible();
    await beat(page, 600);
    await modal.locator('input[aria-label="Session title"]').pressSequentially(SESSION_NEW, { delay: 55 });
    await beat(page, 400);
    await modal.locator('textarea[aria-label="Initial prompt"]').pressSequentially(NEW_SESSION_PROMPT, {
      delay: 14,
    });
    // The backend and account pickers are filled from the daemon, not from a
    // list in the browser. Waiting for the answer keeps the form in the frame
    // complete rather than showing its placeholder-only first paint.
    await expect(modal.locator('select[aria-label="Account"] option')).not.toHaveCount(1);
    await expect(modal.locator('select[aria-label="Backend"] option')).not.toHaveCount(1);
    await beat(page, 1_800);
    await shot("new-session");

    // --- 3. the agent tab, streaming ---------------------------------------
    if (pass.createsSession) {
      await modal.locator("button.af-primary").click();
    } else {
      await modal.locator(".af-modal-foot button.af-ghost").click();
    }
    await expect(modal).toBeHidden();
    await expect(row(page, SESSION_NEW)).toBeVisible({ timeout: 90_000 });
    // Selecting it explicitly rather than relying on create's own selection:
    // the recording must be looking at the new session's pane whatever the
    // create path decides to focus.
    await row(page, SESSION_NEW).click();
    await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open", { timeout: 90_000 });
    await expect(page.locator(".af-term-host")).toContainText("demo-agent", { timeout: 90_000 });
    if (pass.createsSession) {
      // Mid-transcript, so the still catches the agent working rather than done.
      await expect(page.locator(".af-term-host")).toContainText("running ./test.sh", { timeout: 90_000 });
    }
    await settleTerminal(page);
    await beat(page, 900);
    await shot("agent-tab");
    await expect(page.locator(".af-term-host")).toContainText("review it like any branch", {
      timeout: 90_000,
    });
    await beat(page, 1_400);

    // --- 4. review: the branch's diff, beside its PR -----------------------
    await row(page, SESSION_JSON).click();
    await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
    await beat(page, 800);
    await page.locator(".af-tabbar .af-tab", { hasText: "diff" }).click();
    // The LAST line the tab prints, not its first. A terminal shows its bottom,
    // and reading the top of the output is only safe because demo-diff bounds
    // itself to fewer lines than the pane has rows — a property that belongs to
    // that script, not to this wait. Waiting on the bottom line means the wait
    // still says "the diff tab is up" if the budget is ever exceeded, and the
    // still that follows is what shows whether it still reads well.
    await expect(page.locator(".af-term-host")).toContainText("review it like any other", {
      timeout: 60_000,
    });
    await expect(page.locator(".af-term-host")).toContainText("git diff --stat", { timeout: 60_000 });
    await expect(page.locator(".af-pr-badge")).toBeVisible();
    await settleTerminal(page);
    await beat(page, 1_200);
    await shot("review");

    // --- 5. the Tasks view -------------------------------------------------
    await page.locator('.af-viewtab[data-view="tasks"]').click();
    await expect(page.locator(".af-tasks")).toBeVisible();
    await expect(page.locator(".af-tasks .af-task-row")).toHaveCount(2);
    await beat(page, 2_000);
    await shot("tasks");

    // --- 6. the Config view, at the Accounts section -----------------------
    await page.locator('.af-viewtab[data-view="config"]').click();
    await expect(page.locator(".af-config")).toBeVisible();
    const accounts = page.locator(".af-accounts");
    await expect(accounts).toBeVisible();
    await expect(accounts.locator(".af-accounts-row")).not.toHaveCount(0);
    await accounts.scrollIntoViewIfNeeded();
    await beat(page, 2_000);
    await shot("config-accounts");

    // Back where it started, so the video ends on the screen it opened on
    // rather than mid-settings.
    await page.locator('.af-viewtab[data-view="sessions"]').click();
    await expect(page.locator(".af-rail-list")).toBeVisible();
    await beat(page, 1_400);
  } finally {
    await context.close();
  }

  if (pass.video) {
    // saveAs waits for the recording to be flushed, which only happens once the
    // context is closed — hence the ordering.
    await video?.saveAs(join(VIDEO_DIR, "demo-raw.webm"));
  }
}

test("web demo · default theme", async ({ browser }) => {
  await record(browser, {
    suffix: "",
    colorScheme: "light",
    video: true,
    createsSession: true,
    seededRows: 3,
  });
});

test("web demo · dark", async ({ browser }) => {
  // Runs against the state the first pass left behind: four sessions in the
  // rail, and a new-session modal that gets filled in and then cancelled.
  await record(browser, {
    suffix: "-dark",
    colorScheme: "dark",
    video: false,
    createsSession: false,
    seededRows: 4,
  });
});
