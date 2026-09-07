import { expect, test, type APIRequestContext, type Page, type Route, type WebSocketRoute } from "@playwright/test";
import { InFlightOp, Liveness, TabKind, type SessionData } from "../src/types.js";

function latch() {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
}

// Preserve real fixture capabilities while controlling the independent HTTP and
// event channels. None of these race tests creates/kills an actual daemon session.
async function fixture(page: Page, request: APIRequestContext) {
  const response = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
  expect(response.ok()).toBe(true);
  const envelope = await response.json();
  const instances = envelope.data.instances as SessionData[];
  const a = instances.find(s => s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"))!;
  const b = instances.find(s => s.title === (process.env.AF_WEB_SESSION_B ?? "probe-b"))!;
  expect(a?.id).toBeTruthy();
  expect(b?.id).toBeTruthy();
  expect(a.worktree?.repo_path).toBe(b.worktree?.repo_path);
  let events!: WebSocketRoute;
  const state = {
    rows: [a, b],
    snapshotHook: undefined as undefined | ((route: Route) => Promise<void>),
  };
  const fulfillSnapshot = (route: Route) => route.fulfill({
    json: { ...envelope, data: { ...envelope.data, instances: state.rows } },
  });
  await page.route("**/v1/Snapshot", route => state.snapshotHook?.(route) ?? fulfillSnapshot(route));
  await page.routeWebSocket(url => url.pathname === "/v1/events", socket => { events = socket; });
  await page.routeWebSocket(url => url.pathname.endsWith("/stream"), () => {});
  await page.goto(`/#/session/${encodeURIComponent(a.id!)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  const row = (title: string) => page.locator(".af-row").filter({
    has: page.locator(".af-row-title", { hasText: title }),
  });
  return { a, b, state, row, fulfillSnapshot,
    event: (type: string, data: SessionData) => events.send(JSON.stringify({ type, data })),
  };
}

async function submitCreate(page: Page, title: string) {
  await page.locator(".af-rail-new").click();
  await page.getByRole("textbox", { name: "Session title", exact: true }).fill(title);
  await page.locator(".af-modal-card button[type=submit]").click();
}

test("completed create event plus lost HTTP reply does not offer a duplicate create", async ({ page, request }) => {
  const f = await fixture(page, request);
  const pending = latch();
  await page.route("**/v1/CreateSession", async route => {
    await pending.promise;
    await route.abort("failed");
  });
  const title = "Confirmed race create";
  const requested = page.waitForRequest("**/v1/CreateSession");
  await submitCreate(page, title);
  await requested;
  const created = { ...f.a, id: "race-created-session", title, in_flight_op: InFlightOp.None };
  f.state.rows.push(created);
  f.event("session.created", created);
  await expect(f.row(title).filter({ has: page.locator(".af-row-actions button") })).toHaveCount(1);
  pending.release();
  await expect(page.locator(".af-toast .af-recovery-notice")).toBeVisible();
  await expect(page.locator(".af-modal-card")).toHaveCount(0);
  await expect(page.locator(".af-row-creating")).toHaveCount(0);
  await expect(f.row(title)).toHaveCount(1);
});

for (const operation of ["archive", "kill"] as const) {
  test(`completed ${operation} event plus lost HTTP reply does not reopen confirmation`, async ({ page, request }) => {
    const f = await fixture(page, request);
    const pending = latch();
    const method = operation === "archive" ? "ArchiveSession" : "KillSession";
    await page.route(`**/v1/${method}`, async route => {
      await pending.promise;
      await route.abort("failed");
    });
    const row = f.row(f.a.title);
    await row.getByRole("button", { name: /^Actions for / }).click();
    await row.getByRole("button", { name: new RegExp(`^${operation === "archive" ? "Archive" : "Delete"} session`) }).click();
    const requested = page.waitForRequest(`**/v1/${method}`);
    await page.locator(".af-modal-card button[type=submit]").click();
    await requested;
    if (operation === "archive") {
      const archived = { ...f.a, liveness: Liveness.Archived, lifecycle_action: "restore" as const,
        in_flight_op: InFlightOp.None };
      f.state.rows = [archived, f.b];
      f.event("session.archived", archived);
    } else {
      f.state.rows = [f.b];
      f.event("session.killed", f.a);
    }
    await expect(row).toHaveCount(0);
    pending.release();
    await expect(page.locator(".af-toast .af-recovery-notice")).toBeVisible();
    await expect(page.locator(".af-modal-card")).toHaveCount(0);
    await expect(row).toHaveCount(0);
  });
}

for (const navigation of ["session", "view"] as const) {
  test(`create reply preserves ${navigation} navigation away and back`, async ({ page, request }) => {
    const f = await fixture(page, request);
    const pending = latch();
    const created = { ...f.a, id: `race-aba-${navigation}`, title: `ABA ${navigation} create`, in_flight_op: InFlightOp.None };
    await page.route("**/v1/CreateSession", async route => {
      await pending.promise;
      f.state.rows.push(created);
      await route.fulfill({ json: { data: { instance: created }, error: null } });
    });
    const requested = page.waitForRequest("**/v1/CreateSession");
    await submitCreate(page, created.title);
    await requested;
    if (navigation === "session") {
      await f.row(f.b.title).click();
      await f.row(f.a.title).click();
    } else {
      await page.locator('.af-viewtab[data-view="tasks"]').click();
      await page.locator('.af-viewtab[data-view="sessions"]').click();
    }
    await expect(page.locator(".af-term-title")).toHaveText(f.a.title);
    pending.release();
    await expect(f.row(created.title)).not.toHaveClass(/af-row-creating/);
    await expect(page.locator(".af-row-selected .af-row-title")).toHaveText(f.a.title);
    await expect(page.locator(".af-term-title")).toHaveText(f.a.title);
  });
}

test("tab create retries a fenced snapshot before resolving and attaching its new tab", async ({ page, request }) => {
  const f = await fixture(page, request);
  const waitingSnapshot = latch();
  const releaseSnapshot = latch();
  const tab = { id: "race-created-tab", name: "race-shell", kind: TabKind.Shell };
  let snapshotRequests = 0;
  await page.route("**/v1/CreateTab", async route => {
    f.state.rows = [{ ...f.a, tabs: [...(f.a.tabs ?? []), tab] }, f.b];
    f.state.snapshotHook = async snapshotRoute => {
      snapshotRequests++;
      if (snapshotRequests === 1) {
        waitingSnapshot.release();
        await releaseSnapshot.promise;
      }
      await f.fulfillSnapshot(snapshotRoute);
    };
    await route.fulfill({ json: { data: { name: tab.name }, error: null } });
  });
  await page.locator(".af-main .af-term-more").click();
  await page.locator(".af-tab-new").click();
  await page.locator(".af-tab-menu-item").filter({ hasText: /^Terminal$/ }).click();
  await waitingSnapshot.promise;
  const changed = { ...f.b, branch: "race-ledger-event" };
  f.state.rows = [f.state.rows[0], changed];
  f.event("session.updated", changed);
  // Seeing this branch proves the event crossed the in-flight snapshot fence.
  await expect(f.row(f.b.title)).toContainText(changed.branch);
  releaseSnapshot.release();
  await expect.poll(() => snapshotRequests).toBeGreaterThanOrEqual(2);
  await expect(page.locator(".af-term-host .af-pane")).toHaveAttribute("data-tab-id", tab.id);
  await expect(page.locator(".af-term-host .xterm-helper-textarea")).toBeFocused();
  await expect(page.locator(".af-toast")).not.toHaveClass(/af-toast-show/);
});

for (const status of [502, 504]) {
  test(`JSON gateway ${status} after create never offers an immediate duplicate retry`, async ({ page, request }) => {
    const f = await fixture(page, request);
    const pending = latch();
    const title = `Gateway ${status} create`;
    await page.route("**/v1/CreateSession", async route => {
      await pending.promise;
      await route.fulfill({ status, json: { data: null, error: { message: "Upstream response unavailable" } } });
    });
    const requested = page.waitForRequest("**/v1/CreateSession");
    await submitCreate(page, title);
    await requested;
    const created = { ...f.a, id: `gateway-created-${status}`, title, in_flight_op: InFlightOp.None };
    f.state.rows.push(created);
    f.event("session.created", created);
    await expect(f.row(title).filter({ has: page.locator(".af-row-actions button") })).toHaveCount(1);
    pending.release();
    const notice = page.locator(".af-toast .af-recovery-notice");
    await expect(notice).toContainText("Outcome not confirmed");
    await expect(notice).toContainText("Check the session before acting.");
    await expect(notice).not.toContainText("Operation failed");
    await expect(notice).not.toContainText("then try again");
    await expect(page.locator(".af-toast .af-recovery-notice")).toContainText("Upstream response unavailable");
    await expect(page.locator(".af-modal-card")).toHaveCount(0);
    await expect(f.row(title)).toHaveCount(1);
  });
}

for (const operation of ["archive", "kill"] as const) {
  test(`lost ${operation} reply before completion stays uncertain until a later snapshot`, async ({ page, request }) => {
    const f = await fixture(page, request);
    const pending = latch();
    const snapshotRequested = latch();
    const releaseSnapshot = latch();
    const snapshotCompleted = latch();
    const method = operation === "archive" ? "ArchiveSession" : "KillSession";
    await page.route(`**/v1/${method}`, async route => {
      await pending.promise;
      await route.abort("failed");
    });
    const row = f.row(f.a.title);
    await row.getByRole("button", { name: /^Actions for / }).click();
    await row.getByRole("button", { name: new RegExp(`^${operation === "archive" ? "Archive" : "Delete"} session`) }).click();
    const requested = page.waitForRequest(`**/v1/${method}`);
    await page.locator(".af-modal-card button[type=submit]").click();
    await requested;
    await expect(row).toContainText(`[deleting] ${f.a.title}`);
    f.state.snapshotHook = async route => {
      snapshotRequested.release();
      await releaseSnapshot.promise;
      await f.fulfillSnapshot(route);
      snapshotCompleted.release();
    };
    pending.release();
    const notice = page.locator(".af-toast .af-recovery-notice");
    await expect(notice).toContainText(`The ${operation} outcome could not be confirmed`);
    await expect(notice).toContainText("Outcome not confirmed");
    await expect(notice).toContainText("Check the session before acting.");
    await expect(notice).not.toContainText("Operation failed");
    await expect(notice).not.toContainText("then try again");
    await expect(page.locator(".af-modal-card")).toHaveCount(0);
    await expect(row).toContainText(`[deleting] ${f.a.title}`);
    await snapshotRequested.promise;
    f.state.rows = operation === "archive"
      ? [{ ...f.a, liveness: Liveness.Archived, lifecycle_action: "restore", in_flight_op: InFlightOp.None }, f.b]
      : [f.b];
    releaseSnapshot.release();
    await snapshotCompleted.promise;
    // Completion arrives solely through this snapshot; no event owns the result.
    await expect(row).toHaveCount(0);
    await expect(page.locator(".af-modal-card")).toHaveCount(0);
    await expect(notice).toBeVisible();
  });
}
