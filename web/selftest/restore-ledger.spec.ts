import { expect, test, type APIRequestContext, type Page, type WebSocketRoute } from "@playwright/test";
import { InFlightOp, Liveness, TabKind, type SessionData } from "../src/types.js";

function latch() {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
}

async function fixture(page: Page, request: APIRequestContext) {
  const envelope = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const rows = envelope.data.instances as SessionData[];
  const a = { ...rows.find(s => s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"))!,
    liveness: Liveness.Lost, in_flight_op: InFlightOp.None, lifecycle_action: "restore" as const, can_kill: true };
  const b = rows.find(s => s.title === (process.env.AF_WEB_SESSION_B ?? "probe-b"))!;
  const state = { rows: [a, b] as SessionData[], failSnapshot: false, snapshots: 0 };
  let events!: WebSocketRoute;
  await page.routeWebSocket(url => url.pathname === "/v1/events", socket => { events = socket; });
  await page.routeWebSocket(url => url.pathname.endsWith("/stream"), () => {});
  await page.route("**/v1/Snapshot", route => {
    state.snapshots++;
    return state.failSnapshot ? route.abort("connectionfailed")
      : route.fulfill({ json: { data: { ...envelope.data, instances: state.rows } } });
  });
  await page.goto(`/#/session/${encodeURIComponent(a.id!)}`);
  await expect(page.locator("#app")).toHaveAttribute("data-af-resync-settled", "");
  const row = (session: SessionData) => page.locator(".af-row").filter({ has: page.locator(".af-row-title", { hasText: session.title }) });
  const actions = () => row(a).getByRole("button", { name: `Actions for ${a.title}`, exact: true });
  const restore = () => row(a).getByRole("button", { name: `Restore session “${a.title}”`, exact: true });
  const start = async () => { await actions().click(); await restore().click(); };
  return { a, b, state, row, actions, restore, start,
    event: (type: string, data: SessionData) => events.send(JSON.stringify({ type, data })) };
}

test("reconnecting during restore preserves the fence until the post-response Snapshot", async ({ page, request }) => {
  const f = await fixture(page, request);
  const wait = latch(); let restores = 0;
  await page.route("**/v1/RestoreSession", async route => { restores++; await wait.promise; await route.fulfill({ json: { data: {} } }); });
  await f.start(); await expect.poll(() => restores).toBe(1); await page.keyboard.press("Escape");
  await page.locator(".af-appbar button", { hasText: "Disconnect" }).evaluate((button: HTMLButtonElement) => button.click());
  await page.locator(".af-login button.af-primary").click();
  await expect(page.locator("#app")).toHaveAttribute("data-af-resync-settled", "");
  await f.actions().click();
  await expect(f.restore()).toBeDisabled();
  await f.restore().evaluate((button: HTMLButtonElement) => button.click());
  expect(restores).toBe(1);
  const before = f.state.snapshots;
  wait.release();
  await expect.poll(() => f.state.snapshots).toBeGreaterThan(before);
  await expect(f.restore()).toBeEnabled();
  expect(restores).toBe(1);
});

test("optimistic Kill cannot settle an uncertain restore", async ({ page, request }) => {
  const f = await fixture(page, request);
  const restoreWait = latch(); const killWait = latch(); let restores = 0; let kills = 0;
  await page.route("**/v1/RestoreSession", async route => { restores++; await restoreWait.promise; await route.abort("connectionfailed"); });
  await page.route("**/v1/KillSession", async route => { kills++; await killWait.promise; await route.fulfill({ json: { error: { message: "restore owns fence", daemon_rejected: true } } }); });
  await f.start(); await expect.poll(() => restores).toBe(1); await page.keyboard.press("Escape");
  await f.actions().click();
  await f.row(f.a).getByRole("button", { name: `Kill session “${f.a.title}”`, exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete session", exact: true }).click();
  await expect.poll(() => kills).toBe(1);
  f.state.failSnapshot = true;
  restoreWait.release();
  await expect(page.getByText("Outcome not confirmed", { exact: true })).toBeVisible();
  killWait.release();
  await expect(page.getByRole("dialog")).toContainText("restore owns fence");
  await page.getByRole("dialog").getByRole("button", { name: "Cancel", exact: true }).click();
  await f.actions().click();
  await expect(f.restore()).toBeDisabled();
  await f.restore().evaluate((button: HTMLButtonElement) => button.click());
  expect(restores).toBe(1);
});

test("a tab mutation Snapshot also settles a completed restore", async ({ page, request }) => {
  const f = await fixture(page, request);
  let restores = 0;
  await page.route("**/v1/RestoreSession", route => { restores++; f.state.failSnapshot = true; return route.fulfill({ json: { data: {} } }); });
  const beforeRestore = f.state.snapshots;
  await f.start(); await expect.poll(() => restores).toBe(1);
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect.poll(() => f.state.snapshots).toBeGreaterThan(beforeRestore);
  await f.actions().click(); await expect(f.restore()).toBeDisabled();
  let creates = 0;
  await page.route("**/v1/CreateTab", route => {
    creates++;
    f.b.tabs = [...(f.b.tabs ?? []), { id: "ledger-shell", name: "ledger-shell", kind: TabKind.Shell }];
    f.state.rows = [{ ...f.a, liveness: Liveness.Running, lifecycle_action: "archive" }, f.b];
    f.state.failSnapshot = false;
    return route.fulfill({ json: { data: { name: "ledger-shell" } } });
  });
  await f.row(f.b).click(); await page.keyboard.press("Control+]"); await page.keyboard.press("t");
  await expect.poll(() => creates).toBe(1);
  await expect(f.row(f.a)).not.toHaveAttribute("data-state", "lost");
  f.event("session.updated", f.a);
  await expect(f.row(f.a)).toHaveAttribute("data-state", "lost");
  // Creating the tab selected B. A's actions are intentionally withdrawn until
  // hover/focus; reveal that surface before clicking its disclosure.
  await f.row(f.a).hover();
  await f.actions().click();
  await expect(f.restore()).toBeEnabled();
});
