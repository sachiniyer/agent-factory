import { expect, test } from "@playwright/test";

for (const [registrationOnly, loggedIn] of [[true, true], [false, true], [false, false]]) {
  test(`inherited account keeps restrictions: registrationOnly=${registrationOnly}, loggedIn=${loggedIn}`, async ({ page }, info) => {
    await page.route("**/v1/ListPrograms", route => route.fulfill({ json: { data: {
      programs: [{ name: "claude", available: true }], default: "claude",
    }, error: null } }));
    await page.route("**/v1/ListAccounts", route => route.fulfill({ json: { data: {
      agents: ["claude"], defaults: { claude: "work" },
      entries: [{ agent: "claude", name: "work", dir: "/fixture/work", registration_only: registrationOnly, logged_in: loggedIn }],
    }, error: null } }));
    await page.goto("/");
    await page.locator("button.af-rail-new").click();
    const modal = page.getByRole("dialog");
    const select = modal.getByLabel("Account", { exact: true });
    await expect(select).toHaveValue("work");
    if (!(await select.isVisible())) await modal.locator(".af-defaults summary").click();
    const notice = modal.locator(".af-account-hint");
    const explicitNotice = await notice.textContent();
    await select.selectOption("");
    await expect(select).toHaveValue("");
    await expect(notice).toHaveText(explicitNotice ?? "");
    const create = modal.getByRole("button", { name: "Create", exact: true });
    if (registrationOnly) {
      await expect(create).toBeDisabled();
      await expect(modal).toContainText("cannot be scoped");
    } else {
      await expect(create).toBeEnabled();
      if (!loggedIn) await expect(modal).toContainText("has no claude credential yet");
    }
    await page.screenshot({ path: info.outputPath("inherited-account.png") });
    await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  });
}
