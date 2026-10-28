import { test, expect, type WebSocketRoute } from "@playwright/test";
import { TabKind } from "../src/types.js";

for (const replace of [false, true]) {
  test(`legacy deletion ${replace ? "refuses a same-name replacement" : "deletes the unchanged tab"}`, async ({ page, request }) => {
    const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
    const original = envelope.data.instances.find((s: { title: string }) => s.title === "probe-a");
    const tab = { name: "legacy logs", kind: TabKind.Process };
    const session = { ...original, tabs: [{ id: "agent-id", name: "agent", kind: TabKind.Agent }, tab] };
    await page.route("**/v1/Snapshot", route => route.fulfill({ json: {
      ...envelope, data: { ...envelope.data, instances: [session] },
    } }));
    let events!: WebSocketRoute;
    await page.routeWebSocket("**/v1/events*", socket => { events = socket; });
    await page.routeWebSocket("**/stream*", () => {});
    let calls = 0;
    await page.route("**/v1/CloseTab", route => {
      calls++;
      expect(route.request().postDataJSON()).toMatchObject({ id: original.id, tab_id: "", tab_name: tab.name });
      return route.fulfill({ status: 503, json: { error: { message: "Legacy delete reached daemon", daemon_rejected: true } } });
    });
    await page.goto(`/#/session/${original.id}`);
    // Opening the stream triggers one startup resync; let it commit before consent.
    await page.locator("#app[data-af-resync-settled]").waitFor();
    await page.locator(".af-tab-close").click();
    const modal = page.locator(".af-modal-card");
    await expect(modal).toContainText(tab.name);
    if (replace) {
      // A whole newer projection holds another object with the same kind/name.
      // The changed row title is an observable barrier proving the event landed.
      const updated = { ...session, title: "newer legacy projection", tabs: [session.tabs[0], { ...tab }] };
      events.send(JSON.stringify({ type: "session.updated", data: updated }));
      await expect(page.locator(".af-row-title")).toHaveText(updated.title);
    }
    await modal.getByRole("button", { name: "Delete tab", exact: true }).click();
    if (replace) {
      await expect(modal.locator(".af-modal-error")).toContainText("This tab is no longer available to delete.");
      expect(calls).toBe(0);
    } else {
      await expect(modal).toHaveCount(0);
      await expect.poll(() => calls).toBe(1);
    }
  });
}
