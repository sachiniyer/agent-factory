import { test, expect, type WebSocketRoute } from "@playwright/test";
import { TabKind } from "../src/types.js";

for (const kind of [TabKind.Shell, TabKind.Process, TabKind.VSCode, TabKind.Web]) {
  test(`tab deletion asks for the exact target, kind ${kind}`, async ({ page, request }, info) => {
    const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
    const original = envelope.data.instances.find((s: { title: string }) => s.title === "probe-a");
    const target = { id: "consent-tab", name: "exact build logs", kind };
    const other = { id: "other-tab", name: "other", kind: TabKind.Shell };
    const session = { ...original, tabs: [{ id: "agent-tab", name: "agent", kind: TabKind.Agent }, target, other] };
    await page.route("**/v1/Snapshot", route => route.fulfill({ json: {
      ...envelope, data: { ...envelope.data, instances: [session] },
    } }));
    let events!: WebSocketRoute;
    await page.routeWebSocket("**/v1/events*", socket => { events = socket; });
    await page.routeWebSocket("**/stream*", () => {});
    let calls = 0;
    await page.route("**/v1/CloseTab", route => {
      calls++;
      expect(route.request().postDataJSON()).toMatchObject({ id: original.id, tab_id: target.id, tab_name: target.name });
      return route.fulfill({ status: 503, json: { error: { message: "Deletion refused", daemon_rejected: true } } });
    });
    await page.goto(`/#/session/${original.id}`);
    const close = page.locator(".af-tab-close").first();
    await close.click();
    await page.screenshot({ path: info.outputPath("tab-delete.png") });
    const modal = page.locator(".af-modal-card");
    await expect(modal).toContainText(target.name);
    await expect(modal).toContainText("Hiding a pane");
    expect(calls).toBe(0);
    await page.keyboard.press("Escape");
    await expect(modal).toHaveCount(0);
    expect(calls).toBe(0);
    await page.locator(".af-tab").nth(1).click();
    await page.keyboard.press("Control+]");
    await page.keyboard.press("w");
    await expect(modal).toContainText(target.name);
    expect(calls).toBe(0);
    session.tabs = [session.tabs[0], other, target];
    events.send(JSON.stringify({ type: "session.updated", data: session }));
    await expect(page.locator(".af-tab-close").last()).toHaveAttribute("title", `Delete tab “${target.name}”`);
    await modal.getByRole("button", { name: "Delete tab", exact: true }).click();
    await expect.poll(() => calls).toBe(1);
  });
}
