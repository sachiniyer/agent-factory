import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "@playwright/test";

// Proposal mode, step 2: `af proposal start` gives the session being viewed a Docs
// tab, the web splits it to the right of the Agent pane with the lazy editor bundle,
// and an edit in the browser saves through the daemon to the doc on disk.
test("docs tab: af proposal start opens a Docs pane whose edits save to disk", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const title = "docs-probe";
  const slug = "docs-probe-plan";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");

  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();

  const editorLoads: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/lazy/docs-editor.js")) editorLoads.push(r.url());
  });
  await page.goto(`/#/session/${session.id}`);
  await expect(page.locator(".af-tab-label", { hasText: "Agent" })).toBeVisible();
  await expect(page.locator(".af-pane")).toHaveCount(1);
  expect(editorLoads, "the editor bundle must not load before a Docs tab opens").toHaveLength(0);

  const started = JSON.parse(af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title,
    "--docs", "goals,api")) as { path: string; tab?: string; tab_error?: string };
  expect(started.tab_error ?? "", "start must create the Docs tab").toBe("");
  expect(started.tab).toBeTruthy();

  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();
  const panes = page.locator(".af-pane");
  await expect(panes, "Agent and Docs open side by side").toHaveCount(2);
  await expect(panes.nth(0).locator(".xterm")).toBeVisible();
  await expect(panes.nth(1).locator(".af-docs")).toBeVisible();
  const agentBox = await panes.nth(0).boundingBox();
  const docsBox = await panes.nth(1).boundingBox();
  expect(docsBox!.x, "Docs sits to the right of Agent").toBeGreaterThan(agentBox!.x);
  await expect(pane.locator(".af-docs-tab")).toHaveText(["goals", "api"]);
  await expect(pane.locator('.af-docs-tab[aria-selected="true"]')).toHaveText("goals");
  await expect(pane.locator(".af-docs-status"), "a saved doc shows no status").toBeHidden();
  expect(editorLoads).toHaveLength(1);

  const marker = `Edited from the browser ${Date.now()}`;
  await pane.locator(".cm-content").click();
  await page.keyboard.press("Control+End");
  await page.keyboard.type(`\n\n${marker}\n`);
  await expect(pane.locator(".af-docs-status")).toBeHidden({ timeout: 15_000 });
  await expect.poll(() => readFileSync(join(started.path, "goals.md"), "utf8")).toContain(marker);

  await page.locator(".af-tab-label", { hasText: /^Agent$/ }).click();
  await expect(panes, "a tab click leaves the proposal view for that tab alone").toHaveCount(1);
  await expect(panes.nth(0).locator(".xterm")).toBeVisible();
  await page.locator(".af-tab-label", { hasText: /^Proposal$/ }).click();
  await expect(panes, "the Proposal tab brings back Agent and the proposal").toHaveCount(2);
  await expect(panes.nth(0).locator(".xterm")).toBeVisible();
  await expect(panes.nth(1).locator(".af-docs")).toBeVisible();
});

// Proposal mode, step 3: the tab has no Auto / Manual switch, but a session set to
// manual from the CLI holds edits until Send now delivers them to the agent.
test("docs tab: Manual mode holds edits until Send now delivers them to the agent", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const title = "docs-push-probe";
  const slug = "docs-push-plan";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  const started = JSON.parse(af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title,
    "--docs", "goals")) as { tab?: string; tab_error?: string };
  expect(started.tab_error ?? "", "start must create the Docs tab").toBe("");
  af("proposal", "push-mode", "manual", "--repo", mockRepo as string, "--session", title);

  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();

  await page.goto(`/#/session/${session.id}`);
  await expect(page.locator(".af-pane"), "a session opened with a Docs tab shows it split").toHaveCount(2);
  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();
  await expect(pane.locator(".af-docs-status")).toBeHidden();

  const push = pane.locator(".af-docs-push");
  await expect(push.getByRole("button", { name: "Auto", exact: true })).toHaveCount(0);
  await expect(push.getByRole("button", { name: "Manual", exact: true })).toHaveCount(0);
  const sendNow = push.locator(".af-docs-chip-action");
  await expect(sendNow).toHaveText("Send now");
  await expect(sendNow).toBeDisabled();

  const marker = `Pushed from the browser ${Date.now()}`;
  await pane.locator(".cm-content").click();
  await page.keyboard.press("Control+End");
  await page.keyboard.type(`\n\n${marker}\n`);
  await expect(pane.locator(".af-docs-status")).toBeHidden({ timeout: 15_000 });
  await expect(sendNow, "a manual-mode save must not send by itself").toBeEnabled({ timeout: 15_000 });

  await sendNow.click();
  await expect.poll(() => af("sessions", "preview", title, "--repo", mockRepo as string, "--full", "--plain"),
    { timeout: 30_000 }).toContain(`Proposal ${slug}: the user edited goals.md.`);
  await expect(sendNow).toBeDisabled({ timeout: 15_000 });
});

