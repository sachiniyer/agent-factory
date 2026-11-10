import { expect, test, type APIRequestContext } from "@playwright/test";
import type { SessionData } from "../src/types.js";

// T7 — e2e guard for the "could not check vs gone" fix in resolveArchivedRoute
// (web/src/index.ts). A transient NON-auth failure of the by-id deep-link read
// (fetchSessionsById → Snapshot with `ids`) must NOT be false-reported as a
// deleted session: the #/session/<id> hash is preserved (not erased to /), no
// "No session with that id in this daemon" toast fires, and a "could not check"
// notice is surfaced. A subsequent successful read resolves the still-present
// link. The daemon's events stream stays live throughout (only the Snapshot RPC
// is intercepted), so this is the realistic "momentary blip on one call" trigger.

async function session(request: APIRequestContext, title: string): Promise<SessionData & { id: string }> {
  const response = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
  expect(response.ok()).toBeTruthy();
  const body = await response.json();
  const found = body.data.instances.find((s: SessionData) => s.title === title);
  expect(found?.id).toBeTruthy();
  return found;
}

const fragment = (id: string) => `#/session/${encodeURIComponent(id)}`;

test("a transient 5xx on the by-id deep-link read preserves the hash and surfaces 'could not check', not 'missing'", async ({ page, request }) => {
  const archived = await session(request, process.env.AF_WEB_SESSION_WEB_SHELVED ?? "probe-shelved");
  // Fail ONLY the by-id Snapshot (the one carrying `ids`) the first time, then
  // let it through. The initial snapshot (lazy_archive) and every other call
  // continue to the real daemon untouched.
  let failById = true;
  await page.route("**/v1/Snapshot", async (route) => {
    const body = route.request().postDataJSON() as { ids?: string[] };
    if (body?.ids && failById) {
      failById = false;
      await route.fulfill({
        status: 502,
        contentType: "application/json",
        body: JSON.stringify({ data: null, error: { message: "bad gateway (injected)" } }),
      });
      return;
    }
    await route.continue();
  });

  await page.goto("/" + fragment(archived.id));

  // The hash is PRESERVED — not erased to "/" by replaceRoute(null).
  await expect(page).toHaveURL(new RegExp(fragment(archived.id) + "$"));
  // The false "session was deleted" toast does NOT fire.
  await expect(page.locator(".af-toast")).not.toContainText("No session with that id in this daemon");
  // A "could not check" notice is surfaced instead.
  await expect(page.locator(".af-toast")).toContainText(/Could not look up that session/);

  // Now let the by-id read succeed and re-resolve: reload the preserved hash.
  await page.reload();
  // The archived session resolves — the link was preserved, the lookup re-ran.
  await expect(page.locator(".af-term-title")).toContainText(archived.title!);
  await expect(page.locator(".af-row-selected.af-row-archived")).toContainText(archived.title!);
});

test("a transport failure (connectionrefused) on the by-id read preserves the hash too", async ({ page, request }) => {
  const archived = await session(request, process.env.AF_WEB_SESSION_WEB_SHELVED ?? "probe-shelved");
  let failById = true;
  await page.route("**/v1/Snapshot", async (route) => {
    const body = route.request().postDataJSON() as { ids?: string[] };
    if (body?.ids && failById) {
      failById = false;
      await route.abort("connectionrefused");
      return;
    }
    await route.continue();
  });

  await page.goto("/" + fragment(archived.id));

  await expect(page).toHaveURL(new RegExp(fragment(archived.id) + "$"));
  await expect(page.locator(".af-toast")).not.toContainText("No session with that id in this daemon");
  await expect(page.locator(".af-toast")).toContainText(/Could not look up that session/);

  await page.reload();
  await expect(page.locator(".af-term-title")).toContainText(archived.title!);
});
