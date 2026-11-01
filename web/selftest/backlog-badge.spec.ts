// The Backlog tab badge counts the selected project's open questions, and the
// project switcher shows each other project's count next to its name (Refs #6284,
// fork 6622942825). Three projects are served at the transport edge, so the counts
// do not depend on the daemon's state: one question, two questions, and none. The
// two-question project has a name long enough to fill the switcher menu, so the
// badge must stay visible beside a name the menu cuts short.
import { test, expect, type Page } from "@playwright/test";

const envelope = (data: unknown) => ({ data, error: null });
const AT = "2000-01-01T00:00:00Z";
const ONE = "/tmp/badge-one";
const TWO = "/tmp/badge-two-with-a-project-name-long-enough-to-fill-the-switcher-menu";
const NONE = "/tmp/badge-none";

const question = (id: number) => ({
  id, title: `Item ${id}`, status: "queued", created_at: AT, updated_at: AT,
  thread: [{ id: 1, kind: "question", author: "agent:s1", text: "Which one?", at: AT }],
});
const plain = (id: number) => ({ id, title: `Item ${id}`, status: "draft", created_at: AT, updated_at: AT });

function view(items: Array<Record<string, unknown>>) {
  const states: Record<number, unknown> = {};
  for (const it of items) states[it.id as number] = it.thread ? { state: "needs_you", reason: "Question from the agent" } : { state: "draft" };
  return {
    backlog: { schema_version: 1, next_id: items.length + 1, settings: { runner: false, max_running: 1, on_done: "keep" }, items },
    states,
    runner: { state: "off", reason: "The runner is off.", used: 0, max: 1 },
  };
}

const VIEWS: Record<string, unknown> = {
  [ONE]: view([question(1), plain(2)]),
  [TWO]: view([question(1), question(2), plain(3)]),
  [NONE]: view([plain(1)]),
};

async function serve(page: Page): Promise<void> {
  await page.routeWebSocket("**/v1/events*", () => {});
  await page.route("**/v1/Snapshot", route => route.fulfill({ json: envelope({ instances: [] }) }));
  await page.route("**/v1/ListTasks", route => route.fulfill({ json: envelope({ tasks: [] }) }));
  await page.route("**/v1/ListProjects", route => route.fulfill({ json: envelope({
    projects: [ONE, TWO, NONE].map((root, i) => ({ id: `prj_${i}`, checkout_id: `c${i}`, root, relative_root: "", path_exists: true })),
  }) }));
  await page.route("**/v1/GetBacklog", route => {
    const root = (route.request().postDataJSON() as { repo_path: string }).repo_path;
    return route.fulfill({ json: envelope(VIEWS[root]) });
  });
  await page.route("**/v1/GetBudget", route => route.fulfill({ json: envelope({
    budget: { schema_version: 1, share: 30, stop_at: 80, max_per_run: 10, usd_limit: 0, usd_spent: 0 },
  }) }));
}

const tabBadge = (page: Page) => page.locator('.af-viewtab[data-view="backlog"] .af-viewtab-badge');
const option = (page: Page, root: string) => page.locator(`.af-project-menu [data-project-focus="project:${root}"]`);

async function openMenu(page: Page): Promise<void> {
  await page.locator(".af-project-switch").click();
  await expect(page.locator(".af-project-menu")).toBeVisible();
}

// Whether a row's name is cut short, read in one page task. The events socket's open
// triggers a resync, and a resync that replaces the session list rebuilds the
// switcher's rows, at whatever point of the test it lands. A locator's evaluate
// resolves the element and measures it in two round trips, so a rebuild between them
// measured a detached row as 0 by 0 (#4217, run 41317193186). Querying and measuring
// in the same task always reads the row on the page.
async function nameFit(page: Page, root: string): Promise<string> {
  return page.evaluate(selector => {
    const el = document.querySelector(selector);
    if (!el) return "no row";
    return el.scrollWidth > el.clientWidth ? "cut short" : `fits: scrollWidth ${el.scrollWidth}, clientWidth ${el.clientWidth}`;
  }, `.af-project-menu [data-project-focus="project:${root}"] .af-project-item-name`);
}

async function switchTo(page: Page, root: string): Promise<void> {
  await openMenu(page);
  await option(page, root).click();
  await expect(page.locator(".af-project-switch-name")).toHaveText(root.split("/").pop()!);
}

test("the Backlog badge counts the selected project and the switcher shows the others", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await serve(page);
  await page.goto(`/#/project/${encodeURIComponent(ONE)}`);
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-project-switch-name")).toHaveText("badge-one");

  // One question here, two elsewhere: the tab says 1, not 3.
  await expect(tabBadge(page)).toHaveText("1");
  await expect(tabBadge(page)).toHaveAttribute("aria-label", "1 needs you");

  await openMenu(page);
  await expect(option(page, ONE).locator(".af-project-item-badge"), "the current project's count is on the tab, not its row").toHaveCount(0);
  const twoBadge = option(page, TWO).locator(".af-project-item-badge");
  await expect(twoBadge).toHaveText("2");
  await expect(twoBadge).toHaveAttribute("aria-label", "2 need you");
  await expect(option(page, NONE).locator(".af-project-item-badge")).toHaveCount(0);

  // The long name is cut short, and the badge beside it is still drawn whole: the
  // intersection check counts what an overflow-clipping ancestor hides. The name's
  // text is the project's name alone, so lookups by name still match.
  // The poll waits out a rebuild. A name that still fits fails with its measurements:
  // the fixture no longer fills the menu, so the badge check below proves nothing.
  await expect.poll(() => nameFit(page, TWO), { message: "the name is long enough to be cut short" }).toBe("cut short");
  const name = option(page, TWO).locator(".af-project-item-name");
  await expect(name).toHaveText(TWO.split("/").pop()!);
  await expect(twoBadge).toBeInViewport({ ratio: 1 });
  await page.keyboard.press("Escape");

  // Switching follows: the tab shows the new project's count and the old one moves to the menu.
  await switchTo(page, TWO);
  await expect(tabBadge(page)).toHaveText("2");
  await openMenu(page);
  await expect(option(page, ONE).locator(".af-project-item-badge")).toHaveText("1");
  await expect(option(page, TWO).locator(".af-project-item-badge")).toHaveCount(0);
  await page.keyboard.press("Escape");

  // A project with no questions hides the tab badge.
  await switchTo(page, NONE);
  await expect(tabBadge(page)).toBeHidden();
});
