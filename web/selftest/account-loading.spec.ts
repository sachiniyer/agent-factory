import { test, expect } from "@playwright/test";

for (const state of ["pending", "failed", "loaded-empty"] as const) {
  test(`new-session account policy: ${state}`, async ({ page }) => {
    let release!: () => void;
    const waiting = new Promise<void>(resolve => { release = resolve; });
    await page.route("**/v1/ListAccounts", async route => {
      if (state === "pending") await waiting;
      await route.fulfill(state === "failed"
        ? { status: 503, json: { data: null, error: { message: "Registry unavailable" } } }
        : { json: { data: { agents: ["claude"], entries: [], defaults: {} }, error: null } });
    });
    try {
      await page.goto("/");
      await page.locator(".af-rail-new").click();
      const modal = page.getByRole("dialog");
      await modal.getByLabel("Session title", { exact: true }).fill("account-policy-probe");
      const label = state === "pending" ? "Loading accounts…"
        : state === "failed" ? "Accounts unavailable" : "Use agent login (no default)";
      await expect(modal.getByLabel("Account", { exact: true }).locator("option:checked")).toHaveText(label);
      const create = modal.getByRole("button", { name: "Create", exact: true });
      if (state === "pending") {
        await expect(create).toBeDisabled();
        release();
        await expect(modal.getByLabel("Account", { exact: true }).locator("option:checked")).toHaveText("Use agent login (no default)");
        await expect(create).toBeEnabled();
      } else {
        await expect(create).toBeEnabled();
        if (state === "failed") await expect(modal).toContainText("daemon default, if any, applies");
      }
      await modal.getByRole("button", { name: "Cancel", exact: true }).click();
    } finally {
      release();
    }
  });
}

test("explicit account survives a project switch and reaches CreateSession", async ({ page }) => {
  let loads = 0;
  let release!: () => void;
  const waiting = new Promise<void>(resolve => { release = resolve; });
  await page.route("**/v1/ListPrograms", route => route.fulfill({ json: {
    data: { programs: [{ name: "claude" }], default: "claude" }, error: null,
  } }));
  await page.route("**/v1/ListAccounts", async route => {
    if (++loads > 1) await waiting;
    await route.fulfill({ json: { data: {
      agents: ["claude"], defaults: { claude: "personal" },
      entries: ["personal", "work"].map(name => ({
        agent: "claude", name, dir: `/accounts/${name}`, registration_only: false, logged_in: true,
      })),
    }, error: null } });
  });
  // Refuse the synthetic create after observing its payload; no real session needed.
  await page.route("**/v1/CreateSession", route => route.fulfill({ json: {
    data: null, error: { message: "Account payload captured" },
  } }));
  try {
    await page.goto("/");
    await page.locator(".af-rail-new").click();
    const modal = page.getByRole("dialog");
    const account = modal.getByLabel("Account", { exact: true });
    await expect(account.locator('option[value="work"]')).toHaveCount(1);
    if (!await account.isVisible()) await modal.locator(".af-defaults summary").click();
    await account.selectOption("work");
    const project = modal.getByLabel("Project", { exact: true });
    const current = await project.inputValue();
    const other = await project.locator("option").evaluateAll((options, current) =>
      options.map(option => (option as HTMLOptionElement).value).find(value => value && value !== current), current);
    expect(other).toBeTruthy();
    await project.selectOption(other!);
    await expect(account.locator("option:checked")).toHaveText("Loading accounts…");
    await expect(modal.getByRole("button", { name: "Create", exact: true })).toBeDisabled();
    release();
    await expect(account).toHaveValue("work");
    await modal.getByLabel("Session title", { exact: true }).fill("account-reload-probe");
    const request = page.waitForRequest("**/v1/CreateSession");
    await modal.getByRole("button", { name: "Create", exact: true }).click();
    expect((await request).postDataJSON().account).toBe("work");
  } finally {
    release();
  }
});
