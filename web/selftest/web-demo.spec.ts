// The web demo recorder (#3855 lane A) — the moving picture the README and the
// docs home page lead with, produced by `make demo-assets`.
//
// The paced video is not a gate. The still-only visual config (#3908) reuses
// these same beats in CI, without video or pacing. It shares the self-test's
// harness on purpose (the same container image, the same real af daemon on a
// throwaway home, the same loopback tokenless browser) so what the docs show is
// the product the self-test asserts on — but it is reached only through its own
// video config (playwright.demo.config.ts), and the recorder is not an
// assertion about correctness. The `expect`s below are waits: they are how a
// recorder knows a beat has actually landed before it takes the picture, and
// the alternative — sleeping for a plausible duration — is what produces a
// README hero with a half-painted pane in it.
//
// The pass runs twice, once per theme, each in its own browser context:
//
//   1. dashboard        the project's sessions in the rail, the selected one's
//                       agent tab
//   2. new-session      the new-session modal, filled in
//   3. agent-tab        the session that create just made, streaming
//   4. review           the branch's own diff in a tab
//   5. tasks            the Tasks view
//   6. config-accounts  the Config view, at the Accounts section
//
// Additional stills cover parallel work, comparison review, and filled cron/
// watch task forms. Those forms are cancelled so no new automation starts.
//
// Video is recorded for the default-theme pass only: one take is what a hero
// needs, and a second one would double the committed media for a view the
// stills already cover.
//
// Everything the recording needs is handed in via env by
// scripts/container/web-demo-entry.sh (see playwright.demo.config.ts).

import { expect, type Browser, type Locator, type Page, test } from "@playwright/test";
import { join } from "node:path";
import { readFileSync } from "node:fs";
import { openAfterInitialResync } from "./initial-resync.js";
import { stopPolledRoutes } from "./polled-route.js";
import { assertPhoneKeybar, phoneInputStream } from "./phone-keybar.js";
import { DEMO_VIEWPORT } from "./demo-viewport.js";

const visual = process.env.AF_PERF_MODE === "1";
const visualStyle = visual ? readFileSync(new URL("./visual.css", import.meta.url), "utf8") : "";

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
  if (!visual) await page.waitForTimeout(ms);
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

function screenshotFor(page: Page, suffix: string, seededRows?: number): (name: string) => Promise<void> {
  return async (name: string) => {
    if (visual) {
      const deadline = performance.now() + 30_000;
      const remaining = (limit: number): number => {
        const left = deadline - performance.now();
        if (left <= 0) throw new Error("Capture readiness deadline expired");
        return Math.max(1, Math.min(limit, left));
      };
      const observe = () => page.evaluate(() => ({
        states: [...document.querySelectorAll(".af-rail-list .af-operator-state")].map(el => el.textContent),
        project: document.querySelector(".af-project-item-current .af-project-item-meta")?.textContent ?? null,
        filter: [...document.querySelectorAll(".af-filter-item-count")].map(el => el.textContent),
      }));
      let lastObserved: Awaited<ReturnType<typeof observe>> | null = null;
      try {
        await expect(async () => {
          lastObserved = await observe();
          // A new single-image capture must recheck readiness, including hidden
          // rail/menu summaries. Login/unavailable scenes have no app to settle.
          if (await page.locator(".af-app").count()) {
            const states = page.locator(".af-rail-list .af-operator-state");
            const message = `${name}${suffix}: all seeded sessions must report Needs you before capture`;
            if (seededRows !== undefined) await expect(states, message).toHaveCount(seededRows, { timeout: remaining(1_000) });
            else await expect(states, message).not.toHaveCount(0, { timeout: remaining(1_000) });
            const count = seededRows ?? await states.count();
            await expect(states, message).toHaveText(Array(count).fill("Needs you"), { timeout: remaining(1_000) });
            // projectMeta's settled golden is "4 sessions", without a working
            // suffix or an invented waiting label. Assert it even when hidden.
            await expect(page.locator(".af-project-item-current .af-project-item-meta"), message)
              .toHaveText(`${count} session${count === 1 ? "" : "s"}`, { timeout: remaining(1_000) });
            await expect(page.locator(".af-filter-item-count"), message)
              .toHaveText([String(count), "0", "0", "0", "0"], { timeout: remaining(1_000) });
          }
          // toHaveScreenshot retries internally without rechecking readiness.
          // Capture exactly one image instead; only this outer bounded retry
          // may take another, after repeating every state assertion above.
          // PTY input can update operator state while Chromium takes the image.
          // Validate both sides so update mode cannot bless a transient Working row.
          const ready = await observe();
          expect(ready.states.every(state => state === "Needs you")).toBe(true);
          const pixels = await page.screenshot({
            animations: "disabled", caret: "hide", style: visualStyle,
            timeout: remaining(5_000),
          });
          expect(await observe()).toEqual(ready);
          expect(pixels).toMatchSnapshot(`${name}${suffix}.png`, { maxDiffPixels: 0, threshold: 0.2 });
        }).toPass({ timeout: remaining(30_000) });
      } catch (error) {
        try { lastObserved = await observe(); } catch { /* Keep the last observation if the page closed. */ }
        throw new Error(`${name}${suffix}: capture did not settle within 30s; last observed ${JSON.stringify(lastObserved)}`, { cause: error });
      }
    } else {
      await page.screenshot({ path: join(SHOT_DIR, `${name}${suffix}.png`) });
    }
  };
}