// Right-click on a doc tab deletes the doc at once; the file leaves the
// proposal and the strip moves to the next doc.
test("docs tab: right-click Delete removes a doc", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const title = "docs-remove-probe";
  const slug = "docs-remove-plan";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  const started = JSON.parse(af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title,
    "--docs", "goals,api")) as { path: string; tab_error?: string };
  expect(started.tab_error ?? "", "start must create the Docs tab").toBe("");

  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();
  await page.goto(`/#/session/${session.id}`);
  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();
  await expect(pane.locator(".af-docs-tab")).toHaveText(["goals", "api"]);

  const goals = pane.locator(".af-docs-tab", { hasText: "goals" });
  await goals.click({ button: "right" });
  const menu = pane.locator(".af-docs-docmenu");
  await menu.getByRole("menuitem", { name: "Delete" }).click();
  await expect(menu).toBeHidden();
  await expect(pane.locator(".af-docs-tab")).toHaveText(["api"]);
  await expect(pane.locator('.af-docs-tab[aria-selected="true"]')).toHaveText("api");
  await expect.poll(() => existsSync(join(started.path, "goals.md"))).toBe(false);
  expect(existsSync(join(started.path, "api.md"))).toBe(true);
});

// A doc whose delete is still out cannot be reopened: a sync started now would have its
// stored draft cleared by the delete's reply, and closing it would lose what was typed.
test("docs tab: a doc whose delete is pending does not open from its tab", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const title = "docs-pending-delete-probe";
  const slug = "docs-pending-delete-plan";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  const started = JSON.parse(af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title,
    "--docs", "goals,api")) as { path: string; tab_error?: string };
  expect(started.tab_error ?? "", "start must create the Docs tab").toBe("");

  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();

  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/v1/RemoveProposalDoc", async (route) => {
    await held;
    await route.continue();
  });

  await page.goto(`/#/session/${session.id}`);
  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();
  const goalsTab = pane.locator(".af-docs-tab", { hasText: "goals" });
  const apiTab = pane.locator(".af-docs-tab", { hasText: "api" });
  await expect(goalsTab).toHaveAttribute("aria-selected", "true");

  await apiTab.click({ button: "right" });
  await pane.locator(".af-docs-docmenu").getByRole("menuitem", { name: "Delete" }).click();
  // The delete request is held: the tab is still there, and clicking it opens nothing.
  await expect(apiTab).toBeVisible();
  await apiTab.click();
  await expect(goalsTab, "the doc being deleted must not open").toHaveAttribute("aria-selected", "true");
  await expect(apiTab).not.toHaveAttribute("aria-selected", "true");

  release();
  await expect(pane.locator(".af-docs-tab")).toHaveText(["goals"]);
  await expect.poll(() => existsSync(join(started.path, "api.md"))).toBe(false);
});

