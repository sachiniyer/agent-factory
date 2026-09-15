import { expect, test } from "@playwright/test";

test("handoff marks unauthenticated accounts and never preselects their default", async ({ page, request }) => {
 const response = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
 const original = await response.json();
 const instance = original.data.instances.find((i: { title: string }) => i.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
 expect(instance).toBeTruthy();
 instance.current_agent = "claude";
 instance.can_handoff = true;
 instance.account = "work";
 await page.routeWebSocket("**/v1/events*", () => {});
 await page.route("**/v1/Snapshot", route => route.fulfill({ json: { ...original, data: { ...original.data, instances: [instance] } } }));
 await page.route("**/v1/ListPrograms", route => route.fulfill({ json: { data: { programs: [{ name: "claude" }, { name: "codex" }] }, error: null } }));
 await page.route("**/v1/ListAccounts", route => route.fulfill({ json: { data: { agents: ["claude"], defaults: { claude: "personal" }, entries: [
  { agent: "claude", name: "personal", dir: "", logged_in: false, registration_only: false },
  { agent: "claude", name: "spare", dir: "", logged_in: true, registration_only: false },
 ] }, error: null } }));
 await page.goto(`/#/session/${encodeURIComponent(instance.id)}`);
 await page.locator("#app[data-af-resync-settled]").waitFor();
 await page.getByRole("button", { name: "Session actions", exact: true }).click();
 await page.getByRole("button", { name: "Handoff", exact: true }).click();
 const dialog = page.getByRole("dialog");
 const account = dialog.getByRole("combobox", { name: "New account", exact: true });
 await expect(account.locator('option[value="personal"]')).toContainText("not logged in");
 await expect(account).toHaveValue("spare");
 await expect(dialog.locator(".af-account-hint")).toHaveText("");
 await account.selectOption("personal");
 await expect(dialog.locator(".af-account-hint")).toContainText("personal has no claude credential yet");
 await expect(dialog.getByRole("button", { name: "Hand off", exact: true })).toBeEnabled();
 await page.screenshot({ path: test.info().outputPath("handoff-credential-warning.png") });
 await account.selectOption("spare");
 await expect(dialog.locator(".af-account-hint")).toHaveText("");
 await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
 await page.unrouteAll({ behavior: "wait" });
});

for (const hasTarget of [true, false]) {
 test(`handoff without another current-agent account: target available=${hasTarget}`, async ({ page, request }) => {
  const response = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
  const original = await response.json();
  const instance = original.data.instances.find((i: { title: string }) => i.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  Object.assign(instance, { current_agent: "claude", can_handoff: true, account: "work" });
  await page.routeWebSocket("**/v1/events*", () => {});
  await page.route("**/v1/Snapshot", route => route.fulfill({ json: { ...original, data: { ...original.data, instances: [instance] } } }));
  await page.route("**/v1/ListPrograms", route => route.fulfill({ json: { data: { programs: [{ name: "claude" }, { name: "codex" }] }, error: null } }));
  await page.route("**/v1/ListAccounts", route => route.fulfill({ json: { data: { agents: ["claude", "codex"], entries: [
   { agent: "claude", name: "work", dir: "", logged_in: true, registration_only: false },
   ...(hasTarget ? [{ agent: "codex", name: "spare", dir: "", logged_in: true, registration_only: false }] : []),
  ] }, error: null } }));
  await page.goto(`/#/session/${encodeURIComponent(instance.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await page.getByRole("button", { name: "Session actions", exact: true }).click();
  await page.getByRole("button", { name: "Handoff", exact: true }).click();
  const dialog = page.getByRole("dialog");
  const agent = dialog.getByRole("combobox", { name: "New agent", exact: true });
  await expect(agent.locator('option[value="claude"]')).toHaveCount(0);
  if (hasTarget) {
   await expect(agent).toHaveValue("codex");
   await expect(dialog.getByRole("combobox", { name: "New account", exact: true })).toHaveValue("spare");
   await expect(dialog.getByRole("button", { name: "Hand off", exact: true })).toBeEnabled();
  } else {
   await expect(dialog).toContainText("No registered target account is available");
   await expect(dialog.getByRole("button", { name: "Hand off", exact: true })).toBeDisabled();
  }
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.unrouteAll({ behavior: "wait" });
 });
}