async function record(browser: Browser, pass: Pass): Promise<void> {
  const context = await browser.newContext({
    viewport: DEMO_VIEWPORT,
    // The app ships `auto`, so the theme it renders is the one the viewer's OS
    // asks for. Driving that rather than clicking the appbar toggle means both
    // passes show the DEFAULT setting, which is what a new user will see.
    colorScheme: pass.colorScheme,
    recordVideo: pass.video && !visual ? { dir: VIDEO_DIR, size: DEMO_VIEWPORT } : undefined,
  });
  const page = await context.newPage();
  const video = page.video();
  // Freeze wall-clock age labels only; timers and performance.now still advance.
  await prepareVisual(page);
  const shot = screenshotFor(page, pass.suffix);

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
    await settleTerminal(page);
    await beat(page, 1_200);
    await shot("dashboard");

    // Parallel work: select a second independent worktree and show its transcript.
    await row(page, SESSION_USAGE).click();
    await expect(page.locator(".af-term-host")).toContainText("review it like any branch");
    await settleTerminal(page);
    await shot("parallel-work");
    await row(page, SESSION_JSON).click();

    // --- 2. the new-session modal ------------------------------------------
    await page.locator("button.af-rail-new").click();
    const modal = page.locator(".af-modal-card");
    await expect(modal).toBeVisible();
    await beat(page, 600);
    await modal.locator('input[aria-label="Session title"]').pressSequentially(SESSION_NEW, { delay: visual ? 0 : 55 });
    await beat(page, 400);
    await modal.locator('textarea[aria-label="Initial prompt"]').pressSequentially(NEW_SESSION_PROMPT, {
      delay: visual ? 0 : 14,
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
    await expect(row(page, SESSION_NEW).first()).toBeVisible({ timeout: 90_000 });
    // Selecting it explicitly rather than relying on create's own selection:
    // the recording must be looking at the new session's pane whatever the
    // create path decides to focus.
    const createdRow = row(page, SESSION_NEW).and(page.locator(".af-row:not(.af-row-creating)"));
    await expect(createdRow).toBeVisible({ timeout: 90_000 });
    await createdRow.click();
    await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open", { timeout: 90_000 });
    await expect(page.locator(".af-term-host")).toContainText("demo-agent", { timeout: 90_000 });
    if (pass.createsSession) {
      // Mid-transcript, so the still catches the agent working rather than done.
      await expect(page.locator(".af-term-host")).toContainText("running ./test.sh", { timeout: 90_000 });
    }
    if (visual) await expect(page.locator(".af-term-host")).toContainText("review it like any branch");
    await settleTerminal(page);
    await beat(page, 900);
    await shot("agent-tab");
    await expect(page.locator(".af-term-host")).toContainText("review it like any branch", {
      timeout: 90_000,
    });
    await beat(page, 1_400);

    // --- 4. review: the branch's diff in a process tab -----------------------
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
    await settleTerminal(page);
    await beat(page, 1_200);
    await shot("review");

    // Comparison: normal git review in a process tab.
    await shot("comparison-review");

    // --- 5. the Tasks view -------------------------------------------------
    await page.locator('.af-viewtab[data-view="tasks"]').click();
    await expect(page.locator(".af-tasks")).toBeVisible();
    await expect(page.locator(".af-tasks .af-task-row")).toHaveCount(2);
    await beat(page, 2_000);
    await shot("tasks");

    // Use-case forms are filled but never submitted: recording must not start a
    // real watcher or leave a triage task able to fire during the next pass.
    await page.locator(".af-tasks-add").click();
    const taskForm = page.locator(".af-modal-card");
    await taskForm.getByLabel("Task name", { exact: true }).fill("Daily issue triage");
    await taskForm.getByLabel("Schedule type", { exact: true }).selectOption("custom");
    await taskForm.getByLabel("Cron expression", { exact: true }).fill("0 9 * * 1-5");
    await taskForm.getByLabel("Prompt", { exact: true }).fill("Review new issues, group duplicates, and propose next actions");
    await beat(page, 600);
    await shot("scheduled-triage");
    await taskForm.getByLabel("Task name", { exact: true }).fill("CI failure watcher");
    await taskForm.getByLabel("Trigger type", { exact: true }).selectOption("watch");
    await taskForm.getByLabel("Watch command", { exact: true }).fill("./watch-ci-failures.sh");
    await taskForm.getByLabel("Prompt", { exact: true }).fill("Investigate this CI failure: {{line}}");
    await beat(page, 600);
    await shot("event-intake");
    await taskForm.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(taskForm).toBeHidden();

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
    await stopPolledRoutes(context);
    await context.close();
  }

  if (pass.video && !visual) {
    // saveAs waits for the recording to be flushed, which only happens once the
    // context is closed — hence the ordering.
    await video?.saveAs(join(VIDEO_DIR, "demo-raw.webm"));
  }
}

async function recordTerminalChrome(page: Page, shot: (name: string) => Promise<unknown>, prefix: string): Promise<void> {
  const actions = page.getByRole("button", { name: prefix ? "More app controls" : "Session actions", exact: true });
  await actions.click();
  await expect(page.locator(prefix ? ".af-appbar-tools" : ".af-term-head .af-term-menu")).toBeVisible();
  await shot(`${prefix}session-actions`);
  if (!prefix) await page.locator(".af-tab-new").click();
  await expect(page.locator(".af-tab-menu")).toBeVisible();
  await shot(`${prefix}tab-types`);
  if (!prefix) await page.locator(".af-tab-new").click();
  await actions.click();
  await page.locator(".af-pane-host .xterm").first().click();
  await expect(page.locator(".af-term-keyboard")).toBeVisible();
  if (!prefix) await expect(page.locator(".af-terminal-keybar")).toHaveCount(0);
  await shot(`${prefix}terminal-keyboard`);
  if (prefix === "phone-") {
    const ctrl = page.locator(".af-terminal-keybar:visible").getByRole("button", { name: "Ctrl", exact: true });
    await ctrl.dblclick({ delay: 80 });
    await expect(ctrl).toHaveAttribute("data-state", "locked");
    await shot("phone-terminal-modifier-locked");
    await ctrl.click();
  }
  await page.keyboard.press("Control+]");
  await expect(page.locator(".af-term-keyboard")).toBeHidden();
}

/** Phone geometry and keyboard access supplement the pixel oracle. */
async function assertPhoneHeader(page: Page): Promise<void> {
  await expect(page.locator(".af-app")).toHaveClass(/af-session-first/);
  const header = page.locator(".af-appbar");
  const more = header.getByRole("button", { name: "More app controls", exact: true });
  await expect(header.locator(".af-viewnav")).toBeHidden();
  await expect(page.locator(".af-term-head")).toBeHidden();
  await more.focus();
  await page.keyboard.press("Enter");
  const menu = page.locator(".af-appbar-tools");
  for (const selector of [".af-viewnav", ".af-project-menu", ".af-theme-toggle", ".af-tab-menu", ".af-copy-link-phone"]) {
    await expect(menu.locator(selector)).toBeVisible();
  }
  await expect(menu.getByRole("button", { name: "Disconnect", exact: true })).toBeVisible();
  const controls = menu.locator("button:visible:not(:disabled), a:visible[href]");
  const states = page.locator(".af-rail-list .af-operator-state");
  const seededRows = await states.count();
  const settledSummary = `${seededRows} session${seededRows === 1 ? "" : "s"}`;
  // PTY input in the preceding phone pass briefly marks its session Working.
  // That update rebuilds the inlined project rows, so require the recorder's
  // final seeded projection to remain unchanged before walking the panel.
  let settledSamples = 0;
  await expect.poll(async () => {
    const [operatorStates, summary] = await Promise.all([
      states.allTextContents(),
      menu.locator(".af-project-item-current .af-project-item-meta").textContent(),
    ]);
    const settled = operatorStates.length === seededRows &&
      operatorStates.every((state) => state === "Needs you") && summary?.trim() === settledSummary;
    settledSamples = settled ? settledSamples + 1 : 0;
    return settledSamples;
  }, {
    intervals: [250],
    timeout: 30_000,
    message: "the seeded project summary must stay settled before the phone keyboard audit",
  }).toBeGreaterThanOrEqual(5);

  type ControlRole = Parameters<Locator["getByRole"]>[0];
  const controlOrder = (await controls.evaluateAll((elements) => elements.map((element) => ({
    role: element.getAttribute("role") ?? (element.matches("a[href]") ? "link" : "button"),
    name: element.getAttribute("aria-label")?.trim() ||
      (element as HTMLElement).innerText.replace(/\s+/g, " ").trim(),
  })))) as Array<{ role: ControlRole; name: string }>;
  expect(controlOrder.every(({ name }) => name !== ""), "every phone control has an accessible name").toBe(true);

  let injectedRebuild = false;
  for (const [position, control] of controlOrder.entries()) {
    await page.keyboard.press("Tab");
    await expect(
      menu.getByRole(control.role, { name: control.name, exact: true }),
      `Tab ${position + 1} focuses ${control.role} “${control.name}”`,
    ).toBeFocused();
    if (control.role === "button" && control.name === "+ Add project") {
      // Force #4007 instead of waiting for daemon timing. Match the production
      // renderer by replacing every project-menu child with a fresh node, and
      // insert a row so the live indices shift at the same time.
      const rebuild = await menu.evaluate((panel) => {
        const projectMenu = panel.querySelector<HTMLElement>(".af-project-menu")!;
        const current = projectMenu.querySelector<HTMLElement>(".af-project-item-current")!;
        const inserted = current.cloneNode(true) as HTMLElement;
        inserted.dataset.afFocusRebuildInjection = "";
        inserted.classList.remove("af-project-item-current");
        inserted.setAttribute("aria-selected", "false");
        inserted.querySelector<HTMLElement>(".af-project-item-name")!.textContent = "refreshed-project";
        inserted.querySelector<HTMLElement>(".af-project-item-path")!.textContent = "/work/refreshed-project";
        inserted.querySelector<HTMLElement>(".af-project-item-meta")!.textContent = "0 sessions";
        const focused = document.activeElement as HTMLElement;
        projectMenu.replaceChildren(
          ...Array.from(projectMenu.childNodes).flatMap((node) => {
            const replacement = node.cloneNode(true);
            return node === current ? [inserted, replacement] : [replacement];
          }),
        );
        return {
          priorFocusDetached: !focused.isConnected,
          focusDroppedToBody: document.activeElement === document.body,
        };
      });
      expect(rebuild, "a production-style project refresh replaces the focused control").toEqual({
        priorFocusDetached: true,
        focusDroppedToBody: true,
      });
      // Re-resolve the replacement by the identity snapshotted above. This keeps
      // the red/index and green/name forms on the same rebuild and next Tab.
      await menu.getByRole(control.role, { name: control.name, exact: true }).focus();
      injectedRebuild = true;
    }
  }
  expect(injectedRebuild, "the phone focus audit forces a project-row rebuild").toBe(true);
  await menu.locator("[data-af-focus-rebuild-injection]").evaluateAll((elements) => {
    for (const element of elements) element.remove();
  });
  await page.keyboard.press("Escape");
  await expect(more).toBeFocused();
  await expect(menu).toBeHidden();
  const title = header.locator(".af-term-title");
  await expect(title).toBeVisible();
  await expect(title).toHaveAttribute("aria-label", await title.textContent() ?? "");
  const geometry = await page.evaluate(() => {
    const title = document.querySelector<HTMLElement>(".af-appbar > .af-term-title")!;
    const original = title.textContent;
    title.textContent = "A long focused session title that must truncate with … on a phone";
    const truncates = title.scrollWidth > title.clientWidth && getComputedStyle(title).textOverflow === "ellipsis" && getComputedStyle(title).whiteSpace === "nowrap";
    title.textContent = original;
    const header = document.querySelector(".af-appbar")!.getBoundingClientRect();
    const targets = [...document.querySelectorAll<HTMLElement>(".af-appbar button")].filter(el => el.getBoundingClientRect().height > 0);
    return { truncates, oneRow: header.height <= 48,
      targets: targets.every(el => el.offsetHeight >= 44 && el.offsetWidth >= 44),
      fits: document.documentElement.scrollWidth === innerWidth };
  });
  expect(geometry).toEqual({ truncates: true, oneRow: true, targets: true, fits: true });
  await page.locator(".af-pane-host .xterm").first().click();
  await expect.poll(() => page.locator(".af-pane-host .xterm").first().evaluate(el => el.getBoundingClientRect().height / visualViewport!.height),
    { message: "#3981 terminal must occupy at least 85% of the visual viewport" }).toBeGreaterThanOrEqual(0.85);
  const heights = await page.evaluate(() => ({ width: innerWidth, viewport: visualViewport!.height,
    chrome: document.querySelector(".af-appbar")!.getBoundingClientRect().height,
    keybar: document.querySelector(".af-terminal-keybar")!.getBoundingClientRect().height,
    terminal: document.querySelector(".af-pane-host .xterm")!.getBoundingClientRect().height }));
  console.log("3981 chrome heights", JSON.stringify(heights));
  const host = page.locator(".af-term-host");
  const closed = await host.boundingBox();
  await page.locator(".af-nav-toggle").click();
  const opened = await host.boundingBox();
  console.log("3981 drawer geometry", JSON.stringify({ width: heights.width, closed, opened }));
  expect(opened, "P2 opening the overlay must not resize or displace the pane").toEqual(closed);
  await page.locator(".af-nav-toggle").click();
  expect(await host.boundingBox(), "P2 closing the overlay preserves the pane").toEqual(closed);
  await page.locator(".af-pane-host .xterm").first().click();
}

async function assertPhoneTerminalAlignment(page: Page): Promise<void> {
  const alignment = await page.locator(".af-pane-host").first().evaluate(host => {
    const box = host.getBoundingClientRect();
    const css = getComputedStyle(host);
    const screen = host.querySelector(".xterm-screen")!.getBoundingClientRect();
    const main = host.closest(".af-main")!;
    const border = getComputedStyle(main, "::after");
    return { width: innerWidth, left: screen.left, right: screen.right,
      contentLeft: box.left + parseFloat(css.borderLeftWidth) + parseFloat(css.paddingLeft),
      hostRight: box.right, scrollLeft: host.scrollLeft,
      paintLeft: main.getBoundingClientRect().left + parseFloat(border.borderLeftWidth) };
  });
  console.log("3981 terminal alignment", JSON.stringify(alignment));
  expect(alignment.left, "P1 column one clears the painted focus border").toBeGreaterThanOrEqual(alignment.paintLeft);
  expect(alignment.left, "P1 screen starts inside the pane content box").toBeGreaterThanOrEqual(alignment.contentLeft);
  expect(alignment.right, "P1 screen ends inside the pane").toBeLessThanOrEqual(alignment.hostRight);
  expect(alignment.scrollLeft, "P1 pane host has no horizontal scroll").toBe(0);
}

/** Chrome evidence: disclosures, keyboard ownership and phone layouts are real screens. */
async function recordChrome(browser: Browser, pass: Pick<Pass, "colorScheme" | "suffix">, phone: boolean): Promise<void> {
  const context = await browser.newContext({ viewport: DEMO_VIEWPORT, colorScheme: pass.colorScheme });
  const page = await context.newPage();
  const inputStream = phoneInputStream(page);
  await prepareVisual(page);
  const shot = screenshotFor(page, pass.suffix, 4);
  try {
    await openAfterInitialResync(page, async () => { await page.goto("/"); });
    await row(page, SESSION_JSON).click();
    await settleTerminal(page);
    if (!phone) {
      await recordTerminalChrome(page, shot, "");
      await recordControls(page, shot);
      await page.getByRole("button", { name: "Filter sessions", exact: true }).click();
      await expect(page.locator(".af-filter-menu")).toBeVisible();
      await shot("session-filter");
      await page.getByRole("button", { name: "Filter sessions", exact: true }).click();
      await page.getByRole("button", { name: "Switch project", exact: true }).click();
      await expect(page.locator(".af-project-menu")).toBeVisible();
      await shot("project-menu");
      await page.getByRole("button", { name: "Switch project", exact: true }).click();
      await recordLogin(page, shot);
      return;
    }
    // Issue #3981 evidence: the same real focused session at three phone widths.
    for (const width of [320, 360, 390, 430]) {
      await page.setViewportSize({ width, height: 812 });
      await expect(page.locator(".af-app")).toHaveClass(/af-session-first/);
      await settleTerminal(page);
      await assertPhoneHeader(page);
      await assertPhoneKeybar(page, inputStream);
      await settleTerminal(page);
      await assertPhoneTerminalAlignment(page);
      if (width === 320) await shot("phone-session-320");
      await page.screenshot({ path: visual ? test.info().outputPath(`after-phone-session-${width}${pass.suffix}.png`) : join(SHOT_DIR, `phone-session-${width}${pass.suffix}.png`),
        animations: "disabled", caret: "hide", style: visualStyle });
    }
    await page.setViewportSize({ width: 375, height: 812 });
    await settleTerminal(page);
    await expect(page.locator(".af-nav-toggle")).toBeVisible();
    await shot("phone-session");
    await shot("phone-session-first");
    await recordTerminalChrome(page, shot, "phone-");
    await page.locator(".af-nav-toggle").click();
    await expect(page.locator(".af-rail")).toBeVisible();
    await shot("phone-drawer");
    await page.getByRole("button", { name: "More app controls", exact: true }).click();
    await expect(page.locator(".af-project-menu")).toBeVisible();
    await shot("phone-project-menu");
    await page.getByRole("button", { name: "More app controls", exact: true }).click();
    await page.getByRole("button", { name: "Filter sessions", exact: true }).click();
    await expect(page.locator(".af-filter-menu")).toBeVisible();
    await shot("phone-filter");
    await page.getByRole("button", { name: "Filter sessions", exact: true }).click();
    await page.getByRole("button", { name: "More app controls", exact: true }).click();
    await expect(page.locator(".af-appbar-tools")).toBeVisible();
    await shot("phone-controls");
    await page.getByRole("button", { name: "More app controls", exact: true }).click();
    await page.locator(".af-rail-new").click();
    await page.getByLabel("Session title", { exact: true }).fill("review-followup");
    await shot("phone-create");
    await page.keyboard.press("Escape");
    await page.locator(".af-nav-toggle").click();
    await page.getByRole("button", { name: "More app controls", exact: true }).click();
    await page.locator('.af-viewtab[data-view="tasks"]').click();
    await shot("phone-tasks");
    await page.locator('.af-viewtab[data-view="config"]').click();
    await expect(async () => { await page.locator(".af-accounts").scrollIntoViewIfNeeded(); }).toPass({ timeout: 5000 });
    await shot("phone-config");
    await page.locator(".af-account-disclosure summary").click();
    await page.getByLabel("Account agent", { exact: true }).selectOption("claude");
    await page.getByLabel("New claude account name", { exact: true }).fill("team");
    await page.locator(".af-accounts-register:visible").scrollIntoViewIfNeeded();
    await shot("phone-add-account");
  } finally {
    await stopPolledRoutes(context);
    await context.close();
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

// Phone attaches resize daemon-owned PTYs. Capture them only AFTER both hero
// passes, so narrow scrollback reflow cannot contaminate the dark desktop stills.
test("web chrome · both themes", async ({ browser }) => {
  for (const phone of [false, true]) {
    await recordChrome(browser, { suffix: "", colorScheme: "light" }, phone);
    await recordChrome(browser, { suffix: "-dark", colorScheme: "dark" }, phone);
    if (!phone) await recordSplits(browser);
  }
});

// Split after the full-width stills and before phone attaches resize the PTYs.
async function recordSplits(browser: Browser): Promise<void> {
  for (const pass of [{ suffix: "", colorScheme: "light" }, { suffix: "-dark", colorScheme: "dark" }] as const) {
    const context = await browser.newContext({ viewport: DEMO_VIEWPORT, colorScheme: pass.colorScheme });
    const page = await context.newPage();
    await prepareVisual(page);
    try {
      await openAfterInitialResync(page, async () => { await page.goto("/"); });
      await row(page, SESSION_JSON).click();
      await settleTerminal(page);
      const pane = page.locator(".af-pane").first();
      const box = await pane.boundingBox();
      if (!box) throw new Error("Split evidence requires a visible terminal pane");
      await page.locator('.af-tab[data-tab-index="0"]').dragTo(pane, { targetPosition: { x: 8, y: box.height / 2 } });
      await expect(page.locator(".af-pane")).toHaveCount(2);
      await page.locator(".af-pane-host .xterm").first().click();
      // Splitting resizes both PTYs; wait for that repaint before checking idle.
      await settleTerminal(page);
      await screenshotFor(page, pass.suffix, 4)("split-panes");
    } finally {
      await stopPolledRoutes(context);
      await context.close();
    }
  }
}

/** C: disclosure and confirmation evidence, without committing destructive actions. */
async function recordControls(page: Page, shot: (name: string) => Promise<unknown>): Promise<void> {
  await page.locator(".af-rail-new").click();
  await expect(page.locator(".af-defaults summary")).toContainText("Program:");
  await expect(page.locator(".af-defaults summary")).not.toContainText("Account:");
  await page.getByLabel("Session title", { exact: true }).fill("review-followup");
  await shot("create-compact");
  if (!await page.locator(".af-defaults").evaluate((el) => (el as HTMLDetailsElement).open)) await page.locator(".af-defaults summary").click();
  await shot("create-defaults");
  await page.keyboard.press("Escape");
  await row(page, SESSION_JSON).getByRole("button", { name: `Actions for ${SESSION_JSON}`, exact: true }).click();
  await shot("session-lifecycle");
  await row(page, SESSION_JSON).getByRole("button", { name: `Delete session “${SESSION_JSON}”`, exact: true }).click();
  await shot("kill-confirmation");
  await page.keyboard.press("Escape");
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const task = page.locator(".af-task-row").first();
  await expect.poll(async () => (await task.boundingBox())?.height ?? Infinity).toBeLessThanOrEqual(64);
  await task.locator(".af-term-more").click();
  await shot("task-actions");
  await task.getByRole("button", { name: "Remove", exact: true }).click();
  await shot("remove-task");
  await page.keyboard.press("Escape");
  await task.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(page.getByRole("dialog").getByRole("button", { name: "Save", exact: true })).toBeInViewport({ ratio: 1 });
  const onDone = page.getByRole("combobox", { name: "On done", exact: true });
  await expect(onDone).toBeEnabled();
  await onDone.selectOption("archive");
  await page.getByRole("textbox", { name: "Target session", exact: true }).fill("reused");
  await expect(onDone).toBeHidden();
  await expect(page.getByText("Target session will be reused.", { exact: true })).toBeVisible();
  await page.getByRole("textbox", { name: "Target session", exact: true }).fill("");
  await expect(onDone).toBeVisible();
  await expect(onDone).toHaveValue("archive");
  await onDone.selectOption("keep");
  await onDone.scrollIntoViewIfNeeded();
  await onDone.focus();
  await shot("edit-task");
  await page.keyboard.press("Escape");
  await page.locator('.af-viewtab[data-view="config"]').click();
  const configInput = page.getByLabel("vscode_server_binary", { exact: true });
  const savedValue = await configInput.inputValue();
  await configInput.fill("/usr/local/bin/code-server");
  const save = configInput.locator("..").locator(".af-config-save");
  await configInput.press("Tab");
  await expect(save).toBeFocused();
  await expect(page.locator(".af-config-dirty:visible")).toHaveText("Unsaved");
  await expect(save).toHaveCSS("outline-style", "solid");
  await expect(save).toHaveCSS("outline-width", "2px");
  await expect(save).toHaveCSS("border-width", "1px");
  await shot("config-dirty");
  await configInput.fill(savedValue);
  await expect(save).toBeDisabled();
  await expect(save).toHaveCSS("border-width", "1px");
  await page.locator(".af-account-disclosure summary").click();
  await page.getByLabel("Account agent", { exact: true }).selectOption("claude");
  await page.getByLabel("New claude account name", { exact: true }).fill("team");
  await page.locator(".af-accounts-register:visible").scrollIntoViewIfNeeded();
  await shot("add-account");
  await page.getByLabel("New claude account name", { exact: true }).fill("invalid/name");
  await page.locator(".af-accounts-register:visible").getByRole("button", { name: "Register", exact: true }).click();
  await expect(page.locator(".af-accounts-register:visible [role=alert]")).toBeVisible();
  await expect(page.getByLabel("New claude account name", { exact: true })).toHaveValue("invalid/name");
  await page.locator(".af-accounts-register:visible [role=alert]").scrollIntoViewIfNeeded();
  await shot("account-error");
  await page.route("**/v1/config-assistant", (route) => route.fulfill({status: 503, contentType: "application/json", body: JSON.stringify({data: null, error: {message: "Assistant unavailable. Check the configured agent and try again."}})}));
  await page.getByRole("button", { name: "Configure with assistant", exact: true }).click();
  await expect(page.locator(".af-assistant-error")).toBeVisible();
  await shot("assistant-error");
  await page.keyboard.press("Escape");
  await page.unroute("**/v1/config-assistant");
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await row(page, SESSION_JSON).click();
  await settleTerminal(page);
  await page.getByRole("button", { name: "Switch project", exact: true }).click();
  await page.locator(".af-project-add").click();
  await expect(page.locator(".af-dirpicker")).toBeVisible();
  await shot("add-project");
  await page.keyboard.press("Escape");
}

async function recordLogin(page: Page, shot: (name: string) => Promise<unknown>): Promise<void> {
  await page.route("**/v1/auth-info", (route) => route.fulfill({status: 200, contentType: "application/json", body: JSON.stringify({data: {auth_required: true}, error: null})}));
  await page.goto("/");
  await expect(page.locator("#af-token")).toBeVisible();
  await shot("login");
  await page.unroute("**/v1/auth-info");
  await page.route("**/v1/auth-info", (route) => route.fulfill({status: 200, contentType: "application/json", body: JSON.stringify({data: {auth_required: false}, error: null})}));
  await page.route("**/v1/Snapshot", (route) => route.abort("connectionrefused"));
  await page.reload();
  await expect(page.getByRole("heading", { name: "Cannot reach the daemon", exact: true })).toBeVisible();
  await shot("unavailable");
}

async function prepareVisual(page: Page): Promise<void> {
  if (!visual) return;
  await page.clock.setFixedTime(new Date("2000-01-01T00:00:00Z"));
  // Schedule dates are daemon-derived. Normalize only this recorder projection,
  // so the next calendar day cannot invalidate an otherwise identical screenshot.
  await page.route("**/v1/ListTasks", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    for (const task of body.data?.tasks ?? []) {
      if (task.next_run_at) task.next_run_at = "2000-01-03T14:00:00Z";
      // The demo seeds this task at the next hour to avoid a run while recording.
      if (task.name === "nightly-tests") task.cron_expr = "0 14 * * *";
    }
    await route.fulfill({ response, json: body });
  });
}

// Behavioral coverage shares the daemon/container fence with the modal goldens.
test("task completion hints follow the selected consequence", async ({ page }) => {
  await openAfterInitialResync(page, async () => { await page.goto("/"); });
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  await page.locator(".af-tasks-add").click();
  const dialog = page.getByRole("dialog");
  const choice = dialog.getByRole("combobox", { name: "On done", exact: true });
  await expect(choice).toBeEnabled();
  for (const [value, hint] of [
    ["keep", "leaves the run's session in place"],
    ["archive", "archives the run's session — restorable"],
    ["kill", "deletes the run's session and its branch — permanent"],
  ]) {
    await choice.selectOption(value);
    await expect(dialog.locator(".af-on-complete-hint")).toHaveText(hint, { timeout: 5_000 });
    await expect(dialog.locator(".af-on-complete-hint")).toBeVisible();
  }
  await dialog.getByRole("textbox", { name: "Target session", exact: true }).fill("reused");
  await expect(choice).toBeHidden();
  await expect(dialog.locator(".af-on-complete-hint")).toBeHidden();
});

test("task completion catalog failure is visible and preserves the seed", async ({ page }) => {
  await page.route("**/v1/ListOnComplete", route => route.abort("failed"));
  await page.route("**/v1/ListTasks", async route => {
    const response = await route.fetch();
    const body = await response.json();
    body.data.tasks[0].on_complete = "archive";
    await route.fulfill({ response, json: body });
  });
  await openAfterInitialResync(page, async () => { await page.goto("/"); });
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  await page.locator(".af-task-row").first().getByRole("button", { name: "Edit", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.locator(".af-on-complete-hint")).toHaveText("Choices unavailable · current value kept.", { timeout: 5_000 });
  await expect(dialog.locator(".af-on-complete-hint")).toBeVisible();
  await expect(dialog.getByRole("combobox", { name: "On done", exact: true })).toHaveValue("archive");
  await expect(dialog.getByRole("combobox", { name: "On done", exact: true })).toBeDisabled();
  let saved: string | undefined;
  await page.route("**/v1/UpdateTask", async route => {
    saved = route.request().postDataJSON().update.on_complete;
    await route.fulfill({ json: { data: { task: {} }, error: null } });
  });
  await dialog.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => saved).toBe("archive");
  // Saving refreshes tasks; finish intercepted refreshes before closing the page.
  await page.unrouteAll({ behavior: "wait" });
});