// Switching docs mid-edit keeps the keystrokes: the unsaved text is saved on the
// switch and the doc comes back with it, not with the daemon's older text.
test("docs tab: switching docs mid-edit keeps what was typed", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const title = "docs-switch-probe";
  const slug = "docs-switch-plan";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  const started = JSON.parse(af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title,
    "--docs", "goals,api")) as { path: string; tab_error?: string };
  expect(started.tab_error ?? "").toBe("");
  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  await page.goto(`/#/session/${session.id}`);
  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();

  const marker = `typed just before switching ${Date.now()}`;
  await pane.locator(".cm-content").click();
  await page.keyboard.press("Control+End");
  await page.keyboard.type(`\n\n${marker}\n`);
  await pane.locator(".af-docs-tab", { hasText: "api" }).click();
  await expect(pane.locator('.af-docs-tab[aria-selected="true"]')).toHaveText("api");
  await expect(pane.locator(".cm-content")).not.toContainText(marker);
  await pane.locator(".af-docs-tab", { hasText: "goals" }).click();
  await expect(pane.locator(".cm-content")).toContainText(marker);
  await expect.poll(() => readFileSync(join(started.path, "goals.md"), "utf8")).toContain(marker);
});

// A Proposal tab that arrives while the user is typing in the Agent terminal adds the split, which
// rebuilds the pane wrappers and reparents the Agent container: the keyboard must stay in the terminal
// for a user who had it, and must not be handed to one who had left it.
for (const attached of [true, false]) {
  test(`docs tab: a proposal arriving ${attached ? "keeps the keyboard in the Agent terminal" : "does not take a detached user into it"}`, async ({ page, request }) => {
    const afBin = process.env.AF_BIN;
    const mockRepo = process.env.AF_MOCK_REPO;
    test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
    const af = (...args: string[]): string =>
      execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

    const title = attached ? "docs-focus-kept-probe" : "docs-focus-detached-probe";
    const slug = attached ? "docs-focus-kept-plan" : "docs-focus-detached-plan";
    af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
    const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
    const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
    expect(session).toBeTruthy();
    await page.goto(`/#/session/${session.id}`);
    await expect(page.locator(".af-tab-label", { hasText: /^Agent$/ })).toBeVisible();
    const textarea = page.locator(".af-pane-host .xterm-helper-textarea").first();
    await page.locator(".af-pane-host .xterm").first().click();
    await expect(textarea).toBeFocused();
    if (!attached) {
      await textarea.evaluate((el) => (el as HTMLElement).blur());
      await expect(textarea).not.toBeFocused();
    }

    af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title, "--docs", "goals");
    await expect(page.locator(`.af-docs[data-proposal="${slug}"] .cm-content`)).toBeVisible({ timeout: 20_000 });
    if (attached) {
      await expect(page.locator(".af-pane-host .xterm-helper-textarea").first(), "the Agent terminal keeps the keyboard").toBeFocused();
    } else {
      await expect(page.locator(".af-pane-host .xterm-helper-textarea").first(), "a detached user is not pulled into it").not.toBeFocused();
    }
  });
}

// On a phone the session header carries one shortcut per Proposal tab; with several of them the strip
// must not push the app menu out of the viewport, because the tab bar lives behind that menu.
test("docs tab: several Proposal shortcuts keep the app menu on screen at phone width", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const title = "docs-phone-strip-probe";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  af("proposal", "start", "phone-strip-plan", "--repo", mockRepo as string, "--session", title, "--docs", "goals");
  for (const n of [2, 3, 4]) {
    af("sessions", "tabs", "create", title, "--repo", mockRepo as string, "--kind", "docs", "--proposal", "phone-strip-plan", "--name", `phone-${n}`);
  }
  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();

  await page.setViewportSize({ width: 320, height: 700 });
  await page.goto(`/#/session/${session.id}`);
  const shortcuts = page.locator(".af-session-first .af-term-docs");
  await expect(shortcuts).toHaveCount(4, { timeout: 20_000 });
  const more = page.locator(".af-appbar-more");
  await expect(more).toBeVisible();
  const box = (await more.boundingBox())!;
  expect(box.x, "the app menu starts inside the viewport").toBeGreaterThanOrEqual(0);
  expect(box.x + box.width, "and ends inside it").toBeLessThanOrEqual(320);
  const noOverflow = await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth);
  expect(noOverflow, "the page does not scroll sideways").toBe(true);
  // Every shortcut stays reachable: the strip scrolls, it does not clip them.
  await shortcuts.last().scrollIntoViewIfNeeded();
  await expect(shortcuts.last()).toBeVisible();
});

