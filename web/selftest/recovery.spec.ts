// P4: real application scenes with API failures intercepted at the transport edge.
// Runs only in the sanctioned web-selftest container, like web-driver.spec.ts.
import { writeFile } from "node:fs/promises";
import { test, expect, type Page, type TestInfo } from "@playwright/test";

const envelope = (data: unknown) => ({ data, error: null });
const refusal = { status: 503, json: { data: null, error: { message: "The daemon refused this operation. Your input is retained." } } };
async function still(page: Page, info: TestInfo, name: string): Promise<void> {
  const path = info.outputPath(`${name}.png`);
  await page.screenshot({ path, fullPage: true, animations: "disabled" });
  await info.attach(name, { path, contentType: "image/png" });
  const theme = info.titlePath.join(" ").includes("recovery dark") ? "dark" : "light";
  const golden = `${name.toLowerCase().replaceAll(" ", "-")}-${theme}.png`;
  const recovery = page.locator(".af-recovery:visible").last();
  const pixels = await recovery.screenshot({ animations: "disabled" });
  await writeFile(info.outputPath(`golden-${golden}`), pixels);
  expect(pixels).toMatchSnapshot(golden);

}
async function centeredColumn(page: Page, field?: string): Promise<void> {
  const root = page.locator(".af-recovery-login");
  const heading = root.locator("h1");
  const title = await heading.boundingBox();
  expect(title).not.toBeNull();
  const viewport = page.viewportSize()!;
  expect(Math.abs(title!.x + title!.width / 2 - viewport.width / 2)).toBeLessThan(2);
  const extent = await root.evaluate(element => {
    const children = [...element.children].map(child => child.getBoundingClientRect());
    return { top: Math.min(...children.map(rect => rect.top)), bottom: Math.max(...children.map(rect => rect.bottom)) };
  });
  expect(Math.abs((extent.top + extent.bottom) / 2 - viewport.height / 2)).toBeLessThan(2);
  if (field) {
    const target = await page.locator(field).boundingBox();
    expect(target).not.toBeNull();
    expect(title!.y + title!.height).toBeLessThanOrEqual(target!.y);
  }
}
async function emptyData(page: Page, project = true): Promise<void> {
  await page.routeWebSocket("**/v1/events*", () => {});
  await page.route("**/v1/Snapshot", route => route.fulfill({ json: envelope({ instances: [] }) }));
  await page.route("**/v1/ListTasks", route => route.fulfill({ json: envelope({ tasks: [] }) }));
  await page.route("**/v1/ListProjects", route => route.fulfill({ json: envelope({ projects: project ? [{ root: "/tmp/recovery", name: "Recovery" }] : [] }) }));
}
for (const theme of ["light", "dark"] as const) {
  test.describe(`P4 recovery ${theme}`, () => {
    test.setTimeout(25_000);
    test.beforeEach(async ({ page }) => {
      await page.emulateMedia({ colorScheme: theme });
    });
    for (const scene of ["No project registered", "No sessions", "No tasks"] as const) {
      test(scene, async ({ page }, info) => {
        await emptyData(page, scene !== "No project registered");
        await page.goto("/");
        await expect(page.locator(".af-app")).toBeVisible();
        if (scene === "No tasks") await page.getByRole("tab", { name: "Tasks", exact: true }).click();
        const screen = page.locator(".af-recovery").filter({ has: page.getByRole("heading", { name: scene, exact: true }) });
        await expect(screen).toBeVisible();
        await expect(screen.getByRole("button")).toHaveCount(1);
        await expect(page.locator(".af-rail-empty, .af-rail-empty-project")).toHaveCount(0);
        await still(page, info, scene);
        await screen.getByRole("button").click();
        await expect(page.locator(".af-modal-card")).toBeVisible();
      });
    }
    test("Cannot reach the daemon", async ({ page }, info) => {
      await page.route("**/v1/auth-info", route => route.abort("connectionrefused"));
      await page.goto("/");
      await expect(page.getByRole("heading", { name: "Cannot reach the daemon" })).toBeVisible();
      await expect(page.getByRole("button")).toHaveCount(1);
      await centeredColumn(page, ".af-recovery-action");
      await still(page, info, "no-daemon");
      await page.unroute("**/v1/auth-info");
      await page.getByRole("button", { name: "Retry", exact: true }).click();
      await expect(page.locator(".af-app")).toBeVisible();
    });
    test("Login expired", async ({ page }, info) => {
      await page.addInitScript(() => localStorage.setItem("af.token", "expired"));
      await page.route("**/v1/auth-info", route => route.fulfill({ json: envelope({ auth_required: true }) }));
      await page.route("**/v1/Snapshot", route => route.fulfill({ status: 401, json: { data: null, error: { message: "unauthorized" } } }));
      await page.goto("/");
      await expect(page.getByRole("heading", { name: "Login expired" })).toBeVisible();
      await expect(page.locator("#af-token")).toBeVisible();
      await centeredColumn(page, "#af-token");
      await still(page, info, "login-expired");
    });
    test("Connecting column", async ({ page }, info) => {
      let release!: () => void;
      const waiting = new Promise<void>(resolve => { release = resolve; });
      await page.route("**/v1/auth-info", async route => { await waiting; await route.continue(); });
      await page.goto("/", { waitUntil: "domcontentloaded" });
      await expect(page.getByRole("heading", { name: "Connecting…" })).toBeVisible();
      await centeredColumn(page);
      await still(page, info, "connecting");
      release();
    });
    test("Sign-in column", async ({ page }, info) => {
      await page.route("**/v1/auth-info", route => route.fulfill({ json: envelope({ auth_required: true }) }));
      await page.goto("/");
      await expect(page.locator("#af-token")).toBeVisible();
      await centeredColumn(page, "#af-token");
      await still(page, info, "sign-in");
    });
    test("Tokenless column", async ({ page }, info) => {
      await page.goto("/");
      await expect(page.locator(".af-app")).toBeVisible();
      await page.getByRole("button", { name: "Disconnect", exact: true }).click();
      await expect(page.locator(".af-login-form button")).toBeVisible();
      await centeredColumn(page, ".af-login-form button");
      await still(page, info, "tokenless");
    });
    test("Persistent notice dismisses", async ({ page }, info) => {
      await page.route("**/v1/UpdateTask", route => route.fulfill(refusal));
      await page.goto("/");
      await expect(page.locator(".af-app")).toBeVisible();
      await page.getByRole("tab", { name: "Tasks", exact: true }).click();
      const task = page.locator(".af-task-row").first();
      await task.locator(".af-term-more").click();
      await task.getByRole("button", { name: "Disable", exact: true }).click();
      await expect(page.locator(".af-toast-show")).toBeVisible();
      await still(page, info, "notice");
      await page.getByRole("button", { name: "Dismiss", exact: true }).click();
      await expect(page.locator(".af-toast-show")).toHaveCount(0);
    });
    test("No accounts", async ({ page }, info) => {
      await page.route("**/v1/ListAccounts", route => route.fulfill({ json: envelope({ entries: [], agents: ["claude"] }) }));
      await page.goto("/");
      await expect(page.locator(".af-app")).toBeVisible();
      await page.getByRole("tab", { name: "Config", exact: true }).click();
      await expect(page.getByRole("heading", { name: "No accounts" })).toBeVisible();
      await still(page, info, "no-accounts");
      await page.getByRole("button", { name: "Add account", exact: true }).click();
      await expect(page.locator("[data-account-input]")).toBeVisible();
    });
    for (const operation of ["create", "archive", "kill", "task save"] as const) {
      test(`${operation} failed`, async ({ page }, info) => {
        await page.goto("/");
        await expect(page.locator(".af-app")).toBeVisible();
        if (operation === "create") {
          await page.route("**/v1/CreateSession", route => route.fulfill(refusal));
          await page.locator(".af-rail-new").click();
          await page.getByRole("textbox", { name: "Session title", exact: true }).fill("Retained draft");
          await page.locator(".af-modal-card button[type=submit]").click();
          await expect(page.locator(".af-modal-error")).toBeVisible();
          await expect(page.getByRole("textbox", { name: "Session title", exact: true })).toHaveValue("Retained draft");
        } else if (operation === "task save") {
          await page.route("**/v1/AddTask", route => route.fulfill(refusal));
          await page.getByRole("tab", { name: "Tasks", exact: true }).click();
          await page.locator(".af-tasks-add").click();
          await page.getByRole("textbox", { name: "Task name", exact: true }).fill("Retained task");
          await page.locator(".af-modal-card textarea").first().fill("Retained prompt");
          await page.locator(".af-modal-card button[type=submit]").click();
          await expect(page.locator(".af-modal-error")).toBeVisible();
          await expect(page.getByRole("textbox", { name: "Task name", exact: true })).toHaveValue("Retained task");
        } else {
          await page.route(`**/v1/${operation === "kill" ? "KillSession" : "ArchiveSession"}`, route => route.fulfill(refusal));
          const row = page.locator(".af-row").first();
          await row.hover();
          await row.getByRole("button", { name: /^Actions for / }).click();
          await row.getByRole("button", { name: new RegExp(`^${operation === "kill" ? "Kill" : "Archive"} session`) }).click();
          await page.locator(".af-modal-card button[type=submit]").click();
          await expect(page.locator(".af-modal-error")).toBeVisible();
        }
        await still(page, info, `${operation}-failed`);
      });
    }
  });
}
