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