// iOS Safari fires no `contextmenu` on a touch long-press, so the doc-strip tab
// opens its Delete menu from the pointerdown's own 500 ms timer; on lift WebKit
// synthesizes a compatibility `mousedown` + `click` targeting the pressed tab
// (implicit pointer capture), which — without the one-shot suppression in
// `web/src/docs_pane.ts` — close the just-opened menu (the document-capture
// mousedown closer) and open the doc (the tab's unconditional click handler),
// so on a touch Apple device Delete is unreachable.
//
// This is the touch path the bug report traces on iOS Safari. Because
// Playwright's Chromium long-press consumes the gesture with `contextmenu` +
// `pointercancel` (the Android Chrome path), this test starts the production
// timer with a JS-dispatched `pointerdown` (so it does not go through
// Chromium's gesture recognizer and fires no `contextmenu`), then synthesizes
// the lift's compat mouse events + `click` itself — exactly the recorded WebKit
// sequence at the finger lift. It asserts the menu stays open and the
// long-pressed doc does NOT switch, then taps Delete and checks the doc leaves.
test("docs tab: a touch long-press keeps the Delete menu open through the lift's synthesized click", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const title = "docs-touch-long-press-probe";
  const slug = "docs-touch-long-press-plan";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  const started = JSON.parse(af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title,
    "--docs", "goals,api")) as { path: string; tab_error?: string };
  expect(started.tab_error ?? "", "start must create the Docs tab").toBe("");

  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();
  await page.goto(`/#/session/${session.id}`);
  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();
  await expect(pane.locator(".af-docs-tab")).toHaveText(["goals", "api"]);
  const goalsTab = pane.locator(".af-docs-tab", { hasText: "goals" });
  const apiTab = pane.locator(".af-docs-tab", { hasText: "api" });
  await expect(goalsTab).toHaveAttribute("aria-selected", "true");
  const menu = pane.locator(".af-docs-docmenu");
  await expect(menu).toBeHidden();

  // (1) Touch `pointerdown` on the api tab — the doc NOT already open — starts
  // the production 500 ms timer (TAB_PRESS_LIMITS.holdMs in web/src/tabtouch.ts).
  // JS-dispatched so Chromium's long-press gesture recognizer never sees it,
  // mirroring iOS Safari, which fires no `contextmenu` to mask the bug.
  const point = await apiTab.evaluate((el: HTMLElement) => {
    const r = el.getBoundingClientRect();
    return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
  });
  await apiTab.evaluate((el: HTMLElement, p) => {
    el.dispatchEvent(new PointerEvent("pointerdown", {
      bubbles: true, cancelable: true,
      pointerId: 7, pointerType: "touch", isPrimary: true,
      clientX: p.x, clientY: p.y,
    }));
  }, point);

  // (2) The production timer fires and opens the doc menu WITHOUT switching
  // the doc (`openDocMenu` does not call `openDoc`).
  await expect(menu, "the long-press timer must open the doc menu").toBeVisible({ timeout: 2_000 });
  await expect(menu.getByRole("menuitem", { name: "Delete" })).toBeVisible();
  await expect(apiTab, "long-pressing must not open the doc").not.toHaveAttribute("aria-selected", "true");
  await expect(goalsTab).toHaveAttribute("aria-selected", "true");

  // (3) The lift. iOS Safari synthesizes compat `mousedown`/`mouseup`/`click`
  // targeting the pressed tab; without the one-shot suppression the
  // document-capture mousedown closer (`onDocMouseDown`) closes the menu and
  // the tab's `click` handler runs `openDoc(api)`.
  await apiTab.evaluate((el: HTMLElement, p) => {
    const base = { bubbles: true, cancelable: true, clientX: p.x, clientY: p.y, button: 0 };
    el.dispatchEvent(new PointerEvent("pointerup", {
      ...base, pointerId: 7, pointerType: "touch", isPrimary: true,
    }));
    el.dispatchEvent(new MouseEvent("mousedown", base));
    el.dispatchEvent(new MouseEvent("mouseup", base));
    el.dispatchEvent(new MouseEvent("click", base));
  }, point);

  // (4) THE bug assertion: the menu stays open through both synthesized events
  // and the long-pressed doc does not switch/open.
  await expect(menu, "the synthesized mousedown must not close the doc menu").toBeVisible();
  await expect(goalsTab, "the synthesized click must not open the long-pressed doc").toHaveAttribute("aria-selected", "true");
  await expect(apiTab).not.toHaveAttribute("aria-selected", "true");

  // (5) With the menu still open, Delete is reachable: a real click on the
  // menuitem removes the doc and the file leaves the proposal.
  await menu.getByRole("menuitem", { name: "Delete" }).click();
  await expect(menu).toBeHidden();
  await expect(pane.locator(".af-docs-tab")).toHaveText(["goals"]);
  await expect(pane.locator('.af-docs-tab[aria-selected="true"]')).toHaveText("goals");
  await expect.poll(() => existsSync(join(started.path, "api.md"))).toBe(false);
  expect(existsSync(join(started.path, "goals.md"))).toBe(true);
});

// Regression guard: after the touch long-press fix, a real click on a tab (no
// long-press in the picture) still opens the doc — the one-shot flag must not
// linger and swallow the next ordinary click.
test("docs tab: an ordinary click still opens a doc after the long-press fix", async ({ page, request }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const af = (...args: string[]): string =>
    execFileSync(afBin as string, args, { stdio: ["ignore", "pipe", "pipe"], encoding: "utf8" });

  const title = "docs-touch-after-long-press-probe";
  const slug = "docs-touch-after-long-press-plan";
  af("sessions", "create", "--repo", mockRepo as string, "--name", title, "--program", "claude");
  af("proposal", "start", slug, "--repo", mockRepo as string, "--session", title, "--docs", "goals,api");

  const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const session = envelope.data.instances.find((s: { title: string }) => s.title === title);
  expect(session).toBeTruthy();
  await page.goto(`/#/session/${session.id}`);
  const pane = page.locator(`.af-docs[data-proposal="${slug}"]`);
  await expect(pane.locator(".cm-content")).toBeVisible();
  await expect(pane.locator(".af-docs-tab")).toHaveText(["goals", "api"]);
  const goalsTab = pane.locator(".af-docs-tab", { hasText: "goals" });
  const apiTab = pane.locator(".af-docs-tab", { hasText: "api" });
  await expect(goalsTab).toHaveAttribute("aria-selected", "true");
  const menu = pane.locator(".af-docs-docmenu");

  // Open the menu via the touch long-press timer, then Escape to close it: the
  // one-shot flag would be armed and consumed by the next click only if it
  // lingered past the menu's dismissal.
  const point = await apiTab.evaluate((el: HTMLElement) => {
    const r = el.getBoundingClientRect();
    return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
  });
  await apiTab.evaluate((el: HTMLElement, p) => {
    el.dispatchEvent(new PointerEvent("pointerdown", {
      bubbles: true, cancelable: true,
      pointerId: 7, pointerType: "touch", isPrimary: true,
      clientX: p.x, clientY: p.y,
    }));
  }, point);
  await expect(menu).toBeVisible({ timeout: 2_000 });
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  // The one-shot must be cleared (closeDocMenu clears it), so a real click now
  // opens the doc — no suppressed-by-stale-flag regression.
  await apiTab.click();
  await expect(apiTab, "an ordinary click after a long-press menu dismissal must open the doc").toHaveAttribute("aria-selected", "true");
  await expect(goalsTab).not.toHaveAttribute("aria-selected", "true");
});
