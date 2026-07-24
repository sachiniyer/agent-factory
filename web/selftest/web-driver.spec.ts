// web-driver-selftest (#1592 Phase 5 PR6) — the acceptance proof for the embedded
// browser web client, the browser analogue of tui-driver-selftest.sh.
//
// It drives a headless Chromium against a REAL af daemon (a throwaway home on a
// loopback plain-HTTP listener, brought up by web-selftest-entry.sh) and asserts the core
// v1 loop end to end — assertions are the gate, not screenshots:
//
//   1. tokenless open  no token required ⇒ the SPA auto-connects with NO credential
//                      and NEVER shows the paste-token login (#1696); the decision
//                      follows the daemon's /v1/auth-info answer, and a forced
//                      auth_required=true brings the login back even on loopback
//   1b. real errors    a failed login renders the daemon's own message — never the
//                      literal "[object Object]" the string/object envelope fork gave
//   2. sidebar         the rail lists the sessions from the Snapshot/events plane
//   3. attach          click-to-attach opens the xterm terminal + shows live output
//   4. keyboard (#1694) j/k navigate the rail, Enter attaches, Escape returns to rail
//   5. create          the + New modal creates a session; its row appears
//   6. kill            the kill confirm removes the session's row
//   7. archive         the archive confirm moves a session to the archived group
//
// The daemon runs with the shipped default require_token=false, so NO peer needs a
// token — the SPA's /v1/auth-info probe reports auth_required=false, the login screen
// is skipped, and every core action (create/kill/archive/restore/retry/attach) runs on
// the empty-token credential. That makes this harness the end-to-end regression guard
// that tokenless authorization works for ALL actions, not just the read path.
//
// The token-PASTE UI is exercised here by intercepting /v1/auth-info to force
// auth_required=true: that both proves the SPA obeys the daemon's answer rather than
// sniffing its own loopback address (so the tokenless path works for a network peer
// too) and gives the failed-login error assertions a screen to render on. The real
// server-side enforcement stays covered by the Go handler tests
// (daemon/httpauth_test.go network-peer → 401 + spoof-resistance, and
// daemon/web_listener_policy_test.go for the config→policy→gate wiring).
//
// Everything the test needs is handed in via env by the entry script (see
// playwright.config.ts): AF_WEB_BASE_URL and the two seeded session titles
// AF_WEB_SESSION_A / AF_WEB_SESSION_B. No token is needed.

import { expect, type Browser, type BrowserContext, type Locator, type Page, test } from "@playwright/test";
import { decode, Op } from "../src/frame.js";

const SESSION_A = process.env.AF_WEB_SESSION_A ?? "probe-a";
const SESSION_B = process.env.AF_WEB_SESSION_B ?? "probe-b";
// The session seeded in a SECOND repo (redesign PR2), used to prove the single-
// project rail scopes to one project and hides the others. It is created BEFORE
// A/B/web, so the most-recently-active default lands on the first repo — A/B are the
// visible rail on load, and SESSION_C is hidden until its project is selected.
const SESSION_C = process.env.AF_WEB_SESSION_C ?? "probe-c";
// The name of the task the harness seeds (web-selftest-entry.sh) so the tasks list
// is non-empty on load.
const SEEDED_TASK = process.env.AF_WEB_TASK_NAME ?? "probe-task";
// The task in the TASK-ONLY project (a third repo with a task but no session,
// redesign PR2): proves a task-only repo lists in the switcher and its tasks scope.
const TASK3_NAME = process.env.AF_WEB_TASK3_NAME ?? "mock3-task";
// The marker the seeded fake agent prints on launch (web-selftest-entry.sh), so
// "the terminal shows live output" is a deterministic string assertion.
const READY_MARKER = process.env.AF_WEB_READY_MARKER ?? "AF_SELFTEST_READY";
// The web-tab session (feat: web/iframe tabs) and its seeded targets: a LOCAL web
// tab named "preview" pointing at a loopback server the daemon proxies, and an
// EXTERNAL web tab named "external" whose host this test intercepts.
const SESSION_WEB = process.env.AF_WEB_SESSION_WEB ?? "probe-web";
// A session the harness already drove through web tab -> archive -> restore (#1809).
const SESSION_WEB_RESTORED = process.env.AF_WEB_SESSION_WEB_RESTORED ?? "probe-restored";
// A session the harness left ARCHIVED holding a preserved web tab (#1809 follow-up).
const SESSION_WEB_SHELVED = process.env.AF_WEB_SESSION_WEB_SHELVED ?? "probe-shelved";
// The malformed/older web tab (kind=web, no target url) the harness seeds directly
// into the daemon's store, because every API path refuses to mint one (#1818). It is
// part of probe-web's REAL roster for the whole run, not a per-test mock.
const NOURL_TAB = process.env.AF_WEBTAB_NOURL_NAME ?? "nourl";
const WEBTAB_LOCAL_MARKER = process.env.AF_WEBTAB_LOCAL_MARKER ?? "AF_WEBTAB_LOCAL_OK";
const VSCODE_MARKER = process.env.AF_VSCODE_MARKER ?? "AF_VSCODE_TAB_OK";
// No vscode session/tab is seeded — the test below creates its tab through the +
// menu, which needs no fixture and covers the real create flow.
const WEBTAB_EXTERNAL_URL = process.env.AF_WEBTAB_EXTERNAL_URL ?? "https://blocked.example.test/";
// The mirror-path session (#1806/#1811): one web tab targeting a SUBDIRECTORY
// document with sibling, parent-relative and absolute-path assets — the shape the
// original single-document fixture had no sub-resources to express.
const SESSION_VITE = process.env.AF_WEB_SESSION_VITE ?? "probe-vite";
const VITE_MARKER = process.env.AF_VITE_MARKER ?? "AF_VITE_OK";
const VITE_ABS_TITLE = process.env.AF_VITE_ABS_TITLE ?? "AF_ABS_ASSET_EXECUTED";
// The #1810 misroute session — tabs [agent, lower, mis, after], where "mis" is the
// vite-shaped server and "lower"/"after" are the plain one. Visited by exactly one
// test, so its close-a-tab assertions are independent of what an earlier test left
// selected. See web-selftest-entry.sh for why that tab layout is load-bearing.
const SESSION_MIS = process.env.AF_WEB_SESSION_MIS ?? "probe-misroute";
// The dead-dev-server session (#1813): one web tab pointing at DEAD_PORT, which
// NOTHING listens on. The daemon proxies it, gets ECONNREFUSED, and answers 502 with
// the {data,error} envelope — the state the pane used to render as raw text.
// NOT "probe-dead": the status-dot tests mock a rail row by that name, and row()
// filters by substring, so a real session called that wedges them on two matches.
const SESSION_DEAD = process.env.AF_WEB_SESSION_DEAD ?? "probe-noserver";
const DEAD_PORT = process.env.AF_WEB_DEAD_PORT ?? "8892";
// The rename/reorder session (#1813): tabs [agent, alpha(web), beta(process),
// gamma(process)] in creation order. The tests below permute and rename these, so
// they run in file order against the state the previous one left.
const SESSION_ORDER = process.env.AF_WEB_SESSION_ORDER ?? "probe-order";

/** A rail row by its session title. */
function row(page: Page, title: string): Locator {
  return page.locator(".af-rail-list .af-row", { hasText: title });
}

/** One of the quiet lifecycle glyphs revealed on the selected rail row (#2186). */
function railAction(page: Page, title: string, name: "Archive session" | "Restore session" | "Kill session"): Locator {
  return row(page, title).getByRole("button", { name: `${name} “${title}”`, exact: true });
}

/** One state's checkbox in the rail's filter menu (feat: hide archived by default). */
function filterItem(page: Page, kind: string): Locator {
  return page.locator(`.af-filter-item[data-kind="${kind}"]`);
}

/**
 * Sets one state's checkbox in the rail filter and closes the menu.
 *
 * Idempotent (it clicks only when the box disagrees), because these flows share one
 * page in serial order and the filter is PERSISTED: a test that assumed a starting
 * state instead of asserting it would pass or fail on whatever the previous test
 * left in localStorage. Every test that changes the filter restores it before it
 * ends, for the same reason.
 */
async function setFilter(page: Page, kind: string, on: boolean): Promise<void> {
  await page.locator(".af-rail-filter").click();
  const item = filterItem(page, kind);
  await expect(item).toBeVisible();
  if ((await item.getAttribute("aria-checked")) !== String(on)) {
    await item.click();
  }
  await expect(item).toHaveAttribute("aria-checked", String(on));
  // Dismiss by clicking outside the control (the rail title is inert chrome).
  await page.locator(".af-rail-title").click();
  await expect(page.locator(".af-filter-menu")).toBeHidden();
}

/** Restores the default filter — every state but archived — via the menu's own reset,
 *  so a test leaves the shared page as it found it. */
async function resetFilter(page: Page): Promise<void> {
  await page.locator(".af-rail-filter").click();
  const reset = page.locator(".af-filter-reset");
  if (await reset.isEnabled()) {
    await reset.click();
  }
  await expect(reset).toBeDisabled();
  await page.locator(".af-rail-title").click();
  await expect(page.locator(".af-filter-menu")).toBeHidden();
}

/** A project switcher menu item by its EXACT repo basename (redesign PR2). Filters on
 *  the name span with an anchored regex so "mock-repo" never also matches
 *  "mock-repo-2" / "mock-repo-3" (they share the prefix). */
function projectItem(page: Page, name: string): Locator {
  return page
    .locator(".af-project-menu .af-project-item")
    .filter({ has: page.locator(".af-project-item-name", { hasText: new RegExp(`^${name}$`) }) });
}

/**
 * Simulates dragging the tab labelled `tabText` from the tab bar onto an `edge` of
 * the (single) current pane. Playwright's mouse-based dragTo doesn't drive HTML5
 * drag-and-drop reliably, so we dispatch the drag events ourselves with a shared
 * DataTransfer — the same object across dragstart/dragover/drop makes getData work,
 * exactly as a real drag would — and aim the drop at the edge band so the pane splits
 * in that direction (see split.ts zoneAt). A center drop uses the middle instead.
 */
async function dragTabToPane(page: Page, tabText: string, edge: "left" | "right" | "top" | "bottom" | "center"): Promise<void> {
  await page.evaluate(
    ({ tabText, edge }) => {
      const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes(tabText));
      const pane = document.querySelector(".af-term-host .af-pane");
      if (!tab || !pane) {
        throw new Error("drag source or target pane not found");
      }
      const dt = new DataTransfer();
      tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));
      const r = pane.getBoundingClientRect();
      let x = r.left + r.width / 2;
      let y = r.top + r.height / 2;
      const m = 6;
      if (edge === "left") {
        x = r.left + m;
      } else if (edge === "right") {
        x = r.right - m;
      } else if (edge === "top") {
        y = r.top + m;
      } else if (edge === "bottom") {
        y = r.bottom - m;
      }
      const init = { bubbles: true, cancelable: true, dataTransfer: dt, clientX: x, clientY: y };
      pane.dispatchEvent(new DragEvent("dragenter", init));
      pane.dispatchEvent(new DragEvent("dragover", init));
      pane.dispatchEvent(new DragEvent("drop", init));
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
    },
    { tabText, edge },
  );
}

/**
 * Reduces the shown session's tab bar to its single unclosable Agent tab, whatever the
 * previous test left behind — and with it the layout, since dropping to one tab
 * validates every pane back down to one.
 *
 * The suite is serial against one daemon, so a test that fails BEFORE its own cleanup
 * hands the next test a stray tab. That next test then either quietly exercises a
 * different path than it claims, or reds out on a precondition — a second failure
 * pointing at the wrong subject, which is exactly what makes a red suite hard to read
 * (#1897). Asserting the state you need is cheaper than inheriting it.
 *
 * The agent tab (index 0) renders no ×, so closing every × converges on it. Bounded by
 * af's 9-tab-per-session ceiling.
 */
async function resetToAgentTab(page: Page): Promise<void> {
  const tabbar = page.locator(".af-tabbar");
  for (let guard = 0; guard < 9; guard++) {
    const closable = tabbar.locator(".af-tab-close");
    if ((await closable.count()) === 0) {
      break;
    }
    const before = await tabbar.locator(".af-tab").count();
    await closable.first().click();
    await expect(tabbar.locator(".af-tab")).toHaveCount(before - 1, { timeout: 30_000 });
  }
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
}

/** Creates the menu's Terminal choice. The labelled New tab control deliberately
 *  makes every mouse-created kind explicit; the `t` keyboard shortcut remains the
 *  one-keystroke shell path. */
async function createTerminalTab(page: Page): Promise<void> {
  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab-new").click();
  const menu = tabbar.locator(".af-tab-menu");
  await expect(menu).toBeVisible();
  await menu.locator(".af-tab-menu-item", { hasText: /^Terminal$/ }).click();
  await expect(menu).toBeHidden();
}

interface ElementBox {
  x: number;
  y: number;
  width: number;
  height: number;
}

/** Waits for an open new-tab menu to settle, then proves it is pixels the user can
 *  actually click — not merely a non-hidden box clipped by an ancestor's overflow.
 *  Two identical geometry samples avoid accepting a transition's in-flight frame. */
async function settledHitTestableTabMenu(
  page: Page,
  menu: Locator,
  trigger: Locator,
): Promise<{ menu: ElementBox; trigger: ElementBox }> {
  let previous = "";
  let settled: { menu: ElementBox; trigger: ElementBox } | null = null;
  await expect
    .poll(
      async () => {
        const menuBox = await menu.boundingBox();
        const triggerBox = await trigger.boundingBox();
        const itemBox = await menu.locator(".af-tab-menu-item").first().boundingBox();
        const viewport = page.viewportSize();
        if (!menuBox || !triggerBox || !itemBox || !viewport) {
          return false;
        }
        const geometry = [menuBox, triggerBox]
          .flatMap((box) => [box.x, box.y, box.width, box.height])
          .map((value) => value.toFixed(2))
          .join(":");
        const stable = geometry === previous;
        previous = geometry;
        const insideViewport =
          menuBox.x >= 0 &&
          menuBox.y >= 0 &&
          menuBox.x + menuBox.width <= viewport.width &&
          menuBox.y + menuBox.height <= viewport.height;
        const hitTestable = await page.evaluate(
          ({ x, y }) => {
            const hit = document.elementFromPoint(x, y);
            return hit instanceof Element && hit.closest(".af-tab-menu-item") !== null;
          },
          { x: itemBox.x + itemBox.width / 2, y: itemBox.y + itemBox.height / 2 },
        );
        if (stable && insideViewport && hitTestable) {
          settled = { menu: menuBox, trigger: triggerBox };
          return true;
        }
        return false;
      },
      { message: "the settled new-tab menu must be inside the viewport and receive pointer hits", timeout: 5_000 },
    )
    .toBe(true);
  if (!settled) {
    throw new Error("new-tab menu geometry did not settle");
  }
  return settled;
}

/**
 * Dispatches a drop carrying a HAND-CRAFTED drag payload ({index, tabs}) onto the
 * (single) current pane — bypassing the real tab buttons so a stale / out-of-range /
 * mid-drag-reorder snapshot can be injected. Used to prove the drop handler validates
 * the payload (range + drag-time tab-set snapshot vs live) and no-ops an invalid one
 * instead of binding a pane to the wrong / a nonexistent tab.
 */
async function dropSnapshotOnPane(page: Page, payload: { index: number; tabs: string[] }): Promise<void> {
  await page.evaluate((payload) => {
    const pane = document.querySelector(".af-term-host .af-pane");
    if (!pane) {
      throw new Error("no pane to drop on");
    }
    const dt = new DataTransfer();
    dt.setData("application/x-af-tab", JSON.stringify(payload));
    const r = pane.getBoundingClientRect();
    const init = {
      bubbles: true,
      cancelable: true,
      dataTransfer: dt,
      clientX: r.right - 6, // aim at the right edge → a split, if it were allowed
      clientY: r.top + r.height / 2,
    };
    pane.dispatchEvent(new DragEvent("dragover", init));
    pane.dispatchEvent(new DragEvent("drop", init));
  }, payload);
}

/**
 * Drags the tab labelled `tabText` onto an `edge` of the pane at `paneIndex`, HIT-TESTING
 * the drop point the way the browser does instead of dispatching straight at the pane.
 *
 * This is what makes it a regression test for the iframe-swallow bug. dragTabToPane above
 * dispatches directly on .af-pane, which bypasses hit-testing entirely — it would pass
 * whether or not a frame covers the pane, so it can't see this bug. Here the drop point is
 * resolved with elementFromPoint (which honours pointer-events, exactly like the browser's
 * own drag hit-testing). Landing on an <iframe> means the real browser delivers the event
 * to the FRAMED document and this document sees no dragover/drop at all, so that case
 * dispatches NOTHING — reproducing the user's "nothing happens". Returns what was hit so a
 * test can assert on the swallow itself.
 *
 * The aim point is the MIDDLE of the edge band, which is where a user actually drags. A few
 * px from the rim (dragTabToPane's `m = 6`) would land in .af-pane-host's ~10px padding
 * gutter — parent DOM that never swallowed anything — and would quietly miss the bug.
 */
async function dragTabOntoPaneHitTested(
  page: Page,
  tabText: string,
  edge: "left" | "right" | "top" | "bottom" | "center",
  paneIndex = 0,
): Promise<{ hitTag: string; swallowed: boolean }> {
  return await page.evaluate(
    ({ tabText, edge, paneIndex, band }) => {
      const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes(tabText));
      const pane = [...document.querySelectorAll(".af-term-host .af-pane")][paneIndex];
      if (!tab || !pane) {
        throw new Error("drag source or target pane not found");
      }
      const dt = new DataTransfer();
      // The real delegated dragstart listener stamps the payload AND sets the body flag
      // the pointer-events rule keys off — so the hit-test below sees true drag state.
      tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));

      const r = pane.getBoundingClientRect();
      let x = r.left + r.width / 2;
      let y = r.top + r.height / 2;
      const inset = band / 2; // the middle of the outer band
      if (edge === "left") {
        x = r.left + r.width * inset;
      } else if (edge === "right") {
        x = r.right - r.width * inset;
      } else if (edge === "top") {
        y = r.top + r.height * inset;
      } else if (edge === "bottom") {
        y = r.bottom - r.height * inset;
      }

      const hitEl = document.elementFromPoint(x, y);
      const swallowed = hitEl instanceof HTMLIFrameElement;
      const hitTag = hitEl ? hitEl.tagName.toLowerCase() : "none";
      if (hitEl && !swallowed) {
        // Dispatch on whatever the pointer really lands on; the pane's handlers catch it
        // as it bubbles, just as a genuine drag would.
        const init = { bubbles: true, cancelable: true, dataTransfer: dt, clientX: x, clientY: y };
        hitEl.dispatchEvent(new DragEvent("dragenter", init));
        hitEl.dispatchEvent(new DragEvent("dragover", init));
        hitEl.dispatchEvent(new DragEvent("drop", init));
      }
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
      return { hitTag, swallowed };
    },
    { tabText, edge, paneIndex, band: 0.3 }, // EDGE_BAND in split.ts
  );
}

/** Points the task modal's schedule picker (#2057) at the "every N minutes" preset.
 *  The task form no longer has a bare cron text box: a schedule TYPE plus its
 *  contextual inputs generate the expression, so a test that wants an
 *  every-N-minutes cron drives the preset that produces it. */
async function setEveryNMinutes(modal: Locator, minutes: number): Promise<void> {
  await modal.locator('select[aria-label="Schedule type"]').selectOption("everyNMinutes");
  await modal.locator('input[aria-label="Interval"]').fill(String(minutes));
}

/** Points the schedule picker at Custom and fills its raw cron field — the advanced
 *  escape hatch, and the only place a literal expression is typed now. */
async function setCustomCron(modal: Locator, expr: string): Promise<void> {
  await modal.locator('select[aria-label="Schedule type"]').selectOption("custom");
  await modal.locator('input[aria-label="Cron expression"]').fill(expr);
}

/** Schedule used by the task CRUD case before it manually fires TriggerTask.
 *  Keep the minute user-visible, but place the next matching hour far beyond this
 *  two-minute suite so the real scheduler cannot race the manual RPC under test. */
function manualTriggerFixtureCron(now: Date): string {
  return `${now.getMinutes()} ${(now.getHours() + 12) % 24} * * *`;
}

/** Minimal matcher for the minute/hour fields the fixture helper emits. */
function cronMatchesMinute(cron: string, when: Date): boolean {
  const [minuteField, hourField] = cron.split(" ");
  const matches = (field: string, value: number): boolean => {
    if (field === "*") return true;
    const every = /^\*\/(\d+)$/.exec(field);
    return every ? value % Number(every[1]) === 0 : Number(field) === value;
  };
  return matches(minuteField, when.getMinutes()) && matches(hourField, when.getHours());
}

/** Opens the app on the loopback daemon and asserts the tokenless auto-connect
 *  (#1696): the SPA learns via /v1/auth-info that this loopback client needs no
 *  token, skips the paste-token login entirely, and renders the authed shell with
 *  NO credential. The absence of the #af-token field is the proof no login was
 *  shown; the rail being populated proves the Snapshot was fetched authorized. */
async function openTokenless(page: Page): Promise<void> {
  await pinRealFixtureProject(page);
  await page.goto("/");
  // The authed shell renders without any login interaction.
  await expect(page.locator(".af-app")).toBeVisible();
  // The paste-token login was never required — its input is absent from the DOM.
  await expect(page.locator("#af-token")).toHaveCount(0);
  await assertRealRailFixture(page);
}

/**
 * A fresh browser tab has no persisted project choice, so the product correctly
 * defaults to the project holding the newest live session. This harness creates
 * sessions in several repos as it runs; its real-rail tests intentionally consume
 * the original mock repo. Pin that contract before the app's first script executes.
 * The sessionStorage marker makes it one-shot, so later project-persistence tests can
 * select another repo and reload without this setup overwriting their choice.
 */
async function pinRealFixtureProject(page: Page): Promise<void> {
  const repo = process.env.AF_MOCK_REPO;
  if (!repo) return;
  await page.addInitScript((fixtureRepo) => {
    if (window.top !== window) return;
    try {
      const marker = "af-selftest-real-project-pinned";
      if (sessionStorage.getItem(marker) === "1") return;
      localStorage.setItem("af-project", fixtureRepo);
      sessionStorage.setItem(marker, "1");
    } catch {
      // Sandboxed/non-origin documents can deny storage; the real app document does
      // not, and assertRealRailFixture will report the selected repo if that changes.
    }
  }, repo);
}

class RealRailFixtureUnavailableError extends Error {}

/**
 * Describes the three surfaces that can disagree when the seeded rail disappears:
 * the browser's selected project/rendered rows, an out-of-page real Snapshot probe,
 * and a fresh real events socket. Page routing does not affect APIRequestContext, so
 * the Snapshot line still reports the daemon's truth when a regression test (or a
 * stale service worker/client projection) gives the SPA an empty response.
 */
async function realRailDiagnostics(page: Page): Promise<string> {
  const selectedRepo = await page.locator(".af-project-switch-name").textContent().catch(() => null);
  const selectedRepoKey = await page.evaluate(() => localStorage.getItem("af-project")).catch(() => null);
  const renderedRows = await page.locator(".af-rail-list .af-row").allTextContents().catch(() => []);
  const liveState = await page.locator(".af-app").getAttribute("data-live").catch(() => null);

  let snapshotLine: string;
  try {
    const url = new URL("/v1/Snapshot", page.url()).toString();
    const response = await page.context().request.post(url, { data: { repo_id: "" } });
    const status = response.status();
    const text = await response.text();
    let body: unknown;
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
    const envelope = body as {
      data?: { instances?: Array<{ title?: unknown; worktree?: { repo_path?: unknown } }> | null };
      error?: unknown;
    };
    const instances = Array.isArray(envelope?.data?.instances) ? envelope.data.instances : [];
    const rows = instances.map(
      (instance) =>
        `title=${JSON.stringify(instance.title ?? null)} repo=${JSON.stringify(instance.worktree?.repo_path ?? null)}`,
    );
    snapshotLine = `status=${status} error=${JSON.stringify(envelope?.error ?? null)} instances=[${rows.join(", ")}]`;
  } catch (err) {
    snapshotLine = `request failed: ${err instanceof Error ? err.message : String(err)}`;
  }

  const eventsLine = await page
    .evaluate(
      (timeoutMs) =>
        new Promise<string>((resolve) => {
          const url = new URL("/v1/events", window.location.href);
          url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
          // Keep the probe distinguishable from the app's subscription. Tests can
          // isolate a stale client projection while this socket still reaches the
          // daemon's real events plane; the daemon ignores the diagnostic marker.
          url.searchParams.set("af_selftest_probe", "1");
          const socket = new WebSocket(url);
          let settled = false;
          const finish = (result: string): void => {
            if (settled) return;
            settled = true;
            clearTimeout(timer);
            socket.close();
            resolve(result);
          };
          const timer = window.setTimeout(() => finish(`timeout after ${timeoutMs}ms`), timeoutMs);
          socket.addEventListener("open", () => finish("open"), { once: true });
          socket.addEventListener("error", () => finish("error"), { once: true });
          socket.addEventListener("close", (event) => finish(`closed code=${event.code} reason=${JSON.stringify(event.reason)}`), {
            once: true,
          });
        }),
      1_500,
    )
    .catch((err) => `probe failed: ${err instanceof Error ? err.message : String(err)}`);

  return [
    `selected repo=${JSON.stringify(selectedRepo)} persisted=${JSON.stringify(selectedRepoKey)}`,
    `rendered rail rows=${JSON.stringify(renderedRows)} ui events=${JSON.stringify(liveState)}`,
    `real Snapshot: ${snapshotLine}`,
    `real events: ${eventsLine}`,
  ].join("\n");
}

/** Fails at the first missing-row boundary instead of blaming the next selector. */
async function assertRealRailFixture(page: Page, expectedTitle = SESSION_A): Promise<void> {
  try {
    // Presence, not CSS visibility: the responsive rail is deliberately
    // visibility:hidden while its mobile drawer is closed.
    await expect(row(page, expectedTitle)).toHaveCount(1, { timeout: 3_000 });
  } catch {
    throw new RealRailFixtureUnavailableError(
      `real rail fixture unavailable: expected ${JSON.stringify(expectedTitle)}\n${await realRailDiagnostics(page)}`,
    );
  }
}

// NO file-wide serial mode, deliberately (#1898).
//
// The flows do share one daemon and one page, and they must run IN ORDER — but
// order is not what serial mode buys. `fullyParallel: false` + `workers: 1`
// (playwright.config.ts) already run every test sequentially, in declaration
// order, in one worker. With `retries: 0`, the ONLY thing a file-wide
// `mode: "serial"` added was this: when any test fails, skip every test after it.
//
// That is a gate that blinds itself. A failure is exactly when the rest of the
// suite most needs to run, and the cost is not hypothetical — a single flake in
// the status-dots test (#1898) skipped 41 tests, and #1895's fixture drift skipped
// every test behind it on the branch that found it. A regression in any of them
// would have been invisible behind an unrelated red.
//
// Ordinary test isolation replaces it. Playwright discards the worker process
// after a failure and starts a new one, so beforeAll re-runs and the next test
// gets a fresh page, a fresh login, and fresh module state — the page-level
// pollution serial mode was protecting against cannot cross a failure. What
// survives a restart is the DAEMON's state, which is why the few tests that hand
// state to each other are grouped into their own small serial describe (see
// "create → kill" below): the blast radius of a failure is that group, not the
// file.
//
// So: a new test belongs at the top level unless it consumes state another test
// produced. If it does, put the pair in their own serial describe — never reach
// for a file-wide one.

let page: Page;
// The title of the session the create flow makes, handed to the kill flow. It is
// module state, so a worker restart resets it — which is exactly why the two flows
// that share it live in one serial describe rather than at the top level.
let createdTitle = "";
let realRailUnavailable = "";
// Keep the tagged details at each declaration (instead of registering through a
// wrapper) so a failure still reports its real source line; beforeEach uses the tag
// to skip only seeded-rail consumers after a worker restart.
const REAL_FIXTURE_TAG = "@real-fixture";
const REAL_FIXTURE = { tag: REAL_FIXTURE_TAG };

test.beforeAll(async ({ browser }) => {
  page = await browser.newPage();
  try {
    await openTokenless(page);
  } catch (err) {
    if (!(err instanceof RealRailFixtureUnavailableError)) throw err;
    // Do not fail beforeAll: Playwright would restart it for every following test.
    // The first tokenless test reports this once; after a mid-suite worker restart,
    // tagged real-fixture tests skip while independent mocked tests keep running.
    realRailUnavailable = err.message;
  }
});

test.beforeEach(({}, testInfo) => {
  if (realRailUnavailable === "" || !testInfo.tags.includes(REAL_FIXTURE_TAG)) return;
  testInfo.skip(true, "the seeded real rail is unavailable; see the initiating #2276 diagnostic failure");
});

test.afterAll(async () => {
  await page.close();
});

test("tokenless loopback (#1696): the SPA auto-connects with no token, no login screen", async () => {
  test.skip(realRailUnavailable !== "", "the dedicated #2276 diagnostic test reports the missing real rail");
  // The authed shell is up (openTokenless asserted it) with NO paste-token step —
  // reload to prove it is not a one-off: a fresh load re-probes /v1/auth-info and
  // again auto-connects with no credential.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator("#af-token")).toHaveCount(0);
  await assertRealRailFixture(page);
  // The events WS connected on the empty-token credential (the ?access_token= is
  // blank and the loopback peer is exempt): the client publishes an open stream.
  await expect(page.locator(".af-app")).toHaveAttribute("data-live", "open");
});

test("#2276: a fresh shell with no seeded real row reports rail-plane diagnostics", async ({ browser }) => {
  if (realRailUnavailable) throw new Error(realRailUnavailable);
  const ctx = await browser.newContext();
  try {
    const p = await ctx.newPage();
    await p.routeWebSocket(
      (url) => url.pathname === "/v1/events" && !url.searchParams.has("af_selftest_probe"),
      () => {
        // Keep the app's socket open but deliberately silent: the browser
        // projection stays on its mocked Snapshot while the marked diagnostic
        // socket below bypasses this route and probes the real events plane.
      },
    );
    await p.route("**/v1/Snapshot", async (route) => {
      const resp = await route.fetch();
      const body = await resp.json();
      const fixturelessBody = body?.data ? { ...body, data: { ...body.data, instances: [] } } : body;
      await route.fulfill({
        status: resp.status(),
        contentType: "application/json",
        body: JSON.stringify(fixturelessBody),
      });
    });

    let failure = "";
    try {
      await openTokenless(p);
    } catch (err) {
      failure = err instanceof Error ? err.message : String(err);
    }

    expect(failure).toContain(`real rail fixture unavailable: expected ${JSON.stringify(SESSION_A)}`);
    expect(failure).toContain("selected repo=");
    expect(failure).toContain("real Snapshot: status=200");
    const snapshotDiagnostic = failure.split("\n").find((line) => line.startsWith("real Snapshot: ")) ?? "";
    expect(snapshotDiagnostic).toContain(JSON.stringify(SESSION_A));
    expect(failure).toContain("real events: open");
  } finally {
    await ctx.close();
  }
});

// The auth-info envelope the SPA probes, as the daemon would send it.
function authInfoBody(required: boolean): string {
  return JSON.stringify({ data: { auth_required: required }, error: null });
}

// The daemon's REAL failure envelope: `error` is an OBJECT ({message}), not a
// string (apiproto.EnvelopeError). This is the shape that used to render as the
// literal text "[object Object]" on every error surface.
function failureBody(message: string): string {
  return JSON.stringify({ data: null, error: { message } });
}

test("the tokenless path follows the daemon's answer, not loopback detection (#1696)", async ({ browser }) => {
  // This browser IS a loopback peer, so a client that decided "no token" by sniffing
  // its own address would auto-connect here. Force the daemon's answer to
  // auth_required=true and the SPA must show the paste-token login anyway — that is
  // the proof the tokenless path keys off /v1/auth-info and nothing else, so it will
  // equally trigger for a NETWORK peer whose daemon reports no token required
  // (the require_token=false default).
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/auth-info", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: authInfoBody(true) }),
  );

  await p.goto("/");
  await expect(p.locator("#af-token")).toBeVisible();
  await expect(p.locator(".af-app")).toHaveCount(0);

  await ctx.close();
});

test("a failed login renders the daemon's real message, never [object Object]", async ({ browser }) => {
  // The regression this pins: the client typed the envelope's `error` as a string
  // while the daemon sends {message}, so `new ApiError(status, env.error)` stringified
  // an object and every failure read "Login failed: [object Object]" — telling the
  // operator nothing. Drive a real failed login and assert the daemon's own words
  // reach the screen.
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/auth-info", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: authInfoBody(true) }),
  );
  // The login probe hits Snapshot; fail it with a structured, non-401 envelope so
  // describeError takes the branch that renders the message (401 has canned text).
  await p.route("**/v1/Snapshot", (route) =>
    route.fulfill({
      status: 400,
      contentType: "application/json",
      body: failureBody("token rejected by policy: expired credential"),
    }),
  );

  await p.goto("/");
  await p.locator("#af-token").fill("some-token");
  await p.locator(".af-login-form button[type=submit]").click();

  const err = p.locator(".af-error");
  await expect(err).toBeVisible();
  await expect(err).toContainText("token rejected by policy: expired credential");
  await expect(err).not.toContainText("[object Object]");
  await expect(err).not.toContainText("undefined");

  await ctx.close();
});

test("an unreachable daemon reports a real transport message, not [object Object]", async ({ browser }) => {
  // The other failure mode Sachin hit: connection refused rather than a bad token.
  // The thrown value is a TypeError, not an envelope — errorText must still yield a
  // readable string.
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/auth-info", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: authInfoBody(true) }),
  );
  await p.route("**/v1/Snapshot", (route) => route.abort("connectionrefused"));

  await p.goto("/");
  await p.locator("#af-token").fill("some-token");
  await p.locator(".af-login-form button[type=submit]").click();

  const err = p.locator(".af-error");
  await expect(err).toBeVisible();
  await expect(err).toContainText("Couldn't reach the daemon");
  await expect(err).not.toContainText("[object Object]");
  await expect(err).not.toContainText("undefined");

  await ctx.close();
});

// --- token persistence (feat: log in once) ---------------------------------
//
// The property: after ONE successful token login, a returning visit — new tab,
// reload, browser restart — resumes the stored credential and lands in the authed
// shell with no prompt, until the daemon rejects it. Before this, the token lived in
// sessionStorage, so every new tab was a fresh paste of `af token show`.
//
// A browser CONTEXT is the unit of persistence here: pages in one context share the
// origin's localStorage exactly as tabs of one browser profile do, and a second
// context is a different profile. So "a new tab in the same context" is the honest
// proxy for a returning visit, and it is precisely what the old per-tab
// sessionStorage failed.
//
// The daemon under test requires no token (require_token=false), so any bearer is
// accepted — /v1/auth-info is intercepted to force the paste-token login into
// existence, and the rejection cases fake the 401 the same way.

/** The stored bearer token as the page sees it, or null when nothing is stored. */
function storedToken(p: Page): Promise<string | null> {
  return p.evaluate(() => localStorage.getItem("af.token"));
}

/** A context whose pages all believe the daemon requires a token. Context-level (not
 *  page-level) routing so a SECOND page in the context inherits it — that second page
 *  is the returning visit under test. */
async function tokenRequiredContext(browser: Browser): Promise<BrowserContext> {
  const ctx = await browser.newContext();
  await ctx.route("**/v1/auth-info", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: authInfoBody(true) }),
  );
  return ctx;
}

/** Pastes a token into the login form and waits for the authed shell. */
async function loginWithToken(p: Page, token: string): Promise<void> {
  await p.goto("/");
  await expect(p.locator("#af-token")).toBeVisible();
  await p.locator("#af-token").fill(token);
  await p.locator(".af-login-form button[type=submit]").click();
  await expect(p.locator(".af-app")).toBeVisible();
}

test("token persistence: a NEW TAB after a login resumes the token and never prompts", async ({ browser }) => {
  const ctx = await tokenRequiredContext(browser);
  const first = await ctx.newPage();
  await loginWithToken(first, "persisted-token");
  expect(await storedToken(first)).toBe("persisted-token");

  // The returning visit: a fresh tab on the same profile, no interaction at all. It
  // must land in the authed shell — the paste field never renders. (On the old
  // sessionStorage build this tab started empty and showed the login form.)
  const second = await ctx.newPage();
  await second.goto("/");
  await expect(second.locator(".af-app")).toBeVisible();
  await expect(second.locator("#af-token")).toHaveCount(0);
  // And it is really authorized, not just past the gate: the events WS connected on
  // the resumed credential.
  await expect(second.locator(".af-app")).toHaveAttribute("data-live", "open");

  // A reload of the ORIGINAL tab resumes it too (the browser-restart case, minus the
  // restart), and the login form still never appears.
  await first.reload();
  await expect(first.locator(".af-app")).toBeVisible();
  await expect(first.locator("#af-token")).toHaveCount(0);

  await ctx.close();
});

test("token persistence: the login screen says the token will be saved", async ({ browser }) => {
  // Persisting a full-access credential silently is the thing not to do: the screen
  // that takes the token says it keeps it, and names the way back out. Kept as its
  // own test so a copy change never masquerades as a persistence failure.
  const ctx = await tokenRequiredContext(browser);
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-login-note")).toContainText("stays saved in this browser until you disconnect");
  await ctx.close();
});

test("token persistence: Disconnect forgets the token, so the next visit prompts", async ({ browser }) => {
  const ctx = await tokenRequiredContext(browser);
  const p = await ctx.newPage();
  await loginWithToken(p, "forget-me");

  // The logout affordance: on a shared machine or after a rotation, this is the way
  // back to the prompt. It must clear the STORE, not just the in-memory credential.
  await p.locator(".af-appbar button", { hasText: "Disconnect" }).click();
  await expect(p.locator("#af-token")).toBeVisible();
  expect(await storedToken(p)).toBeNull();

  // A new tab confirms it: nothing was left behind to resume.
  const after = await ctx.newPage();
  await after.goto("/");
  await expect(after.locator("#af-token")).toBeVisible();
  await expect(after.locator(".af-app")).toHaveCount(0);

  await ctx.close();
});

test("token persistence: a REJECTED stored token clears itself and prompts once — no loop", async ({ browser }) => {
  const ctx = await tokenRequiredContext(browser);
  // The rotation case: the stored token is no longer valid, so the daemon 401s the
  // login probe. Without the clear, every load would retry the dead credential.
  await ctx.route("**/v1/Snapshot", (route) =>
    route.fulfill({ status: 401, contentType: "application/json", body: failureBody("unauthorized") }),
  );
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator("#af-token")).toBeVisible();
  // Seed a stale token the way a previous successful login would have, then return.
  await p.evaluate(() => localStorage.setItem("af.token", "rotated-away"));

  await p.reload();
  // Exactly one outcome: the paste form, with the rejection explained.
  await expect(p.locator("#af-token")).toBeVisible();
  await expect(p.locator(".af-error")).toContainText("That token was rejected");
  await expect(p.locator(".af-app")).toHaveCount(0);
  // Cleared, so the next load prompts straight away rather than re-probing a token
  // that is known bad.
  expect(await storedToken(p)).toBeNull();

  // And it stays put: a further reload shows the same clean prompt, no error carried
  // over from a retry the app should not be making.
  await p.reload();
  await expect(p.locator("#af-token")).toBeVisible();
  await expect(p.locator(".af-error")).toHaveCount(0);

  await ctx.close();
});

test("token persistence: an unreachable daemon KEEPS the stored token", async ({ browser }) => {
  // The distinction that makes persistence usable: a transport failure says nothing
  // about the credential. Clearing on it would mean every daemon restart (or asleep
  // laptop) deleted a good token and demanded a fresh paste — the exact behavior this
  // feature exists to remove.
  const ctx = await tokenRequiredContext(browser);
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator("#af-token")).toBeVisible();
  await p.evaluate(() => localStorage.setItem("af.token", "still-good"));

  await ctx.route("**/v1/Snapshot", (route) => route.abort("connectionrefused"));
  await p.reload();
  await expect(p.locator(".af-error")).toContainText("Couldn't reach the daemon");
  expect(await storedToken(p)).toBe("still-good");

  // Daemon back: the very next load resumes silently, with no paste in between.
  await ctx.unroute("**/v1/Snapshot");
  await p.reload();
  await expect(p.locator(".af-app")).toBeVisible();
  await expect(p.locator("#af-token")).toHaveCount(0);

  await ctx.close();
});

test("token persistence: the tokenless path stores no credential (#1696)", async () => {
  // The shared page is the tokenless loopback client: it connects with the empty-token
  // sentinel, which must never be written out as a stored credential — a stored ""
  // would read back as "this client needs none" and could skip a login that IS
  // required. Nothing on disk; the /v1/auth-info probe decides afresh on every load.
  expect(await storedToken(page)).toBeNull();
});

test("sidebar lists the seeded sessions from the Snapshot/events plane", REAL_FIXTURE, async () => {
  // Both seeded rows are present — proof the rail is driven by the daemon
  // projection, not a static list. The rail is SCOPED to the default project (the
  // first repo, redesign PR2), so A and B (that repo) show while SESSION_C (the
  // second repo) is hidden until its project is selected.
  await expect(row(page, SESSION_A)).toBeVisible();
  await expect(row(page, SESSION_B)).toBeVisible();
  // The other project's session is NOT in the scoped rail.
  await expect(row(page, SESSION_C)).toHaveCount(0);
  // The top-right switcher shows the current (default) project.
  await expect(page.locator(".af-project-switch-name")).toContainText("mock-repo");
  await expect(page.locator(".af-rail-count")).toHaveText(/[2-9]|\d{2,}/);
  // The events WebSocket connected: the client publishes an open stream, proving the
  // push plane the rail resyncs from is up.
  await expect(page.locator(".af-app")).toHaveAttribute("data-live", "open");
});

test("status dots (#1766): waiting shows a green dot, working shows none, error states are static — no spin anywhere", REAL_FIXTURE, async ({
  browser,
}) => {
  // The daemon can't be coerced to Running/Lost/Dead/Limit on demand, so pin the
  // states by rewriting the Snapshot. Every asserted row is SYNTHETIC, and that is
  // the whole point rather than a convenience (#1898).
  //
  // A REAL row cannot be pinned. The Snapshot fixes its liveness exactly once, at
  // load; the events plane then keeps pushing `session.updated` deltas for it, and
  // applyEvent upserts them in place with NO resync (web/src/sessions.ts) — so the
  // daemon's truth lands straight on top of the fixture. The seeded fake agent
  // prints its marker and then `exec cat`s, so the daemon's pane-churn liveness
  // flips LiveRunning→LiveReady a beat after seeding. Whenever that beat fell inside
  // this test, probe-a's pinned "working" row acquired a real green dot and the
  // "working shows none" assertion failed — a flake that, under the old global
  // serial mode, took the whole suite behind it down with it.
  //
  // A synthetic id the daemon has never heard of receives no deltas, so the pinned
  // state is the only state there will ever be. That makes this deterministic by
  // construction rather than by out-waiting a race. It costs nothing in coverage:
  // the dot is a pure function of liveness, and these rows drive that render path
  // through exactly the same code.
  //
  // Its own context, too: routing Snapshot on the shared page meant a reload in,
  // a reload out, and a window where a leaked route could color another test.
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
    const list = snap?.instances ?? [];
    // Clone a REAL record (same worktree/repo_path, so the synthetic rows land in
    // this project's scoped rail) and vary only what the dot reads. Each gets a
    // distinct branch so its rendered branch line never contains another row's name
    // (the row() locator matches row text by substring).
    const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
    const synth = (title: string, liveness: number) => ({
      ...proto,
      id: `synth-${title}`,
      title,
      branch: `synth-${title}`,
      liveness,
      in_flight_op: 0,
    });
    list.push(
      synth("probe-working", 1), // Running → working → no dot
      synth("probe-waiting", 2), // Ready → waiting → green dot
      synth("probe-lost", 3),
      synth("probe-dead", 4),
      synth("probe-limit", 6),
    );
    if (snap) {
      snap.instances = list;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  // Working row: NO status dot at all — the dot span is omitted entirely. The row
  // must be asserted present FIRST: toHaveCount(0) on a row that never rendered
  // would pass for the wrong reason.
  await expect(row(p, "probe-working")).toBeVisible({ timeout: 15_000 });
  await expect(row(p, "probe-working").locator(".af-dot")).toHaveCount(0);
  // Waiting row: the static filled circle, in the ready color bucket, never spinning.
  const readyDot = row(p, "probe-waiting").locator(".af-dot");
  await expect(readyDot).toHaveClass(/af-dot-ready/);
  await expect(readyDot.locator('.af-icon[data-icon="circle"]')).toHaveCount(1);
  await expect(readyDot).not.toHaveClass(/af-dot-spin/);
  // Error/terminal states keep distinct STATIC shapes.
  await expect(row(p, "probe-lost").locator('.af-icon[data-icon="circle-dashed"]')).toHaveCount(1);
  await expect(row(p, "probe-lost").locator(".af-dot")).toHaveClass(/af-dot-lost/);
  await expect(row(p, "probe-dead").locator('.af-icon[data-icon="circle"]')).toHaveCount(1);
  await expect(row(p, "probe-limit").locator('.af-icon[data-icon="diamond"]')).toHaveCount(1);
  // The animation class is gone from every status row, and the removed "working" dot
  // kind never renders anywhere.
  await expect(p.locator(".af-dot-spin")).toHaveCount(0);
  await expect(p.locator(".af-dot-working")).toHaveCount(0);

  // Retry remains the conditional pane-header escape from a usage-limit wall
  // (#1934): selecting the synthetic limit row reveals it, while selecting an
  // ordinary waiting row withdraws it. The rail move must not displace this path.
  await row(p, "probe-limit").click();
  const retry = p.locator(".af-term-head button", { hasText: "Retry" });
  await expect(retry).toBeVisible();
  const selectedActions = row(p, "probe-limit").locator(".af-row-actions");
  const waitingActions = row(p, "probe-waiting").locator(".af-row-actions");
  await expect(selectedActions).toHaveCSS("opacity", "1");
  await expect(waitingActions).toHaveCount(1);
  await expect(waitingActions).toHaveCSS("opacity", "0");

  // #2223: selection, pointer hover, and keyboard focus all reveal the same quiet
  // controls. Every row reserves the slot, so hovering an unselected row cannot move
  // its text under the pointer.
  await row(p, "probe-limit").hover();
  await expect(selectedActions).toHaveCSS("opacity", "1");
  await p.mouse.move(0, 0);
  const geometry = () =>
    row(p, "probe-waiting").evaluate((el) => {
      const main = el.querySelector(".af-row-main")!.getBoundingClientRect();
      const actions = el.querySelector(".af-row-actions")!.getBoundingClientRect();
      return { mainLeft: main.left, mainWidth: main.width, actionsLeft: actions.left, actionsWidth: actions.width };
    });
  const beforeHover = await geometry();
  await row(p, "probe-waiting").hover();
  await expect(waitingActions).toHaveCSS("opacity", "1");
  expect(await geometry(), "hover reveal keeps the reserved row geometry").toEqual(beforeHover);
  await p.mouse.move(0, 0);
  await expect(waitingActions).toHaveCSS("opacity", "0");

  // Put focus on the preceding button in DOM order, then use a real Tab keystroke to
  // enter this row. The opacity-zero action remains tabbable and :focus-within makes
  // it visible as soon as focus arrives.
  const keyboardTarget = railAction(p, "probe-waiting", "Archive session");
  await keyboardTarget.evaluate((target) => {
    const buttons = [...document.querySelectorAll<HTMLButtonElement>('button:not([disabled])')];
    const before = buttons[buttons.indexOf(target as HTMLButtonElement) - 1];
    if (!before) {
      throw new Error("archive action has no preceding tab stop");
    }
    before.focus();
  });
  await p.keyboard.press("Tab");
  await expect(keyboardTarget).toBeFocused();
  await expect(waitingActions).toHaveCSS("opacity", "1");
  await keyboardTarget.evaluate((el) => el.blur());
  await row(p, "probe-waiting").click();
  await expect(retry).toBeHidden();

  await ctx.close();
});

test("#2234: creating and id-less rows expose no lifecycle actions; the shared projection chooses the verb", REAL_FIXTURE, async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
    const list = snap?.instances ?? [];
    const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
    const synth = (title: string, extra: Record<string, unknown>) => ({
      ...proto,
      id: `synth-${title}`,
      title,
      branch: `synth-${title}`,
      liveness: 2,
      in_flight_op: 0,
      lifecycle_action: "archive",
      can_kill: true,
      ...extra,
    });
    list.push(
      synth("probe-actionable", {}),
      synth("probe-restorable", { liveness: 3, lifecycle_action: "restore" }),
      synth("probe-startup-unknown", { startup_state_unknown: true, lifecycle_action: undefined }),
      synth("probe-creating", { in_flight_op: 1, lifecycle_action: undefined, can_kill: undefined }),
      synth("probe-idless", { id: undefined, lifecycle_action: undefined, can_kill: undefined }),
    );
    if (snap) {
      snap.instances = list;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await p.goto("/");
  await expect(row(p, "probe-actionable")).toBeVisible({ timeout: 15_000 });

  // These are visible status rows, but the Go projection withheld a lifecycle
  // action. The web must not manufacture controls from their pixels/state.
  for (const title of ["probe-creating", "probe-idless"]) {
    const inert = row(p, title);
    await expect(inert).toBeVisible();
    await expect(inert).toHaveAttribute("aria-disabled", "true");
    await inert.hover();
    await expect(inert.locator(".af-row-actions")).toHaveCount(0);
    await expect(inert.getByRole("button")).toHaveCount(0);
  }

  // Runtime uncertainty is not a reversible lifecycle action and cannot attach,
  // but its stable retained record remains explicitly removable.
  const uncertain = row(p, "probe-startup-unknown");
  await expect(uncertain).toBeVisible();
  await uncertain.hover();
  await expect(railAction(p, "probe-startup-unknown", "Kill session")).toHaveCount(1);
  await expect(railAction(p, "probe-startup-unknown", "Archive session")).toHaveCount(0);
  await expect(railAction(p, "probe-startup-unknown", "Restore session")).toHaveCount(0);

  // Keyboard navigation must apply the same runtime-entry fence as row clicks.
  // Walk the entire visible rail in both directions: a kill-only retained row may
  // own its explicit Kill button, but j/k must never make it the terminal target.
  const visibleRows = await p.locator(".af-rail .af-row").count();
  let uncertainSelected = false;
  for (const key of ["j", "k"]) {
    for (let i = 0; i <= visibleRows; i += 1) {
      await p.keyboard.press(key);
      uncertainSelected ||= (await uncertain.getAttribute("aria-selected")) === "true";
    }
  }
  expect(uncertainSelected).toBe(false);

  // The same server-owned value selects Archive vs Restore, and every accessible
  // name carries its target now that unselected rows can own controls.
  await expect(railAction(p, "probe-actionable", "Archive session")).toHaveCount(1);
  await expect(railAction(p, "probe-actionable", "Kill session")).toHaveCount(1);
  await expect(railAction(p, "probe-restorable", "Restore session")).toHaveCount(1);
  await expect(railAction(p, "probe-restorable", "Archive session")).toHaveCount(0);

  await ctx.close();
});

// #2458: the two "live" indicators are gone from the rendered UI — the pip and
// "Live"/"Connecting…" label beside the project selector, and the "Live · master"
// meta beside the attached session's title.
//
// Asserted on the REAL shell, attached, because that is the only state in which
// both used to render: a removal test run on an empty pane would pass against the
// unfixed code. The paired data-attribute assertions are the other half — they
// prove the machinery still reports open, so this is a removal of the indicators
// and not of the thing they indicated.
test("#2458: no live indicator by the project selector, no live/branch meta by the title", REAL_FIXTURE, async () => {
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  await expect(page.locator(".af-app")).toHaveAttribute("data-live", "open");

  await expect(page.locator(".af-live"), "the appbar live indicator must be gone").toHaveCount(0);
  await expect(page.locator(".af-live-pip")).toHaveCount(0);
  await expect(page.locator(".af-live-label")).toHaveCount(0);
  await expect(page.locator(".af-term-meta"), "the pane-header live/branch meta must be gone").toHaveCount(0);

  // Text, not just class names: a rebuilt indicator under a different class would
  // slip past the selectors above while looking identical to the user. The appbar
  // and pane header are checked rather than the whole page, since "Live" can
  // legitimately appear in session content or the terminal's own output.
  await expect(page.locator(".af-appbar")).not.toContainText("Live");
  await expect(page.locator(".af-appbar")).not.toContainText("Connecting…");
  await expect(page.locator(".af-term-head")).not.toContainText("Live");

  // The branch went with it: "Live · master" was one unit, and the head now carries
  // the session title alone.
  const head = await page.locator(".af-term-head-main").textContent();
  expect(head?.trim()).toBe(SESSION_A);
});

test("click-to-attach opens the xterm terminal and shows live output", REAL_FIXTURE, async () => {
  await row(page, SESSION_A).click();

  // The main pane switched to the terminal view and mounted a real xterm instance.
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host .xterm")).toBeVisible();

  // Live output: the seeded fake agent printed its ready marker over the WS PTY
  // stream, and xterm rendered it into the pane. This is the flagship assertion —
  // a real binary PTY frame decoded by the TS codec and painted in the browser.
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  // The terminal's own connection resolved to the live stream, and the keyboard
  // followed the attach into terminal mode (#1693/#1694).
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  await expect(page.locator(".af-app.af-kb-terminal")).toBeVisible();

  // Every actionable instance reserves the quiet action slot, but only the selected
  // row shows it at rest. Kill remains muted; af-danger is reserved for confirmation.
  const visibleRows = page.locator(".af-rail-list .af-row");
  await expect(page.locator(".af-row-actions")).toHaveCount(await visibleRows.count());
  await expect(row(page, SESSION_A).locator(".af-row-actions")).toHaveCSS("opacity", "1");
  await page.mouse.move(0, 0);
  await expect(row(page, SESSION_B).locator(".af-row-actions")).toHaveCSS("opacity", "0");
  await row(page, SESSION_A).hover();
  await expect(row(page, SESSION_A).locator(".af-row-actions")).toHaveCSS("opacity", "1");
  await expect(railAction(page, SESSION_A, "Archive session").locator('.af-icon[data-icon="archive"]')).toHaveCount(1);
  await expect(railAction(page, SESSION_A, "Kill session").locator('.af-icon[data-icon="octagon-x"]')).toHaveCount(1);
  await expect(railAction(page, SESSION_A, "Kill session")).not.toHaveClass(/af-danger/);
  await expect(page.locator(".af-term-head button", { hasText: "Prompt" })).toHaveCount(0);
  await expect(page.locator(".af-term-head button", { hasText: "Archive" })).toHaveCount(0);
  await expect(page.locator(".af-term-head button", { hasText: "Kill" })).toHaveCount(0);
});

test("#2337: agent Shift+Enter preserves xterm input effects while shell keeps CR", REAL_FIXTURE, async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  const inputPayloads: number[][] = [];
  let shellCreated = false;

  p.on("websocket", (ws) => {
    if (!ws.url().includes("/v1/sessions/") || !ws.url().includes("/stream")) {
      return;
    }
    ws.on("framesent", ({ payload }) => {
      const raw = typeof payload === "string" ? new TextEncoder().encode(payload) : new Uint8Array(payload);
      const frame = decode(raw);
      if (frame.op === Op.Input) {
        inputPayloads.push(Array.from(frame.data));
      }
    });
  });

  try {
    await openTokenless(p);
    await row(p, SESSION_A).click();
    await expect(p.locator(".af-term-host .xterm")).toBeVisible();
    await expect(p.locator(".af-main")).toHaveAttribute("data-term-status", "open");

    await p.keyboard.press("Shift+Enter");
    await expect.poll(() => inputPayloads.length, { message: "Shift+Enter must emit one OpInput" }).toBe(1);
    expect(inputPayloads[0], "Shift+Enter reaches the PTY as LF / Ctrl+J, never xterm's default CR").toEqual([0x0a]);

    await p.keyboard.press("Enter");
    await expect.poll(() => inputPayloads.length, { message: "plain Enter must emit one more OpInput" }).toBe(2);
    expect(inputPayloads[1], "plain Enter keeps xterm's submitting CR path").toEqual([0x0d]);

    // A direct websocket write can produce the right LF while bypassing xterm's
    // user-input effects. Build real agent scrollback, park at the oldest line,
    // and create a real xterm selection. Shift+Enter must both reach the PTY and
    // return the user to the prompt with no stale selection left behind.
    const host = p.locator(".af-term-host");
    await host.click();
    for (let i = 1; i <= 40; i += 1) {
      await p.keyboard.type(`shift-enter-scroll-${i}`);
      await p.keyboard.press("Enter");
    }
    await expect(host).toContainText("shift-enter-scroll-40", { timeout: 15_000 });
    const viewport = host.locator(".xterm-viewport");
    await host.hover();
    await p.mouse.wheel(0, -5000);
    await expect(host, "the real wheel must park the agent viewport above the prompt").not.toContainText(
      "shift-enter-scroll-40",
    );
    const parked = await viewport.evaluate((el) => el.scrollTop);

    const oldestRow = host.locator(".xterm-rows > div", { hasText: READY_MARKER }).first();
    await expect(oldestRow).toBeVisible();
    const oldestBox = await oldestRow.boundingBox();
    expect(oldestBox, "the visible ready-marker row must have selectable geometry").toBeTruthy();
    const { x, y, width, height } = oldestBox as { x: number; y: number; width: number; height: number };
    await p.mouse.move(x + 4, y + height / 2);
    await p.mouse.down();
    await p.mouse.move(x + Math.min(width - 4, 120), y + height / 2, { steps: 8 });
    await p.mouse.up();
    const selection = host.locator(".xterm-selection > div");
    await expect(selection, "the setup must create an actual xterm selection").not.toHaveCount(0);

    inputPayloads.length = 0;
    await p.keyboard.press("Shift+Enter");
    await expect.poll(() => inputPayloads.length, { message: "the selected agent still receives one LF" }).toBe(1);
    expect(inputPayloads[0]).toEqual([0x0a]);
    await expect(host, "genuine user input must reveal the newest line at the prompt").toContainText(
      "shift-enter-scroll-40",
    );
    await expect
      .poll(async () => viewport.evaluate((el) => el.scrollTop), {
        message: "genuine user input must advance the viewport from its parked scrollback position",
      })
      .toBeGreaterThan(parked);
    await expect(selection, "genuine user input must clear the stale xterm selection").toHaveCount(0);

    // Ctrl+C immediately after the newline is the behavioral discriminator: if
    // the selection survived, the clipboard branch would copy and send no ETX.
    await p.keyboard.press("Control+c");
    await expect.poll(() => inputPayloads.length, { message: "Ctrl+C after Shift+Enter must interrupt" }).toBe(2);
    expect(inputPayloads[1], "the cleared selection leaves Ctrl+C on the interrupt path").toEqual([0x03]);

    // The same terminal component owns non-agent tabs, where raw-mode programs
    // may distinguish CR from LF. Preserve xterm's historical Shift+Enter CR and
    // execute one command with each Enter variant to pin both paths end to end.
    await createTerminalTab(p);
    shellCreated = true;
    await expect(p.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Terminal", { timeout: 30_000 });
    await expect(p.locator(".af-main")).toHaveAttribute("data-term-status", "open");

    inputPayloads.length = 0;
    await p.keyboard.type("echo $((233700 + 42))");
    await p.keyboard.press("Shift+Enter");
    await expect(p.locator(".af-term-host")).toContainText("233742");
    expect(inputPayloads.at(-1), "a shell keeps xterm's historical Shift+Enter CR").toEqual([0x0d]);

    await p.keyboard.type("echo $((233700 + 43))");
    await p.keyboard.press("Enter");
    await expect(p.locator(".af-term-host")).toContainText("233743");
    expect(inputPayloads.at(-1), "plain shell Enter remains CR").toEqual([0x0d]);
  } finally {
    if (shellCreated) {
      await resetToAgentTab(p);
    }
    await ctx.close();
  }
});

test("the #1694 keyboard model: j/k navigate, Enter attaches, Escape returns to rail", REAL_FIXTURE, async () => {
  // We are attached to A (terminal mode from the previous flow). Escape is the one
  // hatch back to the rail — no stray ESC byte leaks to the PTY.
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-app.af-kb-rail")).toBeVisible();
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);

  // j moves the selection off A to the next row; the terminal is NOT stolen — j/k
  // always navigate the rail in nav mode. (Pre-#1693 j/k silently fed the agent.)
  await page.mouse.move(0, 0);
  await page.keyboard.press("j");
  await expect(row(page, SESSION_A)).not.toHaveClass(/af-row-selected/);
  await expect(row(page, SESSION_A).locator(".af-row-actions")).toHaveCSS("opacity", "0");
  const movedTo = page.locator(".af-rail-list .af-row.af-row-selected");
  await expect(movedTo).toHaveCount(1);
  await expect(movedTo.locator(".af-row-actions")).toHaveCSS("opacity", "1");
  await expect(page.locator(".af-row-actions")).toHaveCount(await page.locator(".af-rail-list .af-row").count());
  await expect(page.locator(".af-app.af-kb-rail")).toBeVisible();

  // k moves back up to A — j/k are symmetric rail navigation.
  await page.keyboard.press("k");
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);

  // Enter attaches the selected row and hands the keyboard to its terminal.
  await page.keyboard.press("Enter");
  await expect(page.locator(".af-app.af-kb-terminal")).toBeVisible();

  // Escape detaches back to the rail, completing the round trip.
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-app.af-kb-rail")).toBeVisible();
});

test("the #1694 keyboard model: [ / ] cycle the top-level view (sessions → tasks → config)", REAL_FIXTURE, async () => {
  // Rail mode from the previous flow. [ / ] cycle the top-level view; they fire in
  // rail mode only (a modal or focused terminal would swallow them). After Escape
  // the active element is document.body, so the document-level capture-phase keydown
  // listener (index.ts) handles the press. The Projects view is gone (redesign PR2);
  // the config view joins the cycle with the config editor.
  const active = (view: string) =>
    expect(page.locator(`.af-viewtab[data-view="${view}"]`)).toHaveClass(/af-viewtab-active/);
  await active("sessions");
  // ] advances sessions -> tasks -> config, then wraps config -> sessions.
  await page.keyboard.press("]");
  await active("tasks");
  await page.keyboard.press("]");
  await active("config");
  await page.keyboard.press("]");
  await active("sessions");
  // [ steps the other way, wrapping sessions -> config, and back to the start view so
  // the following rail-driven flows still see the sessions rail.
  await page.keyboard.press("[");
  await active("config");
  await page.keyboard.press("[");
  await active("tasks");
  await page.keyboard.press("[");
  await active("sessions");
  await expect(page.locator(".af-rail-list")).toBeVisible();
});

test("config: the editor renders from the manifest and writes through the real path", REAL_FIXTURE, async () => {
  // The config view is rendered entirely from GetConfig (the config manifest zipped
  // with live values) — the bundle carries no key list of its own. This drives the
  // real daemon against the container's throwaway AF home, so the write goes through
  // config.SetGlobalConfigValue: the same validated, file-locked, atomic path
  // `af config set` uses.
  await page.locator('.af-viewtab[data-view="config"]').click();
  const pane = page.locator(".af-config");
  await expect(pane).toBeVisible();

  // A tier-1 key the manifest always carries, with its purpose line.
  const row = pane.locator('.af-config-row[data-key="default_program"]');
  await expect(row).toBeVisible();
  await expect(row.locator(".af-config-purpose")).not.toBeEmpty();

  // The advanced tier folds until asked for, so the handful of keys that matter
  // are not buried under twenty.
  await expect(pane.locator('.af-config-row[data-key="daemon_poll_interval"]')).toHaveCount(0);
  await pane.locator(".af-config-toggle").click();
  await expect(pane.locator('.af-config-row[data-key="daemon_poll_interval"]')).toBeVisible();

  // An enumerated key renders a picker built from the manifest's own enum.
  const channel = pane.locator('.af-config-row[data-key="update_channel"] select');
  await expect(channel).toBeVisible();
  await channel.selectOption("preview");

  // The echo names what was WRITTEN, and the restart notice appears at the moment
  // of the edit naming the command to run — config.toml is read at startup, so an
  // editor that changed a value the running daemon ignores must say so.
  const echo = pane.locator('.af-config-row[data-key="update_channel"] .af-config-echo');
  await expect(echo).toHaveText(/set update_channel = preview/);
  await expect(pane.locator('.af-config-row[data-key="update_channel"] .af-config-notice')).toContainText(
    "af daemon restart",
  );

  // Enter on an UNTOUCHED field must do nothing. The Save button is disabled
  // there, and Enter has to honor the same gate: a no-op write still echoes and
  // still raises the restart notice, telling the user they changed something and
  // owe a restart when they did neither.
  const branch = pane.locator('.af-config-row[data-key="branch_prefix"]');
  await expect(branch.locator("button.af-config-save")).toBeDisabled();
  await branch.locator("input").press("Enter");
  await expect(branch.locator(".af-config-echo")).toHaveCount(0);

  // A dynamic table is never offered as one editable value: program_overrides is
  // settable only through its LEAVES, so a field here could only dead-end at a
  // save the writer refuses. It names the command that works instead.
  const overrides = pane.locator('.af-config-row[data-key="program_overrides"]');
  await expect(overrides.locator("input, select")).toHaveCount(0);
  await expect(overrides.locator(".af-config-readonly")).toContainText("af config set program_overrides.<name>");

  // A hand-edited key is shown but never offered as a field whose save could only
  // be refused.
  const readOnly = pane.locator('.af-config-row[data-key="theme"] .af-config-readonly');
  await expect(readOnly).toHaveText(/hand-edited/);
  await expect(page.locator('.af-config-row[data-key="theme"] input')).toHaveCount(0);

  // Back to the sessions view for the flows that follow.
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await expect(page.locator(".af-rail-list")).toBeVisible();
});

test("tabs: create a shell tab, switch to it, see its distinct output, close it (#1592 PR7)", REAL_FIXTURE, async () => {
  // Capture the tab-mutation request bodies so we can assert they carry the stable
  // session id (#1592 PR7 fix 1 — the daemon must resolve by id, not the cross-repo
  // ambiguous title), and let one CloseTab be forced to fail (fix 3 — the error
  // surfaces as a visible toast).
  let lastCreateBody: { id?: string } | null = null;
  let lastCloseBody: { id?: string } | null = null;
  let failClose = false;
  await page.route("**/v1/CreateTab", async (route) => {
    lastCreateBody = route.request().postDataJSON();
    await route.continue();
  });
  await page.route("**/v1/CloseTab", async (route) => {
    lastCloseBody = route.request().postDataJSON();
    if (failClose) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        // The daemon's real failure shape: `error` is an object carrying `message`
        // (apiproto.EnvelopeError). This mock used to send a bare string, mirroring
        // the client's old (wrong) type — so the two agreed with each other and
        // disagreed with the daemon, which is how "[object Object]" survived.
        body: JSON.stringify({ data: null, error: { message: "simulated tab-close failure" } }),
      });
      return;
    }
    await route.continue();
  });

  // Attach to A so its tab bar renders. A fresh session has exactly one tab — the
  // unclosable "Agent" tab — and it is active.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1);
  await expect(tabbar.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");

  // Create a $SHELL tab via New tab → Terminal (the mouse twin of the web `t`). The tab bar grows
  // to two tabs, the new "Terminal" tab appears AND becomes active (createSessionTab
  // attaches it), and the terminal re-points its WS stream to that tab.
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  const shellTab = tabbar.locator(".af-tab", { hasText: "Terminal" });
  await expect(shellTab).toHaveClass(/af-tab-active/);

  // Fix 1: the CreateTab request carried the stable session id, not just the title.
  expect(lastCreateBody?.id, "CreateTab must send the stable session id").toBeTruthy();

  // Distinct output: the shell tab is a FRESH PTY, not the agent's — its pane does
  // not carry the agent's ready marker (the terminal was rebuilt for the new tab).
  await expect(page.locator(".af-term-host")).not.toContainText(READY_MARKER);
  // Wait for the shell tab's stream to be live before typing — keystrokes sent
  // before the WS opens are dropped by the terminal's send() guard.
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  // The + attached the shell tab, so keys reach it: type a marker and see it come
  // back over the new tab's stream — proof the switch re-pointed to a live PTY.
  await page.keyboard.type("echo AF_TAB_OUTPUT_OK");
  await page.keyboard.press("Enter");
  await expect(page.locator(".af-term-host")).toContainText("AF_TAB_OUTPUT_OK", { timeout: 15_000 });

  // 1-9 switch tabs in rail nav mode: Escape back to the rail, then 1 selects the
  // agent tab and 2 the shell tab — the active highlight follows the digit.
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-app.af-kb-rail")).toBeVisible();
  await page.keyboard.press("1");
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await page.keyboard.press("2");
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Terminal");

  // Fix 3: a failed close surfaces a visible toast (tab ops have no modal). Force
  // the next CloseTab to error, click ×, and assert the toast — the tab stays.
  failClose = true;
  await shellTab.locator(".af-tab-close").click();
  await expect(page.locator(".af-toast.af-toast-show")).toContainText("simulated tab-close failure");
  // A failed close leaves the tab in place.
  await expect(tabbar.locator(".af-tab")).toHaveCount(2);
  // The CloseTab request was also id-scoped, at the same session as the create.
  expect(lastCloseBody?.id, "CloseTab must send the stable session id").toBeTruthy();
  expect(lastCloseBody?.id).toBe(lastCreateBody?.id);

  // Now let the close succeed via its × (mirrors `w`): the bar shrinks back to the
  // unclosable agent tab, which becomes active again AND the terminal re-points to
  // it (fix 2 — the visible tab and the streamed tab stay in sync as the list
  // shrinks; the agent pane's ready marker is back on screen).
  failClose = false;
  await shellTab.locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  await page.unroute("**/v1/CreateTab");
  await page.unroute("**/v1/CloseTab");
});

test("split panes (feat): drag a tab to a pane edge splits into two live panes; close collapses back", REAL_FIXTURE, async () => {
  // Attach to A and give it a second tab, so there is a distinct tab to drag into a
  // split (dragging the only tab onto itself just moves it — no split).
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  const tabbar = page.locator(".af-tabbar");
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");

  // A single pane so far — today's zero-config default.
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drag the Agent tab (index 0) onto the RIGHT edge → the pane splits into two, the
  // new right pane bound to the agent tab with its OWN live WS stream.
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  // Two concurrent xterm instances now render side by side, each with per-pane chrome.
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(2);
  await expect(page.locator(".af-term-host .af-pane.af-pane-multi")).toHaveCount(2);

  // BOTH panes show live output. The new agent pane streams the ready marker over its
  // own stream — identify it by that marker.
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
  const agentPane = page.locator(".af-term-host .af-pane", { hasText: READY_MARKER });
  const shellPane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await expect(agentPane).toHaveCount(1);
  await expect(shellPane).toHaveCount(1);
  // The other pane (the shell tab) is an independent live PTY — focus it and type, and
  // its echo comes back over its own stream, proving both panes are live at once.
  await shellPane.locator(".af-pane-host").click();
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  await page.keyboard.type("echo AF_SPLIT_OK");
  await page.keyboard.press("Enter");
  await expect(shellPane).toContainText("AF_SPLIT_OK", { timeout: 15_000 });

  // Close the agent pane via its × — the split collapses and the shell pane fills the
  // whole area (one pane again), without closing the underlying tab.
  await agentPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  // The tab list is unchanged — only the pane closed, not the underlying tab.
  await expect(tabbar.locator(".af-tab")).toHaveCount(2);

  // Restore A to a single tab for the later create/kill/archive flows.
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (feat): a FRESHLY-CREATED tab is a drag source too — drag the new tab splits (#1737 follow-up)", REAL_FIXTURE, async () => {
  // The regression: only tabs present at first render were drag sources; a tab created
  // AFTER load (a new terminal tab) could not be dragged into a split. Create a new
  // terminal tab, then drag THAT tab (not an initial one) onto a pane edge and prove it
  // splits into two live panes — the drag wiring must cover tabs created after render.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  const tabbar = page.locator(".af-tabbar");
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  const shellTab = tabbar.locator(".af-tab", { hasText: "Terminal" });
  await expect(shellTab).toHaveClass(/af-tab-active/);

  // The freshly-created tab is a real HTML5 drag source: draggable is set on it just
  // like an initial tab. A real mouse drag needs this attribute (a synthetic dispatch
  // would fire regardless), so assert it directly — a missing draggable is exactly the
  // "the new tab isn't a drag source" framing of the bug.
  await expect(shellTab).toHaveJSProperty("draggable", true);

  // Show the AGENT tab in the single pane, so dragging the new Terminal tab produces a
  // split with two DISTINCT tabs (agent + the freshly-created shell), not a self-split.
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drag the NEWLY-CREATED Terminal tab (index 1) onto the RIGHT edge → the pane splits,
  // the new right pane bound to the shell tab with its OWN live WS stream.
  await dragTabToPane(page, "Terminal", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(2);
  await expect(page.locator(".af-term-host .af-pane.af-pane-multi")).toHaveCount(2);

  // BOTH panes are live: the agent pane still streams the ready marker, and the other
  // pane is the shell tab the freshly-created drag source bound. Type into the shell
  // pane and its echo returns over its own stream — proving the split from the NEW tab
  // is a real, independent PTY, not a dead pane.
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
  const agentPane = page.locator(".af-term-host .af-pane", { hasText: READY_MARKER });
  const shellPane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await expect(agentPane).toHaveCount(1);
  await expect(shellPane).toHaveCount(1);
  await shellPane.locator(".af-pane-host").click();
  await page.keyboard.type("echo AF_NEWTAB_DRAG_OK");
  await page.keyboard.press("Enter");
  await expect(shellPane).toContainText("AF_NEWTAB_DRAG_OK", { timeout: 15_000 });

  // Collapse the split and restore A to a single tab for the later flows.
  await shellPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (feat): a bar rebuild that replaces a drag's source ends the drag cleanly — no stuck state (#1737 Greptile)", REAL_FIXTURE, async () => {
  // If the source tab button is REPLACED mid-drag (a concurrent tab change rebuilds the
  // bar), no dragend can fire on the now-detached source — the global "dragging" state
  // would otherwise stick, leaving the pane hints + drop overlay on screen forever. The
  // bar rebuild must reconcile-clear that state.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });

  // Begin a real drag on the Terminal tab and drive a dragover so a pane shows its drop
  // overlay — the exact on-screen state a drop/dragend would normally clear. The single
  // shared DataTransfer carries the tab MIME, so split.ts recognises the drag.
  await page.evaluate(() => {
    const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes("Terminal"));
    const pane = document.querySelector(".af-term-host .af-pane");
    if (!tab || !pane) {
      throw new Error("drag source or pane not found");
    }
    const dt = new DataTransfer();
    tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));
    const r = pane.getBoundingClientRect();
    const init = { bubbles: true, cancelable: true, dataTransfer: dt, clientX: r.right - 6, clientY: r.top + r.height / 2 };
    pane.dispatchEvent(new DragEvent("dragenter", init));
    pane.dispatchEvent(new DragEvent("dragover", init));
  });
  // The drag is visibly in progress: body flag set, a drop overlay shown.
  await expect(page.locator("body.af-dragging-tab")).toHaveCount(1);
  await expect(page.locator(".af-term-host .af-drop-overlay.af-drop-show")).toBeVisible();

  // Force a bar rebuild that REPLACES the drag's source button, with NO dragend on it —
  // add another tab (a concurrent tab change would do the same).
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(3, { timeout: 30_000 });

  // The drag ended cleanly: no stuck flag, and the overlay is no longer shown (its
  // visibility is gated on the now-cleared flag).
  await expect(page.locator("body.af-dragging-tab")).toHaveCount(0);
  await expect(page.locator(".af-term-host .af-drop-overlay.af-drop-show")).not.toBeVisible();

  // Restore A to a single tab for the later flows.
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).first().locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).first().locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (feat): an out-of-range dropped tab is ignored — no broken pane", REAL_FIXTURE, async () => {
  // Attach to A (a single agent tab, so tab index 1+ does not exist).
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drop an out-of-range tab index (99) on the pane's edge. The drop handler validates
  // it against the live tab count and no-ops it — no split is created, so no pane can
  // bind to a nonexistent tab and break its stream.
  await dropSnapshotOnPane(page, { index: 99, tabs: ["0:x"] });
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);
  // The one pane still shows the live agent output — it was never disturbed.
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);
});

test("split panes (feat): a mid-drag tab-set change cancels the drop — no misbinding (#1738 repro)", REAL_FIXTURE, async () => {
  // Attach to A and give it a second tab, so a drop index of 1 is IN RANGE (2 tabs).
  // This is the T-Rex reproduction: the index is valid, but the tab set changed since
  // the drag began, so binding by index alone would attach the new pane to the WRONG
  // live tab.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drop an IN-RANGE index (1) whose drag-time snapshot (2 entries — count matches the
  // live 2 tabs, so neither the range nor a count check would catch it) does NOT match
  // the live tab identities. Only the snapshot-vs-live comparison can reject this, and
  // it must: the layout stays a single pane, no split bound to the wrong tab.
  await dropSnapshotOnPane(page, { index: 1, tabs: ["0:stale-agent", "1:stale-shell"] });
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);

  // A well-formed drag with the LIVE snapshot still splits (the happy path is intact) —
  // proven here to show the cancel above was the stale check, not a broken drop path.
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });

  // Clean up: collapse back to one pane and restore A to a single tab.
  await page.locator(".af-term-host .af-pane", { hasText: READY_MARKER }).locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (#1901): dragging the ACTIVE tab splits and opens a DIFFERENT tab beside it", REAL_FIXTURE, async () => {
  // The regression: the tab a pane already shows could not be dragged into a split. The
  // drop bound that tab to BOTH halves, so the one-tab-one-pane dedupe closed the
  // original and the split collapsed back — the drag read as a dead gesture. The new
  // half must open one of the session's OTHER tabs instead, so the user gets two
  // different tabs side by side, never A|A.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  const tabbar = page.locator(".af-tabbar");
  await resetToAgentTab(page);
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  // Creating a tab switches to it: the pane now shows Terminal, and Terminal is the tab
  // the bar marks active. That is the gesture's precondition — the dragged tab and the
  // target pane's tab are THE SAME.
  await expect(tabbar.locator(".af-tab.af-tab-active")).toContainText("Terminal");
  const paneTabId = async (nth: number) =>
    await page.locator(".af-term-host .af-pane").nth(nth).getAttribute("data-tab-id");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  const beforeId = await paneTabId(0);

  // Drag the ACTIVE tab onto the pane's own right edge.
  await dragTabToPane(page, "Terminal", "right");

  // It splits — the assertion that fails outright against the bug (one pane, unchanged).
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(2);

  // The two halves are bound to DIFFERENT tabs — the point of the fix. Read the stable
  // tab id each pane binds, not its label: the label is an ordinal and would still read
  // as two different panes if both were on one tab.
  const leftId = await paneTabId(0);
  const rightId = await paneTabId(1);
  expect(leftId, "the left pane binds a real tab id").toBeTruthy();
  expect(rightId, "the right pane binds a real tab id").toBeTruthy();
  expect(rightId, "the two halves must bind DIFFERENT tabs — never A|A").not.toBe(leftId);
  // The dragged tab stayed put and the new half (on the dragged edge) took the other
  // tab — here the Agent tab, the one focus was last on.
  expect(leftId, "the dragged tab keeps its original half").toBe(beforeId);

  // Both halves are LIVE, each on its own stream. The Agent tab streams the ready
  // marker; the Terminal half echoes what we type into its own PTY.
  const agentPane = page.locator(".af-term-host .af-pane", { hasText: READY_MARKER });
  await expect(agentPane).toHaveCount(1, { timeout: 15_000 });
  await expect(agentPane, "the new half is the Agent tab").toHaveAttribute("data-tab-id", rightId ?? "");
  const termPane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await termPane.locator(".af-pane-host").click();
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  await page.keyboard.type("echo AF_SELFSPLIT_OK");
  await page.keyboard.press("Enter");
  await expect(termPane).toContainText("AF_SELFSPLIT_OK", { timeout: 15_000 });

  // Restore A to one pane and one tab for the flows that follow.
  await agentPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (#1901): dragging the only tab of a ONE-tab session is an inert no-op", REAL_FIXTURE, async () => {
  // The documented degradation: with no other tab to put in the new half, the drag does
  // NOTHING — it does not split, and it does not duplicate the tab into two panes. A
  // duplicate would be the very A|A the dedupe exists to prevent, and it would collapse
  // on the next reconcile anyway. Nothing here may throw.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  // Assert the one-tab precondition rather than inherit it — it IS the subject here.
  await resetToAgentTab(page);
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);
  const before = await page.locator(".af-term-host .af-pane").getAttribute("data-tab-id");

  await dragTabToPane(page, "Agent", "right");

  // Still exactly one pane, still on the same tab, still live.
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);
  await expect(page.locator(".af-term-host .af-pane")).toHaveAttribute("data-tab-id", before ?? "");
  // The pane is untouched, not merely present: its stream still carries the session.
  await page.locator(".af-term-host .af-pane-host").click();
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
});

test("web tab (feat): a local dev-server preview is daemon-proxied and rendered in an iframe", REAL_FIXTURE, async () => {
  // The web-tab session was seeded (web-selftest-entry.sh) with a LOCAL web tab
  // "preview" pointing at a loopback HTTP server. Attaching shows the agent tab
  // plus the two web tabs; the tab bar renders web tabs by name.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  const previewTab = tabbar.locator(".af-tab", { hasText: "preview" });
  await expect(previewTab).toHaveCount(1);

  // Switch to the local web tab: the pane mounts an IFRAME (not an xterm) whose src
  // is the SAME-ORIGIN daemon proxy path, so a remote viewer's browser hits the
  // daemon (which reaches the loopback dev server) rather than its own machine.
  await previewTab.click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\//);
  // The pane is an iframe, not a terminal.
  await expect(page.locator(".af-term-host .af-pane-host .xterm")).toHaveCount(0);
  // Every web tab has a reload control for dev-preview refreshes.
  await expect(page.locator(".af-webpane-reload")).toHaveCount(1);

  // The daemon actually reverse-proxied the loopback server: the framed document
  // shows the marker the server served (proof the proxy relayed real content).
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });

  // The daemon credential is allowed in an iframe src only as a one-hop bootstrap
  // transport. Drive that path with an inert fixture value, observe the redirect,
  // and then ask the framed document what URL arbitrary preview JavaScript sees.
  // The app must render only after the private parameter has disappeared.
  const bootstrapSeen = page.waitForResponse(
    (response) => response.url().includes("af_webtab_token=browser-fixture") && response.status() === 307,
  );
  await frame.evaluate((node) => {
    const iframe = node as HTMLIFrameElement;
    const bootstrap = new URL(iframe.src);
    bootstrap.searchParams.set("af_webtab_token", "browser-fixture");
    bootstrap.searchParams.set("_af_bootstrap_probe", "clean-hop");
    iframe.src = `${bootstrap.pathname}${bootstrap.search}`;
  });
  await bootstrapSeen;
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });
  await expect
    .poll(() => page.frameLocator(".af-webframe").locator("html").evaluate(() => window.location.href))
    .toContain("_af_bootstrap_probe=clean-hop");
  const renderedAddress = await page
    .frameLocator(".af-webframe")
    .locator("html")
    .evaluate(() => ({ href: window.location.href, referrer: document.referrer }));
  expect(new URL(renderedAddress.href).searchParams.get("_af_bootstrap_probe")).toBe("clean-hop");
  expect(new URL(renderedAddress.href).searchParams.has("af_webtab_token")).toBe(false);
  expect(renderedAddress.href).not.toContain("browser-fixture");
  expect(renderedAddress.referrer).not.toContain("browser-fixture");
});

test("web tab (#1806/#1811): a Vite-shaped subdirectory app previews, and its absolute-path asset 404s instead of getting the SPA shell", REAL_FIXTURE, async () => {
  // The fixture is the shape the old single-document fixture structurally could not
  // fail on: a document under /app/ referencing a SIBLING (x.css), a PARENT-relative
  // (../shared.css) and an ABSOLUTE (/assets/app.js) asset.
  await row(page, SESSION_VITE).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab", { hasText: "vite" }).click();

  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  // The src MIRRORS the target's path and is keyed by the tab's stable id — not an
  // ordinal (#1810), not a bare prefix that drops the target's path (#1806).
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\/[^/]+\/[^/]+\/app\/viewer\.html/);

  const framed = page.frameLocator(".af-webframe");
  // 1. The SUBDIRECTORY target document itself loads. This is what #1806 documented
  //    as a known limit; the mirror model removes it.
  await expect(framed.locator("#marker")).toHaveText(VITE_MARKER, { timeout: 15_000 });
  // 2. A SIBLING asset resolves beside the document (/app/x.css upstream).
  await expect(framed.locator("#sib")).toHaveCSS("color", "rgb(1, 2, 3)", { timeout: 15_000 });
  // 3. A PARENT-relative asset resolves INSIDE the prefix (/shared.css upstream).
  //    This is the one only a depth-mirroring URL can serve: with a flat prefix,
  //    "../" climbs out of the tab entirely.
  await expect(framed.locator("#par")).toHaveCSS("color", "rgb(4, 5, 6)", { timeout: 15_000 });

  // 4. The ABSOLUTE-path asset escaped the prefix and hit the daemon ROOT. The page
  //    above really did request it (the framed document carries the <script>), but an
  //    opaque-origin subframe's escaped request is not reported to page.on("response")
  //    — so ask the daemon for that exact URL directly instead, which is the contract
  //    that matters: it must fail HONESTLY rather than silently, with a 404 and never
  //    "200 text/html" handing af's own SPA shell to a <script>.
  const abs = await page.request.get("/assets/app.js");
  expect(abs.status(), "/assets/app.js must 404, not be answered with the SPA shell").toBe(404);
  expect(abs.headers()["content-type"] ?? "", "/assets/app.js must not be answered as HTML").not.toContain(
    "text/html",
  );
  expect(await abs.text(), "the af SPA's own HTML was served as the app's JavaScript").not.toContain(
    'id="app"',
  );

  // 5. And nothing executed: the framed document's title is untouched, so the shell
  //    was never run as the app's script.
  const viteFrame = page.frames().find((f) => f.url().includes("/app/viewer.html"));
  expect(viteFrame, "the previewed frame should be attached").toBeTruthy();
  expect(await viteFrame!.title()).not.toContain(VITE_ABS_TITLE);
});

test("web tab (#1810): closing a LOWER tab leaves an open preview on its OWN dev server", REAL_FIXTURE, async () => {
  // The #1810 misroute, end to end. This session's tabs are [agent, lower, mis,
  // after]; a pane opens on "mis" (index 2, the vite-shaped server). Closing "lower"
  // shifts mis 2->1 and after 3->2, so an ORDINAL-keyed iframe would start serving
  // "after" — the OTHER dev server, at HTTP 200, with no error and no reload. The
  // stable-id route makes that impossible.
  await row(page, SESSION_MIS).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar.locator(".af-tab", { hasText: "lower" })).toHaveCount(1, { timeout: 15_000 });

  await tabbar.locator(".af-tab", { hasText: "mis" }).click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  const framed = page.frameLocator(".af-webframe");
  await expect(framed.locator("#marker")).toHaveText(VITE_MARKER, { timeout: 15_000 });

  // Stamp the live iframe: the expando survives only if this exact element stays
  // mounted. Id-keying means a mere ordinal shift no longer forces a remount, so the
  // dev server's in-page state is kept too.
  await page.evaluate(() => {
    const f = document.querySelector(".af-term-host .af-pane-host iframe.af-webframe") as
      | (HTMLIFrameElement & { __afStamp?: string })
      | null;
    if (f) {
      f.__afStamp = "af-not-remounted";
    }
  });

  // The developer closes an UNRELATED, LOWER tab.
  await tabbar.locator(".af-tab", { hasText: "lower" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab", { hasText: "lower" })).toHaveCount(0, { timeout: 30_000 });
  // The shift really happened — without this the assertion below proves nothing.
  await expect(tabbar.locator(".af-tab")).toHaveCount(3, { timeout: 30_000 });

  // THE ASSERTION: the pane still shows mis's OWN dev server. Under the ordinal route
  // this read WEBTAB_LOCAL_MARKER — a different app, silently.
  await expect(framed.locator("#marker")).toHaveText(VITE_MARKER, { timeout: 15_000 });
  await expect(framed.locator("#marker")).not.toHaveText(WEBTAB_LOCAL_MARKER);
  // Still the same frame element: followed, not torn down.
  const stamp = await page.evaluate(() => {
    const f = document.querySelector(".af-term-host .af-pane-host iframe.af-webframe") as
      | (HTMLIFrameElement & { __afStamp?: string })
      | null;
    return f?.__afStamp ?? null;
  });
  expect(stamp).toBe("af-not-remounted");
});

test("web tab (#1809): a web tab survives archive -> restore and still renders through the proxy", REAL_FIXTURE, async () => {
  // The harness already drove the issue's CLI repro end to end against the real
  // daemon: this session was given a web tab ("webpreview" -> the loopback server)
  // plus a process tab ("watcher"), then archived and restored. Archive used to
  // truncate the tab roster to the agent tab, erasing the URL with no way to get it
  // back. Assert the restored session is whole and its web tab is LIVE, not just a
  // surviving row.
  await row(page, SESSION_WEB_RESTORED).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();

  const restoredTab = tabbar.locator(".af-tab", { hasText: "webpreview" });
  await expect(restoredTab).toHaveCount(1);
  // The process tab does NOT come back: its tmux was torn down at archive time
  // (#1028). The fix is kind-aware, not a blanket "keep every tab".
  await expect(tabbar.locator(".af-tab", { hasText: "watcher" })).toHaveCount(0);

  // The restored web tab renders through the SAME-ORIGIN daemon proxy...
  await restoredTab.click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\//);

  // ...and the proxy relays real content from the still-live target, so the URL
  // survived the round trip intact rather than coming back as an empty shell.
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });
});

test("web tab (#1809 follow-up): an ARCHIVED session's preserved web tab is inert — placeholder, no proxy, no ×", REAL_FIXTURE, async () => {
  // The harness left this session archived holding a preserved web tab (and already
  // proved the CLI refuses to delete it). Preserving the URL must not make an
  // archived session live again: the target is a loopback address whose dev server
  // is gone and whose port may now host something else.
  //
  // Archived rows are hidden by default (feat: hide archived by default), so reveal
  // them to reach this one — and put the filter back before leaving.
  await setFilter(page, "archived", true);
  await row(page, SESSION_WEB_SHELVED).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  const shelvedTab = tabbar.locator(".af-tab", { hasText: "shelvedweb" });
  await expect(shelvedTab).toHaveCount(1, { timeout: 15_000 });

  // The × is withdrawn: clicking it would strip the very URL the archive preserved,
  // and the daemon would refuse anyway — so the affordance must not be offered.
  await expect(shelvedTab.locator(".af-tab-close")).toHaveCount(0);
  // Creation does not vanish: the bar names the restore step instead of looking
  // indistinguishable from a product with no tab-create feature (#2077).
  await expect(tabbar.locator(".af-tab-new")).toHaveCount(0);
  await expect(tabbar.locator(".af-tab-new-unavailable")).toHaveText("Restore this session to create tabs");

  await shelvedTab.click();

  // The pane shows the archived placeholder instead of a frame...
  const archived = page.locator(".af-webpane-fallback.af-webpane-archived");
  await expect(archived).toBeVisible({ timeout: 15_000 });
  await expect(archived).toContainText("archived");
  // ...the iframe is hidden and never had a src assigned at all, so no proxy request
  // is issued — this is the assertion that would fail if the pane merely overlaid a
  // loaded frame with the placeholder.
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toBeHidden();
  expect(await frame.getAttribute("src")).toBeNull();
  // ...and the live target's content is nowhere on the page.
  await expect(page.locator(".af-term-host")).not.toContainText(WEBTAB_LOCAL_MARKER);
  // The "open ↗" escape hatch is withdrawn too: it would only hit the refusing proxy.
  await expect(page.locator(".af-webpane-open:visible")).toHaveCount(0);
  // ...as is ↻: an archived session is inert by design, so the frame is never
  // pointed anywhere and there is nothing to refetch. Restoring the session is the
  // real next step, which is what the placeholder above says.
  await expect(
    page.locator(".af-term-host .af-webpane-reload:visible"),
    "an archived pane must not offer a ↻ that cannot reload",
  ).toHaveCount(0);

  await resetFilter(page);
});

test("web tab (item A / Sachin): an external tab shows af's fallback + a working open link, NEVER the browser's raw refusal", REAL_FIXTURE, async () => {
  // The bug: an external site that sends X-Frame-Options renders the BROWSER'S OWN
  // "refused to connect" page inside the iframe — and that block page FIRES the iframe
  // `load` event, so the old onLoad hid the fallback and revealed the raw refusal. The
  // frame's opaque origin is unreadable, so a working embed and a block page are
  // indistinguishable, and only the refusal is forbidden. So af no longer frames an
  // external target at all: the designed fallback is the whole surface.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const externalTab = tabbar.locator(".af-tab", { hasText: "external" });
  await expect(externalTab).toHaveCount(1);
  await externalTab.click();

  // af's calm fallback is the persistent, primary state — shown at once, not after a
  // timeout, and not gated on any load.
  const fallback = page.locator(".af-term-host .af-webpane-fallback.af-webpane-external");
  await expect(fallback).toBeVisible({ timeout: 15_000 });
  await expect(fallback).toContainText("This site may block embedding.");
  // ...with a WORKING open link at the external URL (the fallback's own, and the bar's).
  await expect(fallback.locator("a.af-webpane-fallback-link")).toHaveAttribute("href", WEBTAB_EXTERNAL_URL);
  await expect(page.locator("a.af-webpane-open")).toHaveAttribute("href", WEBTAB_EXTERNAL_URL);
  await expect(page.locator("a.af-webpane-open")).toBeVisible();

  // ...and NO ↻, because this pane has nothing it could reload. The frame is never
  // navigated here by design, so pressing it re-ran load() straight back onto this
  // same card: no fetch, no navigation, no change, no explanation — a control that is
  // present and does nothing, which reads as af being broken and offers no next step.
  // The working escape is the open link asserted just above; that is the whole point
  // of withdrawing this one rather than leaving it to be discovered as inert.
  await expect(
    page.locator(".af-term-host .af-webpane-reload:visible"),
    "an external pane must not offer a ↻ that cannot reload",
  ).toHaveCount(0);

  // The frame is never pointed at the target — so the browser can never render a
  // refusal page in it. This is the structural guarantee: no src, no navigation, no
  // raw refusal possible, no request made to the external host at all.
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toBeHidden();
  expect(await frame.getAttribute("src"), "an external frame must never be navigated").toBeNull();

  // The regression itself: a block page fires `load`. Dispatch one and prove it can no
  // longer reveal the frame — there is no onLoad left to un-hide the fallback.
  await frame.dispatchEvent("load");
  await expect(fallback, "a synthetic load must not reveal a bare frame").toBeVisible();
  await expect(frame).toBeHidden();
  expect(await frame.getAttribute("src")).toBeNull();
});

test("web tab (feat): a tab with no target URL renders a clean fallback, not a blank pane", REAL_FIXTURE, async () => {
  // The URL-less web tab is a malformed/older record that no API can mint — three
  // guards refuse an empty target — so the harness writes it the way an older version
  // would have, straight into the daemon's own store before it boots
  // (web-selftest-entry.sh). It is therefore REAL here: the daemon serves it on the
  // Snapshot AND on every session.updated, so nothing can contradict it mid-test.
  //
  // This used to rewrite the Snapshot in the browser to inject the tab. That mock
  // patched only ONE plane, so any session.updated — which a tab change now emits
  // (#1812) — replaced the projection with the real roster and deleted the injected
  // tab out from under the assertion below: an intermittent, entirely fictional
  // failure (#1818). Seeding it for real is what removes the race; there is no reload,
  // no route, and no retry left to prop it up.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const nourlTab = tabbar.locator(".af-tab", { hasText: NOURL_TAB });
  await expect(nourlTab).toHaveCount(1, { timeout: 15_000 });
  await nourlTab.click();

  // A clean fallback (no broken iframe), not a blank pane.
  const fallback = page.locator(".af-term-host .af-pane-host .af-webpane-fallback");
  await expect(fallback).toBeVisible({ timeout: 10_000 });
  await expect(fallback).toContainText("no URL");
  // The "open ↗" link is WITHDRAWN — there is nothing to open. This asserts the
  // `hidden` actually takes effect: .af-webpane-open carries an author `display`
  // rule, which outranks the UA [hidden] rule, so hiding it needs an explicit
  // [hidden] override in styles.css. Without that this link renders and goes nowhere
  // (the state #1827's voice polish silently left it in until #1809 caught it).
  await expect(page.locator(".af-webpane-open:visible")).toHaveCount(0);
  // ↻ is withdrawn for the same reason and with the same CSS caveat: with no target
  // there is nothing to reload, and this branch returns before the click listener is
  // even attached — so the button was not merely useless, it was unwired.
  await expect(
    page.locator(".af-term-host .af-webpane-reload:visible"),
    "a URL-less pane must not offer a ↻ that cannot reload",
  ).toHaveCount(0);
});

test("split panes (fix): a WEB/iframe pane doesn't swallow a tab drag — dropping on its edge splits", REAL_FIXTURE, async () => {
  // The bug: dragover/drop over an <iframe> go to the FRAMED document, so a pane showing a
  // web tab ate the drag — no drop zone, no split, "nothing happens". The fix makes pane
  // iframes pointer-events:none for the duration of a drag, so the drag reaches the pane.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab", { hasText: "preview" }).click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drag the Agent tab onto the RIGHT edge of the iframe pane. The drop point is over the
  // frame, exactly where a user aims.
  const res = await dragTabOntoPaneHitTested(page, "Agent", "right");
  // The drag reached the parent document rather than being taken by the frame — the bug
  // itself, asserted directly. Before the fix this is `true` and the split below never
  // happens.
  expect(res.swallowed).toBe(false);

  // The split really happened: two panes, the web tab's iframe alongside a live xterm.
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .af-pane.af-pane-multi")).toHaveCount(2);
  await expect(page.locator(".af-term-host iframe.af-webframe")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);
  // The framed content survived the split — still the proxied dev server, not a blank pane.
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });

  // Collapse back to a single pane for the flows that follow.
  const termPane = page.locator(".af-term-host .af-pane", { hasNot: page.locator("iframe.af-webframe") });
  await termPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
});

test("split panes (fix): the WEB tab itself drags onto a terminal pane edge and splits", REAL_FIXTURE, async () => {
  // The mirror of the case above: the web tab as the drag SOURCE, dropped on a terminal
  // pane. The target isn't framed, so this proves the fix didn't break the ordinary path.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);

  await dragTabOntoPaneHitTested(page, "preview", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  // One pane is the web tab's iframe, the other the agent's terminal.
  await expect(page.locator(".af-term-host iframe.af-webframe")).toHaveCount(1, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);

  const webPane = page.locator(".af-term-host .af-pane", { has: page.locator("iframe.af-webframe") });
  await webPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
});

test("split panes (#1817 follow-up): Alt+j onto a WEB pane returns the keyboard to the rail, not the pane you left", REAL_FIXTURE, async () => {
  // The cyclePane half of the SplitView.focus() boolean contract. nav.ts resolves Alt+j/k
  // in EITHER mode, ahead of the terminal branch, so a user ATTACHED to a terminal pane can
  // cycle onto an iframe pane that has no xterm to receive the keyboard. When cyclePane
  // ignored the boolean, focus() silently no-opped and the pane the user cycled AWAY from
  // kept DOM focus: the header highlighted the web pane while keystrokes still went to the
  // agent — a silent WRONG target, worse than the silent no-target on the create path.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  await dragTabOntoPaneHitTested(page, "preview", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host iframe.af-webframe")).toHaveCount(1, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);

  // The precondition: really attached, with a real xterm holding the keyboard. (Clicking
  // the terminal directly — rail+Enter would attach the FOCUSED pane, which the drop just
  // made the web one.)
  await page.locator(".af-term-host .xterm").click();
  await expect(page.locator(".af-app.af-kb-terminal")).toBeVisible();

  // Alt+j cycles pane focus onto the web pane. There is no terminal to hand the keyboard
  // to, so it must come back to the rail rather than stay with the agent behind us.
  await page.keyboard.press("Alt+j");
  await expect(
    page.locator(".af-app.af-kb-rail"),
    "cycling onto a pane with no terminal must release the keyboard to the rail",
  ).toBeVisible();

  // Proof the keyboard really is the rail's: a bare j navigates instead of reaching the
  // agent we cycled away from.
  await expect(row(page, SESSION_WEB)).toHaveClass(/af-row-selected/);
  await page.keyboard.press("j");
  await expect(
    row(page, SESSION_WEB),
    "j must navigate the rail, not land in the terminal we cycled away from",
  ).not.toHaveClass(/af-row-selected/);
  await page.keyboard.press("k");
  await expect(row(page, SESSION_WEB)).toHaveClass(/af-row-selected/);

  // Leave one pane behind, exactly as the tests around this one do.
  const webPane = page.locator(".af-term-host .af-pane", { has: page.locator("iframe.af-webframe") });
  await webPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
});

test("split panes (fix): a pane iframe is inert ONLY while dragging — normal interaction is untouched", REAL_FIXTURE, async () => {
  // The fix must not cost the web tab its interactivity: the frame is inert for the drag
  // and immediately usable again afterwards. Both states are asserted on the live frame.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await page.locator(".af-tabbar .af-tab", { hasText: "preview" }).click();
  await expect(page.locator(".af-term-host iframe.af-webframe")).toHaveCount(1, { timeout: 15_000 });

  const framePointerEvents = () =>
    page.evaluate(() => {
      const f = document.querySelector(".af-term-host iframe.af-webframe");
      return f ? getComputedStyle(f).pointerEvents : "missing";
    });

  // At rest the frame takes the pointer — clicks and scrolls inside the preview work.
  expect(await framePointerEvents()).toBe("auto");

  // Mid-drag it is inert, which is what lets the drag through to the pane.
  await page.evaluate(() => {
    const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes("Agent"));
    tab?.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: new DataTransfer() }));
  });
  expect(await framePointerEvents()).toBe("none");

  // dragend restores it. (A drag whose source is replaced mid-drag is covered by the bar
  // rebuild clearing the same flag — see the #1737 stuck-state test above — so the frame
  // can't be left dead either way.)
  await page.evaluate(() => {
    const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes("Agent"));
    tab?.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: new DataTransfer() }));
  });
  expect(await framePointerEvents()).toBe("auto");
});

test("web tab (feat): a surviving web tab that only SHIFTS ordinal is followed, not remounted (#1779)", REAL_FIXTURE, async () => {
  // Ordering note: this consumes probe-web's "preview" tab, so it must stay AFTER the
  // web-tab tests above that rely on it.
  //
  // A pane shows the EXTERNAL web tab (index 2). A lower tab is closed, so the tab it
  // shows shifts to index 1. Nothing about that tab changed — and an external tab's
  // iframe src is the target URL, which encodes no ordinal — so the frame must be
  // FOLLOWED in place. Remounting would reload it and drop its in-page state.
  //
  // (A PROXIED tab used to be the opposite case and had to remount, because its src
  // was /v1/webtab/{session}/{ORDINAL}/. Since #1810 keyed that route by the stable
  // tab id, no web pane's address embeds an ordinal and both kinds are followed —
  // see the #1810 test above, which asserts it on a proxied pane.)
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar.locator(".af-tab", { hasText: "preview" })).toHaveCount(1, { timeout: 15_000 });

  await tabbar.locator(".af-tab", { hasText: "external" }).click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  // An external tab is now frameless (item A): its pane shows af's fallback, not the
  // site. But the no-remount property this test turns on is unchanged — the iframe
  // ELEMENT still lives in the pane (hidden, no src), so stamping it and watching the
  // stamp survive an ordinal shift proves the pane was FOLLOWED, not rebuilt, exactly
  // as before. The external fallback's presence is what marks it as the external tab.
  const externalFallback = page.locator(".af-term-host .af-webpane-fallback.af-webpane-external");
  await expect(externalFallback).toBeVisible();

  // Stamp the iframe element. An expando rides the DOM node itself, so it survives if
  // and only if this exact element is still mounted — a remount builds a fresh one.
  await page.evaluate(() => {
    const f = document.querySelector(".af-term-host .af-pane-host iframe.af-webframe") as
      | (HTMLIFrameElement & { __afStamp?: string })
      | null;
    if (f) {
      f.__afStamp = "af-not-remounted";
    }
  });

  // Close the LOWER "preview" tab: the shown external tab shifts from index 2 to 1.
  await tabbar.locator(".af-tab", { hasText: "preview" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab", { hasText: "preview" })).toHaveCount(0, { timeout: 30_000 });
  // The shift really happened — without this the assertion below would prove nothing.
  // Asserted as "external now sits at ordinal 1", not as a roster total (#1863): probe-web
  // permanently carries the seeded URL-less tab (#1818), and any tab mutation republishes
  // the roster (#1812), so a total is both bigger than it looks and free to move under the
  // assertion. The ordinal is the thing this test actually turns on — [Agent, preview,
  // external, nourl] before the close, [Agent, external, nourl] after, so "external"
  // shifts 2 → 1. data-tab-index is the roster index the bar rendered it at.
  await expect(tabbar.locator('.af-tab[data-tab-index="1"] .af-tab-label')).toHaveText("external", {
    timeout: 30_000,
  });

  // The pane still shows the SAME tab, through the SAME iframe element.
  await expect(frame).toHaveCount(1);
  await expect(externalFallback).toBeVisible();
  const stamp = await page.evaluate(() => {
    const f = document.querySelector(".af-term-host .af-pane-host iframe.af-webframe") as
      | (HTMLIFrameElement & { __afStamp?: string })
      | null;
    return f?.__afStamp ?? null;
  });
  expect(stamp).toBe("af-not-remounted");
});

// Ordering note: the #1779 test above consumed probe-web's "preview" tab, so the
// session's tabs here are [Agent, external] and "external" is the non-agent tab these
// two use as the retained pane.

test("tabs (#1855): re-selecting the SAME session leaves the bar on the tab the pane shows, not Agent", REAL_FIXTURE, async () => {
  // Selecting a session asserted `activeTab: 0` about a layout it had already retained:
  // the tree still pointed at tab N, so the bar highlighted Agent while the pane kept
  // showing N. Re-selecting the session you are ALREADY on is the shortest proof —
  // setSession's same-session branch finds nothing changed, so report() (the only
  // writer of activeTab) never fires and the reset is never undone.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");

  // Park the pane on "external" (index 1) via a REAL index change (0 then 1): clicking
  // the tab you are already on is itself a no-op report, so it could not establish this
  // precondition on a desynced store.
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await tabbar.locator(".af-tab", { hasText: "external" }).click();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });

  // Click the row that is already selected. The pane is untouched (same session, same
  // tree) — so the bar must still name the tab it is showing. External tabs are
  // frameless (item A), so the external fallback's presence is the "pane still shows
  // external" signal, and the active label is the bar's claim under test.
  await row(page, SESSION_WEB).click();
  await expect(frame).toHaveCount(1);
  await expect(page.locator(".af-term-host .af-webpane-fallback.af-webpane-external")).toBeVisible();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");
});

test("tabs (#1855): switching away and back keeps activeTab on the visible pane — and the next close doesn't yank it to Agent", REAL_FIXTURE, async () => {
  // The issue's own repro. It needs the re-entry to settle on the SAME focused index the
  // last report carried: report() dedups on (focusedTab, shownTabs, paneCount), so an
  // identical index makes it early-return and the `activeTab: 0` reset stands
  // uncorrected. probe-web is re-entered on tab 1 ("external"), so probe-b is parked on
  // ITS tab 1 to collide.
  const tabbar = page.locator(".af-tabbar");
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");

  // A throwaway terminal tab on probe-web, so there is a tab to close later that is
  // NEITHER the agent tab nor the active one — closing it must not move the pane.
  //
  // Waited on BY NAME, never by a roster total (#1863). probe-web permanently carries the
  // seeded URL-less tab (#1818), so its roster here is [Agent, external, nourl] and the
  // old "3 tabs" wait was ALREADY satisfied before the + click even landed — it passed
  // instantly against the pre-create roster and let the test race ahead of the
  // precondition it was there to establish. The tab's own name can't be true early, and
  // it stays true across the session.updated rebuild the create publishes (#1812).
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await createTerminalTab(page);
  const throwaway = tabbar.locator(".af-tab", { hasText: "Terminal" });
  await expect(throwaway).toHaveCount(1, { timeout: 30_000 });
  // The create attaches the new tab, so the pane is on it — which is what makes the
  // "external" click below the real index change this test needs going in.
  await expect(throwaway).toHaveClass(/af-tab-active/, { timeout: 30_000 });

  // Park probe-web on "external" (index 1) — an index change, so the store is truthful
  // going in.
  await tabbar.locator(".af-tab", { hasText: "external" }).click();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });

  // Switch away, and leave probe-b focused on its own tab 1 (create also attaches),
  // so the last reported focused tab is 1 — the collision.
  await row(page, SESSION_B).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Terminal");

  // Back to probe-web: the retained pane still shows "external", so the bar must too.
  // External is frameless (item A); the external fallback marks the shown tab.
  await row(page, SESSION_WEB).click();
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .af-webpane-fallback.af-webpane-external")).toBeVisible();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");

  // Closing an UNRELATED tab (the throwaway, to the RIGHT of the active one) leaves the
  // pane where it was. With a stale activeTab the close arithmetic read 0 and re-pointed
  // the pane at Agent, tearing down the live iframe.
  // (The bar always renders the SELECTED session, and probe-web is selected here, so
  // `throwaway` resolves against probe-web's Terminal — not the one probe-b grew above.)
  await throwaway.locator(".af-tab-close").click();
  // Again by name (#1863): the disappearance of THIS tab is the event being waited on,
  // and the seeded nourl tab means the surviving total is 3, not 2.
  await expect(throwaway).toHaveCount(0, { timeout: 30_000 });
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");
  await expect(frame).toHaveCount(1);

  // Restore probe-b's tab set for the flows that follow.
  await row(page, SESSION_B).click();
  await expect(tabbar.locator(".af-tab", { hasText: "Terminal" })).toHaveCount(1);
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (feat): logout clears retained trees — a fresh login shows the single-leaf default", REAL_FIXTURE, async () => {
  // Split A into two panes.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await createTerminalTab(page);
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });

  // Log out, then reconnect (tokenless loopback: the no-auth login offers a single
  // Connect button, no token to paste).
  await page.locator(".af-appbar button", { hasText: "Disconnect" }).click();
  await expect(page.locator(".af-login")).toBeVisible();
  await page.locator(".af-login button.af-primary").click();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-app")).toHaveAttribute("data-live", "open");

  // Re-select A: the retained split was cleared on logout, so it opens as a SINGLE
  // pane (the zero-config default), not the previous two-pane split.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Restore A to a single tab (the shell tab survived on the daemon; only the pane
  // was gone) for the later create/kill/archive flows.
  const bar = page.locator(".af-tabbar");
  const shellTab = bar.locator(".af-tab", { hasText: "Terminal" });
  if ((await shellTab.count()) > 0) {
    await shellTab.locator(".af-tab-close").click();
    await expect(bar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
  }
});

test("project switcher (redesign PR2): lists projects with counts; selecting one scopes + swaps the rail; persists", REAL_FIXTURE, async () => {
  // The top-right switcher shows the current (default) project — the first repo.
  const switcher = page.locator(".af-project-switch");
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");

  // Open the menu: it lists every project — the two session repos AND the task-only
  // repo (the cross-project glance that replaces the old all-projects rail), each with
  // its per-project count meta.
  await switcher.click();
  const menu = page.locator(".af-project-menu");
  await expect(menu).toBeVisible();
  const items = menu.locator(".af-project-item");
  await expect(items).toHaveCount(3);
  const currentItem = menu.locator(".af-project-item-current");
  await expect(currentItem.locator(".af-project-item-name")).toHaveText("mock-repo");
  // The second project (a session repo) shows its session count; the third (task-only,
  // no session) shows its task count — proof it's derived from sessions OR tasks.
  await expect(projectItem(page, "mock-repo-2").locator(".af-project-item-meta")).toContainText("1 session");
  await expect(projectItem(page, "mock-repo-3").locator(".af-project-item-meta")).toContainText("1 task");

  // Select the second project: the rail SWAPS to it — only SESSION_C shows, and the
  // first repo's A/B are gone (proof the rail is scoped to ONE project, the behavior
  // this PR replaces).
  await projectItem(page, "mock-repo-2").click();
  await expect(menu).toBeHidden();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-2");
  await expect(row(page, SESSION_C)).toBeVisible();
  await expect(row(page, SESSION_A)).toHaveCount(0);
  await expect(row(page, SESSION_B)).toHaveCount(0);

  // The choice persists across a reload (localStorage): the second project is still
  // selected and its rail still shows only SESSION_C.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-2");
  await assertRealRailFixture(page, SESSION_C);
  await expect(row(page, SESSION_C)).toBeVisible();
  await expect(row(page, SESSION_A)).toHaveCount(0);

  // Switch back to the first project so the following rail-driven flows (tabs, create,
  // kill, archive) see A/B again. Persisted as the first repo for the rest of the run.
  await switcher.click();
  await projectItem(page, "mock-repo").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");
  await expect(row(page, SESSION_A)).toBeVisible();
  await expect(row(page, SESSION_B)).toBeVisible();
});

test("#2276: selecting the reconciled project repairs a stale persisted choice", REAL_FIXTURE, async ({ browser }) => {
  const fixtureRepo = process.env.AF_MOCK_REPO;
  expect(fixtureRepo, "the real-project persistence regression needs AF_MOCK_REPO").toBeTruthy();

  const ctx = await browser.newContext();
  try {
    const p = await ctx.newPage();
    await openTokenless(p);
    await expect(p.locator(".af-project-switch-name")).toHaveText("mock-repo");

    // Reproduce the state left by a project disappearing: reconciliation has already
    // selected a valid fallback in memory, but localStorage still names the vanished
    // project. Clicking the checked project is the user's explicit choice to keep it,
    // so it must repair storage even though the in-memory scope does not change.
    await p.evaluate((stale) => localStorage.setItem("af-project", stale), `${fixtureRepo}-deleted`);
    await p.locator(".af-project-switch").click();
    const current = projectItem(p, "mock-repo");
    await expect(current).toHaveAttribute("aria-selected", "true");
    await current.click();
    await expect(p.locator(".af-project-menu")).toBeHidden();

    expect(await p.evaluate(() => localStorage.getItem("af-project"))).toBe(fixtureRepo);
  } finally {
    await ctx.close();
  }
});

test("task-only project (redesign PR2, Fix 1): a repo with a task but no session lists, scopes Tasks, and its delete is disabled", REAL_FIXTURE, async () => {
  // Select the task-only project (a third repo with a task but NO session). It lists in
  // the switcher (derived from sessions OR tasks), so its tasks stay reachable.
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo-3").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-3");

  // Its rail is the clean empty state (no sessions), not a blank rail. It has no
  // archived sessions either, so the empty state stays a bare one-liner — no
  // "N archived hidden" hint to explain something that isn't there.
  const empty = page.locator(".af-rail-empty-project");
  await expect(empty).toContainText("No active sessions in");
  await expect(empty).toContainText("mock-repo-3");
  await expect(empty.locator(".af-rail-empty-new")).toBeVisible();
  await expect(empty).not.toContainText("archived hidden");

  // The delete-project action is DISABLED here — there are no live sessions to archive,
  // so it can never be a silent no-op (Greptile Fix 2). An archived-only repo, by the
  // same rule, is never even a project. Toggle the menu open, assert, then closed.
  await page.locator(".af-project-switch").click();
  await expect(page.locator(".af-project-menu .af-project-delete")).toBeDisabled();
  await page.locator(".af-project-switch").click();
  await expect(page.locator(".af-project-menu")).toBeHidden();

  // The Tasks view is scoped to this project: its task shows, and the first repo's
  // seeded task does NOT (proof the Tasks view operates within the selected project).
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const tasks = page.locator(".af-tasks");
  await expect(tasks).toBeVisible();
  await expect(tasks.locator(".af-task-row", { hasText: TASK3_NAME })).toHaveCount(1);
  await expect(tasks.locator(".af-task-row", { hasText: SEEDED_TASK })).toHaveCount(0);

  // Return to the first project + the sessions view for the following rail-driven flows.
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");
  await expect(row(page, SESSION_A)).toBeVisible();
});

test("task-only project (redesign PR2, follow-on): add-task targets ITS repo, and a reload restores it as itself", REAL_FIXTURE, async () => {
  // Capture the AddTask request so we can prove it targeted the task-only project's
  // repo — not a session-derived one, and not blocked by the absence of a session.
  let addBody: { task?: { project_path?: string } } | null = null;
  await page.route("**/v1/AddTask", async (route) => {
    addBody = route.request().postDataJSON();
    await route.continue();
  });

  // Select the task-only project.
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo-3").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-3");

  // Fix 1: add a task from the Tasks view. The project picker DEFAULTS to this
  // (task-only) project, so submitting creates the task on mock-repo-3's repo, and the
  // form is not blocked despite the project having no session.
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const added = `mock3-added-${Date.now().toString(36)}`;
  await page.locator(".af-tasks-add").click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator('input[aria-label="Task name"]').fill(added);
  await setEveryNMinutes(modal, 5);
  await modal.locator('textarea[aria-label="Prompt"]').fill("echo mock3-added");
  await modal.locator("button.af-primary").click();

  await expect(page.locator(".af-tasks .af-task-row", { hasText: added })).toBeVisible({ timeout: 30_000 });
  await expect(modal).toBeHidden();
  // The task was created on the SELECTED task-only project's repo (mock-repo-3), not
  // another project's (mock-repo / mock-repo-2).
  expect(addBody?.task?.project_path, "AddTask must target the selected task-only project's repo").toContain(
    "mock-repo-3",
  );
  expect(addBody?.task?.project_path).not.toContain("mock-repo-2");
  await page.unroute("**/v1/AddTask");

  // Fix 2: reload. The persisted task-only selection restores AS ITSELF (a real
  // task-derived project), not a temporary session-backed fallback — and the Tasks
  // view scopes to it (its tasks show, another project's seeded task does not).
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-3");
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  await expect(page.locator(".af-tasks .af-task-row", { hasText: TASK3_NAME })).toHaveCount(1);
  await expect(page.locator(".af-tasks .af-task-row", { hasText: SEEDED_TASK })).toHaveCount(0);

  // Return to the first project + the sessions view for the following rail-driven flows.
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");
  await expect(row(page, SESSION_A)).toBeVisible();
});

test("#2381: the manual-trigger fixture cannot also fire from its cron during the test window", () => {
  const boundary = new Date(2026, 6, 22, 9, 10, 0);
  const cron = manualTriggerFixtureCron(boundary);

  for (let offset = 0; offset <= 10; offset += 1) {
    const duringTest = new Date(boundary.getTime() + offset * 60_000);
    expect(cronMatchesMinute(cron, duringTest), `schedule ${cron} fires at +${offset} minutes`).toBe(false);
  }
});

test("tasks view (#1592 PR8): list the seeded task; add / trigger / remove round-trips", REAL_FIXTURE, async () => {
  // Capture the task-mutation request bodies so we can prove every mutation carries
  // the STABLE task id — the add mints it client-side, and trigger/remove must send
  // that SAME id, never the (non-unique) name (the #1678 id-scoping class, PR8).
  let addedTaskId: string | undefined;
  let triggerId: string | undefined;
  let removeId: string | undefined;
  await page.route("**/v1/AddTask", async (route) => {
    addedTaskId = route.request().postDataJSON()?.task?.id;
    await route.continue();
  });
  await page.route("**/v1/TriggerTask", async (route) => {
    triggerId = route.request().postDataJSON()?.id;
    await route.continue();
  });
  await page.route("**/v1/RemoveTask", async (route) => {
    removeId = route.request().postDataJSON()?.id;
    await route.continue();
  });

  // Switch to the tasks view. The seeded cron task is listed on load — proof the
  // pane is driven by the daemon's ListTasks, not a static list.
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  await expect(page.locator('.af-viewtab[data-view="tasks"]')).toHaveClass(/af-viewtab-active/);
  const tasks = page.locator(".af-tasks");
  await expect(tasks).toBeVisible();
  const seeded = tasks.locator(".af-task-row", { hasText: SEEDED_TASK });
  await expect(seeded).toHaveCount(1);
  await expect(seeded.locator(".af-task-trigger")).toContainText("0 9 * * *");

  // Add a cron task via the + Add modal. The project picker defaults to the scoped
  // project (redesign PR2), so the task lands in it; a cron task requires a prompt
  // (the daemon rejects an empty one).
  const added = `probe-task-${Date.now().toString(36)}`;
  await tasks.locator(".af-tasks-add").click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator('input[aria-label="Task name"]').fill(added);
  await setCustomCron(modal, manualTriggerFixtureCron(new Date()));
  await modal.locator('textarea[aria-label="Prompt"]').fill("echo scheduled-web");
  await modal.locator("button.af-primary").click();

  // The new task's row appears (AddTask succeeded; the list refetched).
  const addedRow = tasks.locator(".af-task-row", { hasText: added });
  await expect(addedRow).toBeVisible({ timeout: 30_000 });
  await expect(modal).toBeHidden();
  expect(addedTaskId, "AddTask must mint + send a stable task id").toBeTruthy();

  // Enable/disable round-trips via UpdateTask: the new task is enabled (Disable
  // shown). Disabling flips it, and re-enabling flips it back — proof the toggle
  // rides UpdateTask keyed by the task's id.
  await addedRow.locator("button", { hasText: "Disable" }).click();
  await expect(addedRow.locator("button", { hasText: "Enable" })).toBeVisible({ timeout: 30_000 });
  await addedRow.locator("button", { hasText: "Enable" }).click();
  await expect(addedRow.locator("button", { hasText: "Disable" })).toBeVisible({ timeout: 30_000 });

  // Trigger-now round-trips via TriggerTask (enabled cron tasks only). Await the RPC
  // response and assert the envelope carries no error — the daemon fired it — and the
  // id sent matches the one AddTask minted (id-stability, not the name).
  const [triggerResp] = await Promise.all([
    page.waitForResponse("**/v1/TriggerTask"),
    addedRow.locator("button", { hasText: "Trigger" }).click(),
  ]);
  expect((await triggerResp.json()).error, "TriggerTask must succeed (no envelope error)").toBeNull();
  expect(triggerId, "TriggerTask must send the same stable id AddTask minted").toBe(addedTaskId);

  // Remove round-trips via RemoveTask: the row disappears, and the id sent is again
  // the stable one (never the name).
  await addedRow.locator("button", { hasText: "Remove" }).click();
  await expect(tasks.locator(".af-task-row", { hasText: added })).toHaveCount(0, { timeout: 30_000 });
  expect(removeId, "RemoveTask must send the same stable id").toBe(addedTaskId);

  await page.unroute("**/v1/AddTask");
  await page.unroute("**/v1/TriggerTask");
  await page.unroute("**/v1/RemoveTask");

  // Return to the sessions view so the subsequent create/kill/archive flows drive the
  // rail (which is hidden while the tasks view shows).
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await expect(page.locator(".af-rail-list")).toBeVisible();
});

test("tasks view edit (#1935): the Edit form is seeded from the task, and a changed cron/prompt PERSISTS", REAL_FIXTURE, async () => {
  // The gap this fixes: before #1935 the web could create a task but never change it —
  // the only UpdateTask call was the enable toggle, and no edit form existed. This
  // fails on the old bundle at the very first step (no "Edit" button in the row).
  //
  // Capture the AddTask (to learn the minted id) and UpdateTask (to prove the patch is
  // a field-level UpdateTask keyed by that SAME stable id, #1678) request bodies.
  let addedTaskId: string | undefined;
  let updateBody: { id?: string; update?: Record<string, unknown> } | null = null;
  await page.route("**/v1/AddTask", async (route) => {
    addedTaskId = route.request().postDataJSON()?.task?.id;
    await route.continue();
  });
  await page.route("**/v1/UpdateTask", async (route) => {
    updateBody = route.request().postDataJSON();
    await route.continue();
  });

  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const tasks = page.locator(".af-tasks");
  await expect(tasks).toBeVisible();

  // Seed a task of our own to edit (so this test never mutates the harness's seeded
  // task, which other tests assert on). A cron task requires a prompt.
  const named = `probe-edit-${Date.now().toString(36)}`;
  const originalCron = "*/5 * * * *";
  await tasks.locator(".af-tasks-add").click();
  const addModal = page.locator(".af-modal-card");
  await expect(addModal).toBeVisible();
  await addModal.locator('input[aria-label="Task name"]').fill(named);
  await setEveryNMinutes(addModal, 5); // generates originalCron
  await addModal.locator('textarea[aria-label="Prompt"]').fill("echo original");
  await addModal.locator("button.af-primary").click();

  const editedRow = tasks.locator(".af-task-row", { hasText: named });
  await expect(editedRow).toBeVisible({ timeout: 30_000 });
  await expect(addModal).toBeHidden();
  expect(addedTaskId, "AddTask must mint + send a stable task id").toBeTruthy();
  await expect(editedRow.locator(".af-task-trigger")).toContainText(originalCron);

  // Open the Edit form. It reuses the add-task modal in edit mode, SEEDED from the
  // task's current values — proof the form is not a blank re-entry.
  await editedRow.locator("button", { hasText: "Edit" }).click();
  const editModal = page.locator(".af-modal-card");
  await expect(editModal).toBeVisible();
  await expect(editModal.locator(".af-modal-title")).toHaveText("Edit task");
  await expect(editModal.locator('input[aria-label="Task name"]')).toHaveValue(named);
  // The schedule seeds through ParseCron (#2057): the stored expression re-opens as
  // the preset it maps to, and the read-only cron line shows what would be saved.
  await expect(editModal.locator('select[aria-label="Schedule type"]')).toHaveValue("everyNMinutes");
  await expect(editModal.locator('input[aria-label="Interval"]')).toHaveValue("5");
  await expect(editModal.locator('input[aria-label="Generated cron"]')).toHaveValue(originalCron);
  await expect(editModal.locator('textarea[aria-label="Prompt"]')).toHaveValue("echo original");

  // Change the cron expression and the prompt, then save. This one goes through the
  // Custom escape hatch, so the literal expression is what gets stored.
  const newCron = "30 8 * * 1";
  await setCustomCron(editModal, newCron);
  await editModal.locator('textarea[aria-label="Prompt"]').fill("echo edited");
  await editModal.locator("button.af-primary", { hasText: "Save" }).click();
  await expect(editModal).toBeHidden();

  // The UpdateTask patch is a field-level TaskUpdate keyed by the SAME id AddTask
  // minted (never the name). It carries the changed fields plus project_path (#1935's
  // type addition reaching the wire), and does NOT carry `enabled` — the edit form
  // leaves the toggle's bit as-stored (the field-level merge, #1700).
  expect(updateBody?.id, "UpdateTask must target the id AddTask minted").toBe(addedTaskId);
  expect(updateBody?.update?.cron_expr).toBe(newCron);
  expect(updateBody?.update?.watch_cmd, "switching to/keeping cron clears watch_cmd").toBe("");
  expect(updateBody?.update?.prompt).toBe("echo edited");
  expect(updateBody?.update?.project_path, "the edit reaches project_path (#1935)").toBeTruthy();
  expect(updateBody?.update, "the edit patch omits `enabled` (the toggle owns it)").not.toHaveProperty("enabled");

  // The row reflects the new trigger without a reload (the list refetched).
  await expect(editedRow.locator(".af-task-trigger")).toContainText(newCron, { timeout: 30_000 });

  // The real proof: RELOAD and re-fetch from the daemon. The new cron persisted, so
  // the daemon actually applied the UpdateTask — not just an optimistic local echo.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await assertRealRailFixture(page);
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const reloadedRow = page.locator(".af-tasks .af-task-row", { hasText: named });
  await expect(reloadedRow).toBeVisible({ timeout: 30_000 });
  await expect(reloadedRow.locator(".af-task-trigger")).toContainText(newCron);
  await expect(reloadedRow.locator(".af-task-trigger")).not.toContainText(originalCron);

  // Clean up our task and return to the sessions view for the following flows.
  await page.route("**/v1/RemoveTask", (route) => route.continue());
  await reloadedRow.locator("button", { hasText: "Remove" }).click();
  await expect(page.locator(".af-tasks .af-task-row", { hasText: named })).toHaveCount(0, { timeout: 30_000 });

  await page.unroute("**/v1/AddTask");
  await page.unroute("**/v1/UpdateTask");
  await page.unroute("**/v1/RemoveTask");
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await expect(page.locator(".af-rail-list")).toBeVisible();
});

test("schedule picker (#2057): a preset generates the cron, an edit re-opens as that preset, and the change PERSISTS", REAL_FIXTURE, async () => {
  // Phase 2 of #2057: the raw-cron box in the task form is replaced by a schedule
  // TYPE plus only that type's inputs, a plain-English preview, and the generated
  // cron shown read-only. Cron is still what gets stored, so this asserts the whole
  // chain — picker state → generated expression → what the daemon persists → what
  // the picker shows when the task is re-opened.
  //
  // The cron/human strings asserted here are the same ones the shared vector file
  // (schedule/testdata/vectors.json) pins for the Go model, so this flow also
  // demonstrates the browser and TUI pickers agreeing on real input.
  let addedCron: string | undefined;
  let updateBody: { update?: Record<string, unknown> } | null = null;
  await page.route("**/v1/AddTask", async (route) => {
    addedCron = route.request().postDataJSON()?.task?.cron_expr;
    await route.continue();
  });
  await page.route("**/v1/UpdateTask", async (route) => {
    updateBody = route.request().postDataJSON();
    await route.continue();
  });

  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const tasks = page.locator(".af-tasks");
  await expect(tasks).toBeVisible();

  const named = `probe-sched-${Date.now().toString(36)}`;
  await tasks.locator(".af-tasks-add").click();
  const addModal = page.locator(".af-modal-card");
  await expect(addModal).toBeVisible();
  await addModal.locator('input[aria-label="Task name"]').fill(named);

  // A brand-new task opens on a friendly default (daily at 9:00 AM), NOT a blank
  // cron box, with the preview and generated cron already filled in.
  const typeSelect = addModal.locator('select[aria-label="Schedule type"]');
  const generated = addModal.locator('input[aria-label="Generated cron"]');
  const preview = addModal.locator(".af-schedule-human");
  await expect(typeSelect).toHaveValue("daily");
  await expect(generated).toHaveValue("0 9 * * *");
  await expect(preview).toHaveText("Every day at 9:00 AM");

  // Contextual inputs: a daily schedule shows the time, and nothing else. The
  // weekday toggles, the day-of-month cell and the raw-cron escape hatch are all
  // hidden until their type is picked — and the OTHER trigger's watch field stays
  // hidden too (it shares the same `hidden` mechanism this relies on).
  await expect(addModal.locator('input[aria-label="Hour"]')).toBeVisible();
  await expect(addModal.locator(".af-weekdays")).toBeHidden();
  await expect(addModal.locator('input[aria-label="Day of month"]')).toBeHidden();
  await expect(addModal.locator('input[aria-label="Cron expression"]')).toBeHidden();
  await expect(addModal.locator('input[aria-label="Watch command"]')).toBeHidden();

  // Build "every week on Mon, Wed at 9:30 AM" through the picker.
  await typeSelect.selectOption("weekly");
  await expect(addModal.locator(".af-weekdays")).toBeVisible();
  await addModal.locator('input[aria-label="Minute"]').fill("30");
  // Monday is on by default; adding Wednesday makes the two-day list.
  await expect(addModal.locator('button[aria-label="Monday"]')).toHaveAttribute("aria-pressed", "true");
  await addModal.locator('button[aria-label="Wednesday"]').click();
  await expect(addModal.locator('button[aria-label="Wednesday"]')).toHaveAttribute("aria-pressed", "true");

  // The preview and the read-only cron are live, and byte-identical to what the Go
  // model produces for this schedule.
  await expect(preview).toHaveText("Every week on Mon, Wed at 9:30 AM");
  await expect(generated).toHaveValue("30 9 * * 1,3");

  await addModal.locator('textarea[aria-label="Prompt"]').fill("echo scheduled");
  await addModal.locator("button.af-primary").click();

  // What the daemon was asked to store is the generated expression — the wire format
  // is unchanged, only the input UX.
  const row = tasks.locator(".af-task-row", { hasText: named });
  await expect(row).toBeVisible({ timeout: 30_000 });
  await expect(addModal).toBeHidden();
  expect(addedCron, "AddTask must carry the picker's generated cron").toBe("30 9 * * 1,3");
  await expect(row.locator(".af-task-trigger")).toContainText("30 9 * * 1,3");

  // Re-open it: the stored cron round-trips back through ParseCron into the WEEKLY
  // preset with its cells restored — not a raw string in a text box.
  await row.locator("button", { hasText: "Edit" }).click();
  const editModal = page.locator(".af-modal-card");
  await expect(editModal).toBeVisible();
  await expect(editModal.locator('select[aria-label="Schedule type"]')).toHaveValue("weekly");
  await expect(editModal.locator('input[aria-label="Hour"]')).toHaveValue("9");
  await expect(editModal.locator('input[aria-label="Minute"]')).toHaveValue("30");
  await expect(editModal.locator('select[aria-label="AM/PM"]')).toHaveValue("AM");
  await expect(editModal.locator('button[aria-label="Monday"]')).toHaveAttribute("aria-pressed", "true");
  await expect(editModal.locator('button[aria-label="Wednesday"]')).toHaveAttribute("aria-pressed", "true");
  await expect(editModal.locator('button[aria-label="Tuesday"]')).toHaveAttribute("aria-pressed", "false");
  await expect(editModal.locator('input[aria-label="Generated cron"]')).toHaveValue("30 9 * * 1,3");

  // Switch it to a different preset and save.
  await editModal.locator('select[aria-label="Schedule type"]').selectOption("everyNHours");
  await editModal.locator('input[aria-label="Interval"]').fill("6");
  await expect(editModal.locator(".af-schedule-human")).toHaveText("Every 6 hours");
  await expect(editModal.locator('input[aria-label="Generated cron"]')).toHaveValue("0 */6 * * *");
  await editModal.locator("button.af-primary", { hasText: "Save" }).click();
  await expect(editModal).toBeHidden();

  expect(updateBody?.update?.cron_expr, "the edit patch carries the newly generated cron").toBe("0 */6 * * *");
  await expect(row.locator(".af-task-trigger")).toContainText("0 */6 * * *", { timeout: 30_000 });

  // The real proof: RELOAD and re-fetch from the daemon — the generated cron was
  // actually persisted, and the picker seeds itself from it again.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await assertRealRailFixture(page);
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const reloadedRow = page.locator(".af-tasks .af-task-row", { hasText: named });
  await expect(reloadedRow).toBeVisible({ timeout: 30_000 });
  await expect(reloadedRow.locator(".af-task-trigger")).toContainText("0 */6 * * *");
  await reloadedRow.locator("button", { hasText: "Edit" }).click();
  const reopened = page.locator(".af-modal-card");
  await expect(reopened.locator('select[aria-label="Schedule type"]')).toHaveValue("everyNHours");
  await expect(reopened.locator('input[aria-label="Interval"]')).toHaveValue("6");
  await reopened.locator("button.af-ghost", { hasText: "Cancel" }).click();
  await expect(reopened).toBeHidden();

  // Clean up and return to the sessions view for the following flows.
  await reloadedRow.locator("button", { hasText: "Remove" }).click();
  await expect(page.locator(".af-tasks .af-task-row", { hasText: named })).toHaveCount(0, { timeout: 30_000 });

  await page.unroute("**/v1/AddTask");
  await page.unroute("**/v1/UpdateTask");
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await expect(page.locator(".af-rail-list")).toBeVisible();
});

test("#2218: slow create closes immediately, shows daemon state, then opens attached", REAL_FIXTURE, async () => {
  const created = `probe-create-slow-${Date.now().toString(36)}`;
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  await page.locator("button.af-rail-new").click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator('input[aria-label="Session title"]').fill(created);
  await modal.locator("button.af-primary").click();

  // The fake agent sleeps for eight seconds, so neither assertion can be hidden by
  // a fast local create. Submit synchronously releases the modal; the row arrives
  // separately from the daemon's session.updated projection with OpCreating.
  await expect(modal, "submit must release the modal before provisioning finishes").toBeHidden({ timeout: 3000 });
  const creating = row(page, created);
  await expect(creating, "the daemon's pending row must arrive during the slow backend").toHaveClass(/af-row-creating/, {
    timeout: 6000,
  });
  await expect(creating).toHaveAttribute("aria-disabled", "true");

  // A pending row has its final id but no terminal. It must stay inert so a click
  // cannot pre-select that id and suppress the success-time selection change that
  // builds the pane (the exact slow-only regression guarded by index.ts's comment).
  const selectedAfterClick = await creating.evaluate((row) => {
    row.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    return row.getAttribute("aria-selected");
  });
  expect(selectedAfterClick).toBe("false");

  // Completion replaces the same id, selects it in the same store update, and opens
  // the Agent tab attached. A stuck empty pane here is the naive async regression.
  await expect(creating).not.toHaveClass(/af-row-creating/, { timeout: 30_000 });
  await expect(creating).toHaveAttribute("aria-selected", "true");
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(1);
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 30_000 });

  // Leave no successful probe behind for later shared-page tests.
  await railAction(page, created, "Kill session").click();
  const killModal = page.locator(".af-modal-card");
  await expect(killModal).toBeVisible();
  await killModal.locator("button.af-danger").click();
  await expect(row(page, created)).toHaveCount(0, { timeout: 30_000 });
});

test("#2218: failing slow create shows the daemon error and leaves no phantom row", REAL_FIXTURE, async () => {
  const created = `probe-create-fail-${Date.now().toString(36)}`;
  await page.locator("button.af-rail-new").click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator('input[aria-label="Session title"]').fill(created);
  const response = page.waitForResponse((r) => r.url().endsWith("/v1/CreateSession") && r.request().method() === "POST");
  await modal.locator("button.af-primary").click();

  await expect(modal, "a failing backend must not hold the form open either").toBeHidden({ timeout: 3000 });
  const creating = row(page, created);
  await expect(creating, "failure must still expose the real in-flight window").toHaveClass(/af-row-creating/, {
    timeout: 6000,
  });

  const failed = await response;
  const envelope = (await failed.json()) as { error?: { message?: string } };
  const daemonMessage = envelope.error?.message ?? "";
  expect(daemonMessage).toContain("failed to start instance");
  await expect(page.locator(".af-toast"), "the web must render the daemon's exact failure").toHaveText(daemonMessage);
  await expect(creating, "the failed provisional id must be removed").toHaveCount(0, { timeout: 30_000 });

  // A fresh authoritative Snapshot must agree: reload cannot resurrect a phantom.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await assertRealRailFixture(page);
  await expect(row(page, created)).toHaveCount(0);
});

test.describe("create → kill (one session, two flows)", () => {
  // The ONE genuine ordering dependency in this file, and the only place serial
  // mode is warranted: create stashes the title it invented and kill consumes it,
  // so kill cannot run — or mean anything — on its own. Serial keeps the pair
  // honest: if create fails, kill SKIPS rather than failing a second time for a
  // reason that is not its own.
  //
  // Scoped to these two tests on purpose. This is the blast radius of a failure
  // here: two tests, not the 41 a file-wide serial took down in #1898.
  test.describe.configure({ mode: "serial" });

  test("create: the + New modal creates a session and its row appears", REAL_FIXTURE, async () => {
    const created = `probe-created-${Date.now().toString(36)}`;

    // Regression guard (#1592 PR7 review): first move the CURRENT session onto a
    // NON-agent tab, so a create path that wrongly preserved activeTab would build a
    // ?tab=1 stream URL for the brand-new session (which has only the agent tab).
    await row(page, SESSION_A).click();
    await expect(page.locator(".af-main.af-main-term")).toBeVisible();
    await createTerminalTab(page);
    await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(2, { timeout: 30_000 });
    await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Terminal");

    // Capture every PTY stream WebSocket opened from here on, so we can assert the
    // new session's stream carries NO stale tab= selector.
    const streamUrls: string[] = [];
    page.on("websocket", (ws) => {
      if (ws.url().includes("/stream")) {
        streamUrls.push(ws.url());
      }
    });

    await page.locator("button.af-rail-new").click();
    const modal = page.locator(".af-modal-card");
    await expect(modal).toBeVisible();

    // #1933 end-to-end: the backend picker is populated by the DAEMON (ListBackends
    // against the picked project), not by a list in the web. This is the only test
    // that proves the whole chain — enum → RPC → rendered options — through a real
    // daemon; the unit tests either side of it both stub their counterpart.
    const backendSelect = modal.locator('select[aria-label="Backend"]');
    await expect(backendSelect).toBeVisible();
    // Populated asynchronously, so wait for the daemon's answer rather than the
    // "repo default"-only placeholder the field is built with.
    await expect(backendSelect.locator("option")).not.toHaveCount(1, { timeout: 30_000 });
    await expect(backendSelect.locator("option")).toHaveText(
      [/^Repo default \(local\)$/, "local", "docker", "ssh", "Remote sandbox · coder-launch.sh (hook)"],
      { timeout: 30_000 },
    );
    // The mock repo configures no docker.image. Picking docker must state the missing
    // key AND block Create — the choose-time message standing in for the create-time
    // failure this issue is about. The reason is the daemon's own text, so this also
    // proves the CLI and the web say the same thing.
    await backendSelect.selectOption("docker");
    await expect(modal.locator(".af-modal-hint")).toHaveText(/docker\.image/);
    await expect(modal.locator("button.af-primary")).toBeDisabled();

    // The repo's versioned remote_hooks config is available, and the option names
    // its launcher while retaining "hook" as the submitted CLI/config key.
    await backendSelect.selectOption("hook");
    await expect(modal.locator(".af-modal-hint")).toHaveText("");
    await expect(modal.locator("button.af-primary")).toBeEnabled();

    // Back to the repo default: the notice clears, Create is live again, and the
    // submit below sends NO backend — so this create stays local.
    await backendSelect.selectOption("");
    await expect(modal.locator(".af-modal-hint")).toHaveText("");
    await expect(modal.locator("button.af-primary")).toBeEnabled();

    // Title is required; the project picker defaults to the scoped project (redesign
    // PR2 — the first mock repo A/B live in), so the created session lands there and is
    // visible in the scoped rail. Program is left at "Repo default" (claude → the fake
    // agent). Submit with the modal's Create button.
    await modal.locator('input[aria-label="Session title"]').fill(created);
    await modal.locator("button.af-primary").click();

    // The fast path may cross OpCreating between animation frames, but it must still
    // close immediately and settle to the daemon's completed projection.
    await expect(modal).toBeHidden();
    await expect(row(page, created)).toBeVisible({ timeout: 30_000 });

    // The new session is auto-selected AND attached at its AGENT tab (index 0), not
    // the tab-2 we were on: its tab bar has just the agent tab, and its terminal
    // shows the fake agent's ready marker — which it could not if the stream had
    // dialed a ?tab=<n> the session has no tab for.
    await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(1);
    await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
    await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 30_000 });

    // And the new session's stream URL carries no stale tab= selector (the agent tab
    // is the default, sent only for a non-agent tab).
    const lastStream = streamUrls[streamUrls.length - 1];
    expect(lastStream, "a PTY stream WS should have opened for the new session").toBeTruthy();
    expect(lastStream).not.toContain("tab=");

    // Stash it for the kill flow.
    createdTitle = created;
  });

  test("kill: the kill confirm removes the session's row", REAL_FIXTURE, async () => {
    expect(createdTitle).not.toBe("");
    // The created session is the current selection, so its rail row reveals the
    // quiet actions. Kill it and confirm.
    await row(page, createdTitle).click();
    await expect(page.locator(".af-main.af-main-term")).toBeVisible();
    await railAction(page, createdTitle, "Kill session").click();

    const modal = page.locator(".af-modal-card");
    await expect(modal).toBeVisible();
    await modal.locator("button.af-danger").click();

    // The killed row disappears from the rail (the killed event removes it).
    await expect(row(page, createdTitle)).toHaveCount(0, { timeout: 30_000 });
  });
});

test("archive: an unselected row's hover action retires that session, not the selection", REAL_FIXTURE, async () => {
  // Keep A selected, then invoke B's hover-revealed action. The modal target must be
  // the row that owns the button; deriving it from the current selection would archive
  // A instead (#2223).
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await page.mouse.move(0, 0);
  await expect(row(page, SESSION_B).locator(".af-row-actions")).toHaveCSS("opacity", "0");
  await row(page, SESSION_B).hover();
  await expect(row(page, SESSION_B).locator(".af-row-actions")).toHaveCSS("opacity", "1");
  await railAction(page, SESSION_B, "Archive session").click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await expect(modal).toContainText(SESSION_B);
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);
  await expect(row(page, SESSION_B)).not.toHaveClass(/af-row-selected/);
  await modal.locator("button.af-primary").click();

  // B LEAVES the default rail (feat: hide archived by default) — archived sessions are
  // history, and the rail shows the work you can still act on. This is the real
  // archive flow, not a filter toggle: the row goes as the daemon's archived event
  // lands, which is the end-to-end proof the filter reads live projection state.
  await expect(row(page, SESSION_B)).toHaveCount(0, { timeout: 30_000 });

  // B is NOT killed, though — revealing the archive brings the very same row back,
  // muted, carrying the static archive shape (no spinner): the error/terminal states
  // keep distinct static icons (#1766).
  await setFilter(page, "archived", true);
  await expect(row(page, SESSION_B)).toHaveClass(/af-row-archived/, { timeout: 30_000 });
  const archivedDot = row(page, SESSION_B).locator(".af-dot");
  await expect(archivedDot).toHaveClass(/af-dot-archived/);
  await expect(archivedDot.locator('.af-icon[data-icon="archive"]')).toHaveCount(1);
  await expect(archivedDot).not.toHaveClass(/af-dot-spin/);
  // An archived unselected row reserves the same quiet slot, reveals it on hover,
  // and reverses the lifecycle action to Restore rather than Archive.
  await page.mouse.move(0, 0);
  await expect(row(page, SESSION_B).locator(".af-row-actions")).toHaveCSS("opacity", "0");
  await expect(railAction(page, SESSION_B, "Archive session")).toHaveCount(0);
  await expect(
    railAction(page, SESSION_B, "Restore session").locator('.af-icon[data-icon="archive-restore"]'),
  ).toHaveCount(1);
  await row(page, SESSION_B).hover();
  await expect(row(page, SESSION_B).locator(".af-row-actions")).toHaveCSS("opacity", "1");

  await resetFilter(page);
  await expect(row(page, SESSION_B)).toHaveCount(0);
});

test("restore (#1932): the selected rail row's Restore action brings an archived session back", REAL_FIXTURE, async () => {
  // The return leg of archive. The preceding test left B archived; the web could
  // shelve a session but — until #1932 — offered no way back, contradicting the "you
  // can restore it later" copy the archive confirm itself prints. This drives the real
  // round-trip through the daemon AND pins the LIVE button swap: renderMain (which
  // builds the pane header) runs only on a selection change, so archiving/restoring
  // the row that STAYS selected has to flip Archive⇄Restore in place — now in the
  // rail, but still patched by patchMainHead rather than decided only at render time.
  //
  // Hold the archived filter ON for the whole flow so B stays visible in the rail
  // across both transitions; restore the default (archived hidden) only at the end,
  // leaving B archived for the downstream filter tests.
  await setFilter(page, "archived", true);
  await expect(row(page, SESSION_B)).toHaveClass(/af-row-archived/, { timeout: 30_000 });

  await row(page, SESSION_B).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  // An archived session's lifecycle slot IS Restore, not Archive — one selected-row
  // slot whose accessible verb and static glyph both reverse.
  await expect(railAction(page, SESSION_B, "Archive session")).toHaveCount(0);
  await expect(
    railAction(page, SESSION_B, "Restore session").locator('.af-icon[data-icon="archive-restore"]'),
  ).toHaveCount(1);
  await railAction(page, SESSION_B, "Restore session").click();

  // Restore is a confirm (mirroring kill/archive), so it inherits their busy/error
  // surface; the primary button POSTs RestoreSession.
  const restoreModal = page.locator(".af-modal-card");
  await expect(restoreModal).toBeVisible();
  await expect(restoreModal).toContainText("Restore");
  await restoreModal.locator("button.af-primary").click();

  // The daemon's session.restored event resyncs the rail: B rejoins the LIVE group,
  // no longer archived — the end-to-end proof the archived session returned to active.
  await expect(row(page, SESSION_B)).not.toHaveClass(/af-row-archived/, { timeout: 30_000 });
  // ...and its rail row — STILL selected, never reselected — flips its verb back to
  // Archive in place: the patchMainHead path remains load-bearing after the move.
  await expect(railAction(page, SESSION_B, "Restore session")).toHaveCount(0, { timeout: 30_000 });
  await expect(railAction(page, SESSION_B, "Archive session").locator('.af-icon[data-icon="archive"]')).toHaveCount(1);

  // Re-archive from that same live-flipped button (NO reselect): restores the fixture
  // the downstream filter tests need (B archived) and re-proves both the archive leg
  // and the reverse (Archive→Restore) live flip in one flow.
  await railAction(page, SESSION_B, "Archive session").click();
  const archiveModal = page.locator(".af-modal-card");
  await expect(archiveModal).toBeVisible();
  await archiveModal.locator("button.af-primary").click();
  await expect(row(page, SESSION_B)).toHaveClass(/af-row-archived/, { timeout: 30_000 });
  await expect(railAction(page, SESSION_B, "Archive session")).toHaveCount(0);
  await expect(
    railAction(page, SESSION_B, "Restore session").locator('.af-icon[data-icon="archive-restore"]'),
  ).toHaveCount(1);

  // Back to the default filter (archived hidden): B drops from the rail, exactly as
  // the archive test left it.
  await resetFilter(page);
  await expect(row(page, SESSION_B)).toHaveCount(0);
});

// --- the status filter (feat: hide archived by default) --------------------
//
// These run right after the archive flow, which leaves the first project holding both
// live sessions (A, the web probes) and an archived one (B) — the mix the filter
// exists to sort out. They restore the default filter before finishing, so the flows
// that follow see the rail they expect (the filter is persisted, and the page is
// shared).

test("filter (feat): the default shows every state EXCEPT archived", REAL_FIXTURE, async () => {
  // The sane default: archived off, everything else on. Asserted through the menu the
  // user actually reads, so a default that drifts in filter.ts is caught here too.
  await page.locator(".af-rail-filter").click();
  await expect(filterItem(page, "archived")).toHaveAttribute("aria-checked", "false");
  for (const kind of ["working", "ready", "lost", "dead", "limit"]) {
    await expect(filterItem(page, kind), `${kind} must be shown by default`).toHaveAttribute("aria-checked", "true");
  }
  // The default is NOT a "narrowed" state — it must not nag with an indicator the
  // user has to go undo, and its reset is inert.
  await expect(page.locator(".af-rail-filter")).not.toHaveClass(/af-rail-filter-narrowed/);
  await expect(page.locator(".af-filter-reset")).toBeDisabled();
  await page.locator(".af-rail-title").click();

  // Live sessions show; the archived one does not.
  await expect(row(page, SESSION_A)).toBeVisible();
  await expect(row(page, SESSION_B)).toHaveCount(0);
  // The count agrees with the rows on screen — it counts what the rail SHOWS, not
  // what the project holds.
  const shown = await page.locator(".af-rail-list .af-row").count();
  await expect(page.locator(".af-rail-count")).toHaveText(String(shown));
});

test("filter (feat): Show archived reveals the archived row, muted, and hides it again", REAL_FIXTURE, async () => {
  await setFilter(page, "archived", true);
  // The archived row is back and reads as inactive: it reuses the dimmed archived
  // styling rather than looking like live work.
  const archived = row(page, SESSION_B);
  await expect(archived).toBeVisible();
  await expect(archived).toHaveClass(/af-row-archived/);
  expect(
    Number(await archived.evaluate((el) => Number(getComputedStyle(el).opacity))),
    "the archived row must render de-emphasized",
  ).toBeLessThan(1);
  // Revealing the archive IS a departure from the default, so the control says so.
  await expect(page.locator(".af-rail-filter")).toHaveClass(/af-rail-filter-narrowed/);
  await expect(page.locator(".af-rail-filter-dot")).toHaveClass(/af-rail-filter-dot-on/);

  // Unchecking puts it away again — the toggle is symmetric, not one-way.
  await setFilter(page, "archived", false);
  await expect(row(page, SESSION_B)).toHaveCount(0);
  await expect(page.locator(".af-rail-filter")).not.toHaveClass(/af-rail-filter-narrowed/);
});

test("filter (feat): the choice persists across a reload", REAL_FIXTURE, async () => {
  await setFilter(page, "archived", true);
  await expect(row(page, SESSION_B)).toBeVisible();

  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await assertRealRailFixture(page);

  // The archived row is there on load with NO interaction — the choice came back from
  // localStorage, and the menu agrees with what the rail drew.
  await expect(row(page, SESSION_B)).toHaveClass(/af-row-archived/, { timeout: 30_000 });
  await page.locator(".af-rail-filter").click();
  await expect(filterItem(page, "archived")).toHaveAttribute("aria-checked", "true");
  await page.locator(".af-rail-title").click();

  // ...and so does the default, once restored: the persistence is a round trip, not a
  // one-way write that can only ever add rows.
  await resetFilter(page);
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await assertRealRailFixture(page);
  await expect(row(page, SESSION_A)).toBeVisible({ timeout: 30_000 });
  await expect(row(page, SESSION_B)).toHaveCount(0);
});

test("filter (feat): the default hides ONLY archived, and each state's box hides exactly its own group", REAL_FIXTURE, async ({
  browser,
}) => {
  // One row per state, SYNTHETIC and in their own context — the #1898 rule: a real
  // row cannot be pinned to a state (the daemon keeps pushing session.updated deltas
  // that land straight on top of a Snapshot fixture, and the seeded agent's liveness
  // flips LiveRunning→LiveReady a beat after seeding). A synthetic id the daemon has
  // never heard of receives no deltas, so "Ready is unchecked ⇒ the ready row is
  // gone" is deterministic by construction instead of by out-waiting a race. The
  // filter is a pure function of the row's state, so these drive the same code path.
  //
  // The context is its own for a second reason: the filter is PERSISTED, and toggling
  // six states on the shared page would leak into the serial flows that follow.
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
    const list = snap?.instances ?? [];
    // Clone a REAL record so the synthetic rows land in this project's scoped rail,
    // and vary only what the filter reads. Distinct branches keep each row's text
    // free of another's name (row() matches by substring).
    const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
    const synth = (title: string, liveness: number) => ({
      ...proto,
      id: `synth-filter-${title}`,
      title,
      branch: `synth-filter-${title}`,
      liveness,
      in_flight_op: 0,
    });
    list.push(
      synth("filt-working", 1), // LiveRunning
      synth("filt-ready", 2), // LiveReady
      synth("filt-lost", 3), // LiveLost
      synth("filt-dead", 4), // LiveDead
      synth("filt-archived", 5), // LiveArchived
      synth("filt-limit", 6), // LiveLimitReached
    );
    if (snap) {
      snap.instances = list;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await openTokenless(p);

  // Each state's row and the checkbox that governs it, in the menu's own order.
  const states: Array<{ kind: string; title: string }> = [
    { kind: "working", title: "filt-working" },
    { kind: "ready", title: "filt-ready" },
    { kind: "lost", title: "filt-lost" },
    { kind: "dead", title: "filt-dead" },
    { kind: "limit", title: "filt-limit" },
    { kind: "archived", title: "filt-archived" },
  ];

  // THE DEFAULT: every state but archived. Asserted per state against a real row, so
  // a default that hides more than the archive is caught here, not by a user.
  for (const { kind, title } of states) {
    const shown = kind !== "archived";
    await expect(row(p, title), `${title} default visibility`).toHaveCount(shown ? 1 : 0, { timeout: 15_000 });
  }

  // Each box hides exactly its own group: uncheck it, that row goes and EVERY other
  // state stays — then recheck and it comes back.
  for (const { kind, title } of states) {
    if (kind === "archived") {
      continue; // already off by default; its reveal is covered above
    }
    await setFilter(p, kind, false);
    await expect(row(p, title), `${kind} unchecked ⇒ its row goes`).toHaveCount(0);
    for (const other of states) {
      if (other.kind === kind || other.kind === "archived") {
        continue;
      }
      await expect(row(p, other.title), `${kind} unchecked must not disturb ${other.kind}`).toHaveCount(1);
    }
    await setFilter(p, kind, true);
    await expect(row(p, title), `${kind} rechecked ⇒ its row is back`).toHaveCount(1);
  }

  // Narrowing to a SINGLE state is the "show me only what's working" case: uncheck
  // every other state and only that group is left standing.
  for (const { kind } of states) {
    if (kind !== "working") {
      await setFilter(p, kind, false);
    }
  }
  await expect(row(p, "filt-working")).toHaveCount(1);
  await expect(p.locator(".af-rail-list .af-row-archived")).toHaveCount(0);
  for (const { kind, title } of states) {
    if (kind !== "working") {
      await expect(row(p, title), `${title} must be filtered out`).toHaveCount(0);
    }
  }

  await ctx.close();
});

test("#2188: a filtered selected session keeps one visible management surface", REAL_FIXTURE, async ({ browser }) => {
  // Use a separate context because the filter is persisted, but a REAL session: the
  // invariant matters precisely while its terminal keeps streaming. Hiding every
  // live state makes the final absence deterministic even if the agent transitions
  // Ready↔Working while the check is in flight.
  const title = SESSION_A;
  const ctx = await browser.newContext();
  try {
    const p = await ctx.newPage();
    await openTokenless(p);

    await expect(row(p, title)).toBeVisible({ timeout: 15_000 });
    await row(p, title).click();
    await expect(p.locator(".af-term-title")).toHaveText(title);
    await expect(p.locator(".af-main.af-main-term .xterm")).toBeVisible();

    // While the selected row is visible, its rail controls are the one action
    // surface; the pane header must not duplicate them.
    await expect(railAction(p, title, "Archive session")).toBeVisible();
    await expect(railAction(p, title, "Kill session")).toBeVisible();
    await expect(p.locator(".af-term-actions")).toBeHidden();

    // Hide every non-archived state. Selection and the terminal pane intentionally
    // survive this display filter, so management must move with them instead of
    // disappearing with the row.
    for (const kind of ["working", "ready", "lost", "dead", "limit"]) {
      await setFilter(p, kind, false);
    }
    await expect(row(p, title)).toHaveCount(0);
    await expect(p.locator(".af-term-title")).toHaveText(title);
    await expect(p.locator(".af-main.af-main-term .xterm")).toBeVisible();
    const headActions = p.locator(".af-term-actions");
    await expect(headActions).toBeVisible();
    await expect(headActions.getByRole("button", { name: `Archive session “${title}”`, exact: true })).toBeVisible();
    await expect(headActions.getByRole("button", { name: `Kill session “${title}”`, exact: true })).toBeVisible();

    // The fallback is the same action path, not decorative recovery copy: both
    // buttons open the existing target-qualified confirmations. Cancelling keeps the
    // seeded fixture unchanged for every later flow.
    const modal = p.locator(".af-modal-card");
    await headActions.getByRole("button", { name: `Archive session “${title}”`, exact: true }).click();
    await expect(modal).toContainText(`Archive ${title}?`);
    await modal.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(modal).toBeHidden();
    await headActions.getByRole("button", { name: `Kill session “${title}”`, exact: true }).click();
    await expect(modal).toContainText(`Kill ${title}?`);
    await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  } finally {
    await ctx.close();
  }
});

test("filter (feat): keyboard nav walks the VISIBLE rows — j never lands on a hidden one", REAL_FIXTURE, async () => {
  // The rail's j/k order must be the rows on screen. If nav still walked the archived
  // rows the rail hides, j would select something invisible: the pane would swap to a
  // session the user cannot see in the rail, with no row highlighted.
  await setFilter(page, "archived", false);
  await row(page, SESSION_A).click();
  await page.keyboard.press("Escape"); // hand the keyboard back to the rail

  const visited: string[] = [];
  const rows = await page.locator(".af-rail-list .af-row").count();
  for (let i = 0; i < rows + 2; i++) {
    await page.keyboard.press("j");
    const sel = page.locator(".af-row-selected");
    if ((await sel.count()) === 1) {
      visited.push((await sel.locator(".af-row-title").innerText()).trim());
    }
  }
  // j walked the rail and never selected the archived session hidden from it.
  expect(visited.length, "j must move the selection").toBeGreaterThan(0);
  expect(visited, "j must never select a row the filter hides").not.toContain(SESSION_B);
  // Every row it did land on is one the rail is actually drawing.
  for (const title of new Set(visited)) {
    await expect(row(page, title), `${title} must be a visible row`).toBeVisible();
  }
});

test("delete project (#1735, redesign PR2, Fix 2): deleting an archived-only-bound project makes it go away — not a no-op", REAL_FIXTURE, async () => {
  // Use the SECOND project (SESSION_C, no task): switch to it, then delete it from the
  // switcher menu footer. Delete archives its one live session; with no task left, the
  // repo is no longer a project — so it must DISAPPEAR from the switcher (not linger as
  // a stale archived-only entry whose delete is a silent no-op).
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo-2").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-2");
  await expect(row(page, SESSION_C)).toBeVisible();

  // Open the switcher menu and click the reversible Delete-project footer action (it is
  // ENABLED here — there is a live session to archive). The copy makes the reversibility
  // explicit ("restorable").
  await page.locator(".af-project-switch").click();
  const del = page.locator(".af-project-menu .af-project-delete");
  await expect(del).toBeEnabled();
  await del.click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await expect(modal).toContainText("restorable");
  await modal.locator("button.af-danger").click();

  // The project ACTUALLY GOES AWAY: SESSION_C is archived, the repo now has no live
  // session and no task, so it drops from the derivation and selection reconciles to
  // another valid project. Do not hard-code WHICH one: an earlier task flow creates a
  // newer live session in mock-repo-3, so the product's most-recent default can
  // correctly choose either fixture depending on when that event reaches this page.
  const switchName = page.locator(".af-project-switch-name");
  await expect(switchName).not.toHaveText("mock-repo-2", { timeout: 30_000 });
  await page.locator(".af-project-switch").click();
  await expect(projectItem(page, "mock-repo-2")).toHaveCount(0);
  await expect(projectItem(page, "mock-repo")).toHaveCount(1);
  // Downstream real-rail cases consume SESSION_A, so restore their project explicitly
  // instead of handing them whichever valid default won the timing race above.
  await projectItem(page, "mock-repo").click();
  await expect(switchName).toHaveText("mock-repo");
  await expect(page.locator(".af-project-menu")).toBeHidden();

  // Back on the explicitly selected first project, its live rail is intact.
  await expect(row(page, SESSION_A)).toBeVisible();
});

// NOTE on #1675 PR4 (ended PTY → "exited", not a reconnect loop): this is already
// wired end-to-end — the daemon emits a MsgExit control frame on session-end
// (daemon/ws_pty.go, covered by the Go handler tests), and terminal.ts settles to an
// "exited" state + stops reconnecting on it (see onControl's "exit" arm and the
// TerminalStatus="exited" pane header). It is NOT browser-tested here: a real
// mid-stream exit can't be forced without killing the session (which removes the row
// and disposes the terminal before the exit renders), and mocking the per-session WS
// against the loopback HTTP daemon proved unreliable in this harness. The Go side is
// the regression guard for the emit; the client arm is exercised by the daemon's own
// session-end path in manual play-testing.

test("empty state (#1592 PR9): an empty Snapshot renders the empty rail + placeholder", async () => {
  // Force the authoritative Snapshot to come back empty, then reload so bootstrap
  // re-seeds the rail from it. HTTP routing (unlike WS mocking) is deterministic
  // against the loopback daemon, so this reliably drives the zero-sessions state the
  // seeded harness otherwise never reaches. Runs LAST — it reloads and mocks Snapshot,
  // so it must not precede the create/kill/archive flows.
  await page.route("**/v1/Snapshot", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ data: { instances: [] }, error: null }),
    });
  });
  // Empty the task list too (redesign PR2): a task-only project would otherwise keep a
  // project selected, so the GLOBAL no-projects empty state needs both planes empty.
  await page.route("**/v1/ListTasks", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ data: { tasks: [] }, error: null }),
    });
  });
  await page.reload();

  // The authed shell still comes up (tokenless loopback), but the rail shows its
  // empty-state copy instead of rows, and the count reads 0 — the empty state renders
  // as designed rather than a broken/blank shell.
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-rail-empty")).toContainText("No sessions yet");
  await expect(page.locator(".af-rail-count")).toHaveText("0");
  // With nothing selected the main pane is the "Select a session" placeholder.
  await expect(page.locator(".af-main-empty")).toContainText("Select a session");

  await page.unroute("**/v1/Snapshot");
  await page.unroute("**/v1/ListTasks");
});

test("filter (feat): a project whose sessions are ALL archived reads as empty, and the filter still reveals them", REAL_FIXTURE, async ({
  browser,
}) => {
  // An archived-only project is only reachable when something else keeps the repo a
  // project: a scheduled task. projectSummaries makes a repo a project if it has a LIVE
  // session OR a task, so an archived-only repo with no task stops being one and the
  // switcher drops it entirely (the deliberate #1735 rule — a stale archived-only entry
  // whose delete is a silent no-op must never list).
  //
  // So the fixture is a SYNTHETIC repo: archived sessions + a task to keep it a project.
  // Synthetic and in its own context for the #1898 reason — flipping the REAL rows to
  // archived does not hold, because the events plane keeps pushing session.updated for
  // them and applyEvent lands the daemon's truth straight back on top of the fixture
  // (an earlier draft of this test did exactly that, and the rail refilled with live
  // rows before the first assertion). Ids the daemon has never heard of receive no
  // deltas, so the state is the only state there will ever be.
  const ARCHIVED_REPO = "/work/mock-archived-only";
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
    const list = snap?.instances ?? [];
    const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
    const shelved = (title: string) => ({
      ...proto,
      id: `synth-shelved-${title}`,
      title,
      branch: `synth-shelved-${title}`,
      liveness: 5, // LiveArchived
      in_flight_op: 0,
      worktree: { ...(proto.worktree as Record<string, unknown>), repo_path: ARCHIVED_REPO },
    });
    list.push(shelved("shelf-one"), shelved("shelf-two"));
    if (snap) {
      snap.instances = list;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  // The task is what keeps the archived-only repo a project at all.
  await p.route("**/v1/ListTasks", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const data = body?.data as { tasks?: Array<Record<string, unknown>> };
    const tasks = data?.tasks ?? [];
    const proto = { ...(tasks[0] ?? {}) };
    tasks.push({
      ...proto,
      id: "synth-shelved-task",
      name: "shelf-task",
      project_path: ARCHIVED_REPO,
    });
    if (data) {
      data.tasks = tasks;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  // The archived-only repo IS a project (its task keeps it listed) — select it.
  await p.locator(".af-project-switch").click();
  await projectItem(p, "mock-archived-only").click();
  await expect(p.locator(".af-project-switch-name")).toHaveText("mock-archived-only");

  // The rail is NOT a blank panel: it says the project has no active work, and —
  // crucially — accounts for what it is hiding rather than leaving the sessions
  // silently missing.
  const empty = p.locator(".af-rail-empty-project");
  await expect(empty).toBeVisible({ timeout: 15_000 });
  await expect(empty).toContainText("No active sessions in");
  await expect(empty).toContainText("mock-archived-only");
  await expect(empty).toContainText("2 archived hidden");
  await expect(p.locator(".af-rail-list .af-row")).toHaveCount(0);
  await expect(p.locator(".af-rail-count")).toHaveText("0");

  // The hint's inline "Show archived" is a real control, not decoration: it reveals the
  // archive in place, from the very empty state that reported it.
  await empty.locator(".af-rail-show-archived").click();
  await expect(p.locator(".af-rail-list .af-row")).toHaveCount(2);
  await expect(p.locator(".af-rail-count")).toHaveText("2");
  await expect(row(p, "shelf-one")).toHaveClass(/af-row-archived/);
  await expect(row(p, "shelf-two")).toHaveClass(/af-row-archived/);
  // The empty state still stands above them, and no longer claims to be hiding
  // anything: "no active sessions" is a statement about live work, not a claim that
  // the rail is bare.
  await expect(empty).toBeVisible();
  await expect(empty).not.toContainText("archived hidden");

  await ctx.close();
});

// --- theme (redesign PR1): design tokens + light/dark ----------------------
//
// These run LAST: the first reloads with a persisted dark choice (to prove the boot
// stamp beats first paint), and both mutate localStorage + data-theme. They assert on
// the chrome (data-theme + token-driven computed colors), not on session state, so
// the earlier flows are undisturbed. The final test resets to Auto + clears the saved
// choice so the page is left in its default theme.

/** The computed background color of a selector, for a token-driven-color diff. */
async function bgColor(p: Page, selector: string): Promise<string> {
  return p.evaluate((sel) => {
    const node = document.querySelector(sel);
    return node ? getComputedStyle(node).backgroundColor : "";
  }, selector);
}

/** The resolved value of a CSS custom property on :root, for a token-flip diff. */
async function cssVar(p: Page, name: string): Promise<string> {
  return p.evaluate((n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim(), name);
}

test("theme (redesign PR1): a saved dark choice is stamped before the app mounts — no flash", async () => {
  // Persist a dark choice, then install a document-start trap on #app.replaceChildren
  // (how index.ts mounts its content into #app) that records data-theme AT THE EXACT
  // synchronous instant the app first mounts. Because the boot stamp runs at index.ts
  // module top — earlier in the SAME synchronous module turn than mount() — the trap
  // must see data-theme already "dark". This is race-free (no rAF/microtask timing),
  // unlike a paint- or observer-based probe.
  await page.evaluate(() => localStorage.setItem("af-theme", "dark"));
  await page.addInitScript(() => {
    interface ThemeProbe {
      __afMountTheme?: string | null;
    }
    const w = window as unknown as ThemeProbe;
    w.__afMountTheme = "__unset__";
    const orig = Element.prototype.replaceChildren;
    Element.prototype.replaceChildren = function (this: Element, ...args: (Node | string)[]): void {
      if (this.id === "app" && w.__afMountTheme === "__unset__") {
        w.__afMountTheme = document.documentElement.getAttribute("data-theme");
      }
      return orig.apply(this, args);
    };
  });
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();

  const atMount = await page.evaluate(
    () => (window as unknown as { __afMountTheme?: string | null }).__afMountTheme,
  );
  // data-theme was already "dark" the instant the app mounted: no light→dark flash.
  expect(atMount).toBe("dark");
  // And it stuck: <html data-theme="dark"> after the app is up.
  expect(await page.evaluate(() => document.documentElement.getAttribute("data-theme"))).toBe("dark");
  // The dark theme option reads active in the appbar toggle.
  await expect(page.locator('.af-theme-opt[data-theme-opt="dark"]')).toHaveClass(/af-theme-opt-active/);
});

test("theme (redesign PR1): toggling Light vs Dark changes token-driven colors live", async () => {
  // Force Dark and capture a token-driven color (the rail surface).
  await page.locator('.af-theme-opt[data-theme-opt="dark"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  const darkRail = await bgColor(page, ".af-rail");
  const darkBody = await bgColor(page, "body");
  // The web/iframe-pane tokens (fixed in this PR to match the terminal pane) resolve
  // to their dark values.
  const darkTerm = await cssVar(page, "--af-bg-term");
  const darkBorderSubtle = await cssVar(page, "--af-border-subtle");

  // Toggle to Light: the SAME selectors resolve to different token values, proving the
  // chrome is driven by the CSS custom properties, not hardcoded colors.
  await page.locator('.af-theme-opt[data-theme-opt="light"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  const lightRail = await bgColor(page, ".af-rail");
  const lightBody = await bgColor(page, "body");
  const lightTerm = await cssVar(page, "--af-bg-term");
  const lightBorderSubtle = await cssVar(page, "--af-border-subtle");

  expect(lightRail).not.toBe(darkRail);
  expect(lightBody).not.toBe(darkBody);
  // The web-pane canvas + separator tokens flip too, so the embedded web pane reads
  // correctly in both themes (the dark-mode regression this PR fixes).
  expect(lightTerm).not.toBe(darkTerm);
  expect(lightBorderSubtle).not.toBe(darkBorderSubtle);
  // The light rail surface is the white token (#ffffff → rgb(255, 255, 255)).
  expect(lightRail).toBe("rgb(255, 255, 255)");
  await expect(page.locator('.af-theme-opt[data-theme-opt="light"]')).toHaveClass(/af-theme-opt-active/);

  // Reset to Auto and clear the saved choice so the page is left in its default theme.
  // Auto removes data-theme entirely (follow prefers-color-scheme).
  await page.locator('.af-theme-opt[data-theme-opt="auto"]').click();
  await expect
    .poll(() => page.evaluate(() => document.documentElement.hasAttribute("data-theme")))
    .toBe(false);
  await page.evaluate(() => localStorage.removeItem("af-theme"));
});

interface TerminalGeometry {
  hostHeight?: number;
  viewportHeight: number;
  scrollHeight: number;
  scrollTop: number;
  rows: number;
}

async function terminalGeometry(host: Locator, includeHost = false): Promise<TerminalGeometry> {
  return host.evaluate(
    (root, withHost) => {
      const pane = root.querySelector<HTMLElement>(".af-pane-host");
      const viewport = root.querySelector<HTMLElement>(".xterm-viewport");
      return {
        ...(withHost ? { hostHeight: pane?.clientHeight ?? -1 } : {}),
        viewportHeight: viewport?.clientHeight ?? -1,
        scrollHeight: viewport?.scrollHeight ?? -1,
        scrollTop: viewport?.scrollTop ?? -1,
        rows: root.querySelectorAll(".xterm-rows > div").length,
      };
    },
    includeHost,
  );
}

/** Proves the #2347 failure boundary and the user-visible repair against two real
 *  browser subscribers to one PTY. The tall peer deliberately owns a grid larger
 *  than all emitted output, collapsing the first client's scroll range to zero. */
async function assertActiveTerminalReclaimsPeerGeometry(
  first: Page,
  second: Page,
  firstHost: Locator,
  lastLine: string,
  lineCount: number,
  scenario: string,
  expectedFirstRow?: string,
  wheelImmediately = false,
): Promise<void> {
  const firstViewport = firstHost.locator(".xterm-viewport");
  const firstRows = (): Promise<number> => firstHost.locator(".xterm-rows > div").count();

  // On a phone the drawer closes underneath the pointer that selected the row,
  // leaving that pointer over the newly exposed terminal. Move it to inert appbar
  // chrome and cross two paint boundaries so a delayed pointerenter from the drawer
  // transition cannot reassert the tall peer after we return to the first page.
  await second.mouse.move(0, 0);
  await second.evaluate(
    () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      }),
  );
  await expect
    .poll(firstRows, { message: `${scenario}: the authoritative peer resize must reach the first client` })
    .toBeGreaterThan(lineCount);
  const stranded = await terminalGeometry(firstHost, true);
  expect(stranded.rows, `${scenario}: the peer must overwrite the local row count`).toBeGreaterThan(lineCount);
  expect(stranded.scrollHeight, `${scenario}: the oversized grid must remove the local scroll range`).toBe(
    stranded.viewportHeight,
  );

  // The pointer comes back to the first window before the user reaches for the
  // wheel. Its container never changed, so ResizeObserver has nothing to report;
  // pointer activation must reclaim local geometry. Before #2347, rows stayed at
  // the peer's size, every wheel was dead, and only a physical resize fixed them.
  // The tall peer is already the active page after construction. Re-focusing it
  // here would queue another local fit immediately before the first page's fit;
  // that synthetic race would correctly let the peer win last-writer-wins again.
  if (wheelImmediately) {
    // The peer's authoritative grid persists after disconnect. Close only this
    // page so a delayed tall-host activation cannot win again while this branch
    // isolates the first-wheel-vs-deferred-restore ordering.
    await second.close();
  }
  await first.bringToFront();
  if (wheelImmediately) {
    // Target the actual pane with a trusted pointer transition, then send the real
    // wheel immediately—without polling for the deferred anchor frame in between.
    await firstHost.locator(".af-pane-host").hover();
    await first.mouse.wheel(0, -900);
    await expect
      .poll(firstRows, { message: `${scenario}: the first wheel must refit the peer-owned grid` })
      .toBeLessThan(lineCount);
    await first.evaluate(
      () =>
        new Promise<void>((resolve) => {
          requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
        }),
    );
    const repaired = await terminalGeometry(firstHost);
    expect(
      repaired.scrollHeight,
      `${scenario}: the first wheel must recreate scrollback; geometry=${JSON.stringify({ stranded, repaired })}`,
    ).toBeGreaterThan(repaired.viewportHeight);
    expect(
      repaired.scrollTop,
      `${scenario}: deferred restore must not erase the first wheel; geometry=${JSON.stringify({ stranded, repaired })}`,
    ).toBeLessThan(repaired.scrollHeight - repaired.viewportHeight);
    return;
  }
  // Target the actual xterm mount, not its full split-layout wrapper. On narrow
  // layouts the wrapper also spans drawer/scrim stacking surfaces; the production
  // activation listener lives on .af-pane-host.
  await firstHost.locator(".af-pane-host").hover();
  await expect
    .poll(firstRows, { message: `${scenario}: pointer activation must refit; stranded=${JSON.stringify(stranded)}` })
    .toBeLessThan(lineCount);
  const refit = await terminalGeometry(firstHost);
  expect(
    refit.scrollHeight,
    `${scenario}: the local grid must recreate scrollback; geometry=${JSON.stringify({ stranded, refit })}`,
  ).toBeGreaterThan(refit.viewportHeight);
  await expect
    .poll(() => firstViewport.evaluate((el) => el.scrollTop), {
      message: `${scenario}: activation must restore the pre-peer position; geometry=${JSON.stringify({ stranded, refit })}`,
    })
    .toBeGreaterThan(0);
  if (expectedFirstRow !== undefined) {
    await expect(firstHost.locator(".xterm-rows > div").first()).toHaveText(expectedFirstRow);
  } else {
    // A bottom-following viewport must still show the newest output. A deliberately
    // scrolled-up viewport proved that output before activation and must not be
    // yanked down merely so the newest line remains in xterm's rendered DOM.
    await expect(firstHost).toContainText(lastLine);
  }
  const repaired = await terminalGeometry(firstHost);
  await first.mouse.wheel(0, -900);
  await expect
    .poll(() => firstViewport.evaluate((el) => el.scrollTop), {
      message: `${scenario}: wheel must work without a resize; geometry=${JSON.stringify({ stranded, repaired })}`,
    })
    .toBeLessThan(repaired.scrollTop);
}

test("#2347: activating a terminal repairs peer-owned geometry before scrolling", REAL_FIXTURE, async ({
  browser,
}) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const shellName = "peer-fit-2347";

  const firstCtx = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  let secondCtx: BrowserContext | null = null;
  let created = false;
  try {
    const first = await firstCtx.newPage();
    await openTokenless(first);
    await row(first, SESSION_A).click();
    af("sessions", "tab-create", SESSION_A, "--command", "bash", "--name", shellName);
    created = true;
    const firstShell = first.locator(".af-tabbar .af-tab", { hasText: shellName });
    await expect(firstShell).toHaveCount(1, { timeout: 15_000 });
    await firstShell.click();
    await expect(first.locator(".af-main")).toHaveAttribute("data-term-status", "open");
    const firstHost = first.locator(".af-term-host");
    const firstRows = (): Promise<number> => firstHost.locator(".xterm-rows > div").count();
    await expect.poll(firstRows).toBeGreaterThan(24);
    const localRows = await firstRows();
    const localViewport = first.viewportSize();
    expect(localViewport, "desktop peer test needs a fixed local viewport").not.toBeNull();

    await firstHost.click();
    await first.keyboard.type("for i in $(seq 1 60); do echo peer-fit-line-$i; done");
    await first.keyboard.press("Enter");
    await expect(firstHost).toContainText("peer-fit-line-60", { timeout: 15_000 });
    const firstViewport = firstHost.locator(".xterm-viewport");
    await first.mouse.wheel(0, -180);
    await expect
      .poll(async () => {
        const { scrollHeight, viewportHeight, scrollTop } = await terminalGeometry(firstHost);
        return scrollTop > 0 && scrollTop < scrollHeight - viewportHeight;
      }, { message: "desktop shell setup must pause on a real middle scrollback line" })
      .toBe(true);
    const readingLine = (await firstHost.locator(".xterm-rows > div").first().textContent()) ?? "";
    expect(readingLine.trim(), "desktop shell setup must anchor a non-empty visible line").not.toBe("");
    // Leave the pane for the peer window. Returning across this boundary is the
    // pointer-activation path a side-by-side user takes before their first wheel.
    await first.mouse.move(0, 0);

    secondCtx = await browser.newContext({ viewport: { width: 1280, height: 2000 } });
    const second = await secondCtx.newPage();
    await openTokenless(second);
    await row(second, SESSION_A).click();
    const secondShell = second.locator(".af-tabbar .af-tab", { hasText: shellName });
    await expect(secondShell).toHaveCount(1, { timeout: 15_000 });
    await secondShell.click();
    await expect(second.locator(".af-main")).toHaveAttribute("data-term-status", "open");
    const secondHost = second.locator(".af-term-host");
    await expect.poll(firstRows).toBeGreaterThan(60);
    // New output while the first client is inactive must not stale its reading
    // anchor. A one-time distance from the bottom jumps down by these ten lines.
    await secondHost.click();
    await second.keyboard.type("for i in $(seq 61 70); do echo peer-fit-line-$i; done");
    await second.keyboard.press("Enter");
    await expect(firstHost).toContainText("peer-fit-line-70", { timeout: 15_000 });
    await assertActiveTerminalReclaimsPeerGeometry(
      first,
      second,
      firstHost,
      "peer-fit-line-70",
      60,
      "desktop shell",
      readingLine,
    );

    await test.step("a second peer resize already matching the local grid still restores the reading anchor", async () => {
      const secondReadingLine = (await firstHost.locator(".xterm-rows > div").first().textContent()) ?? "";
      expect(secondReadingLine.trim(), "second peer setup must anchor a visible line").not.toBe("");
      await first.mouse.move(0, 0);

      // The tall peer reclaims the shared PTY, so the first client records a fresh
      // pending anchor. Then the SAME peer resizes to the first page's viewport and
      // broadcasts the local grid before the first page becomes active again.
      await second.bringToFront();
      await secondHost.locator(".af-pane-host").hover();
      await expect
        .poll(firstRows, { message: "the tall peer must own the grid before returning it to local size" })
        .toBeGreaterThan(60);
      await second.setViewportSize(localViewport!);
      await expect
        .poll(firstRows, { message: "the peer's second resize must already match the first host's local rows" })
        .toBe(localRows);
      const alreadyLocal = await terminalGeometry(firstHost, true);

      // No FitAddon resize is needed now, but the peer reflows above still displaced
      // the viewport. Activation must consume the pending marker independently of
      // whether `needsFit` is true.
      await first.bringToFront();
      await firstHost.locator(".af-pane-host").hover();
      await first.evaluate(
        () =>
          new Promise<void>((resolve) => {
            requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
          }),
      );
      await expect(
        firstHost.locator(".xterm-rows > div").first(),
        `already-local activation must restore the named reading row; geometry=${JSON.stringify(alreadyLocal)}`,
      ).toHaveText(secondReadingLine);
    });
  } finally {
    await secondCtx?.close();
    await firstCtx.close();
    if (created) {
      try {
        af("sessions", "tab-delete", SESSION_A, "--name", shellName);
      } catch {
        // The throwaway daemon may already have removed a failed startup.
      }
    }
  }
});

test("#2347: the mobile agent terminal also reclaims peer-owned geometry", REAL_FIXTURE, async ({ browser }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const sessionName = "peer-agent-2347";
  const lineCount = 40;
  const lastLine = `peer-agent-line-${lineCount}`;
  af("sessions", "create", "--name", sessionName, "--program", "claude");

  const firstCtx = await browser.newContext({ viewport: { width: 375, height: 667 } });
  let secondCtx: BrowserContext | null = null;
  try {
    const first = await firstCtx.newPage();
    await openTokenless(first);
    await first.locator(".af-nav-toggle").click();
    await expect(row(first, sessionName)).toBeVisible({ timeout: 15_000 });
    await row(first, sessionName).click();
    await expect(first.locator(".af-main")).toHaveAttribute("data-term-status", "open");
    const firstHost = first.locator(".af-term-host");
    const firstRows = (): Promise<number> => firstHost.locator(".xterm-rows > div").count();
    await expect.poll(firstRows).toBeGreaterThan(0);
    await expect.poll(firstRows).toBeLessThan(lineCount);

    // The fixture's `claude` is the real agent-tab PTY path with a deterministic
    // cat-shaped agent. Feed it many lines directly so this test covers the agent
    // terminal without polluting SESSION_A's marker-dependent serial flows. Use
    // real Enter key events: insertText("\\n") writes literal LF bytes and does
    // not exercise the PTY line discipline, so it can paint text without creating
    // the row progression/scrollback that this regression needs.
    await firstHost.click();
    for (let i = 1; i <= lineCount; i += 1) {
      await first.keyboard.type(`peer-agent-line-${i}`);
      await first.keyboard.press("Enter");
    }
    await expect(firstHost).toContainText(lastLine, { timeout: 15_000 });
    const local = await terminalGeometry(firstHost);
    expect(
      local.scrollHeight,
      `mobile agent setup must have scrollback; geometry=${JSON.stringify(local)}`,
    ).toBeGreaterThan(local.viewportHeight);
    expect(
      local.scrollTop,
      `mobile agent setup must start at the bottom; geometry=${JSON.stringify(local)}`,
    ).toBeGreaterThan(0);
    await first.mouse.move(0, 0);

    const peerContext = await browser.newContext({ viewport: { width: 375, height: 2000 } });
    secondCtx = peerContext;
    const openTallPeer = async (label: string): Promise<Page> => {
      const peer = await peerContext.newPage();
      await openTokenless(peer);
      await peer.locator(".af-nav-toggle").click();
      await expect(row(peer, sessionName)).toBeVisible({ timeout: 15_000 });
      await row(peer, sessionName).click();
      await expect(peer.locator(".af-main")).toHaveAttribute("data-term-status", "open");
      await expect.poll(firstRows, { message: label }).toBeGreaterThan(lineCount);
      const stranded = await terminalGeometry(firstHost, true);
      expect(stranded.rows, `${label}; peer geometry=${JSON.stringify(stranded)}`).toBeGreaterThan(lineCount);
      expect(stranded.scrollHeight, `${label}; peer geometry=${JSON.stringify(stranded)}`).toBe(
        stranded.viewportHeight,
      );
      return peer;
    };
    const expectSettledLocalGeometry = async (label: string): Promise<void> => {
      let observed = await terminalGeometry(firstHost, true);
      try {
        await expect
          .poll(
            async () => {
              observed = await terminalGeometry(firstHost, true);
              return observed.rows < lineCount && observed.scrollHeight > observed.viewportHeight;
            },
            { message: label },
          )
          .toBe(true);
      } catch (error) {
        const detail = error instanceof Error ? error.message : String(error);
        throw new Error(`${label}; last geometry=${JSON.stringify(observed)}\n${detail}`);
      }
    };

    // Each tall peer leaves its authoritative grid behind, then disconnects. That
    // isolates the local trigger under test: an activation still queued in a live
    // peer is a legitimate later writer, not evidence that this host failed to fit.
    // Start with #2354's explicit drawer transition. dispatchEvent deliberately does
    // not focus or move a pointer into this page, so the only possible repair is the
    // app shell's layout-change hook — not #2347's focus/pointer fallbacks.
    let peer = await openTallPeer("the tall peer must strand the mobile client before the drawer check");
    await peer.close();
    await test.step("drawer visibility transitions explicitly refit the active pane", async () => {
      const app = first.locator(".af-app");
      const toggle = first.locator(".af-nav-toggle");
      await toggle.dispatchEvent("click");
      await expect(app).toHaveClass(/af-nav-open/);
      await expectSettledLocalGeometry("opening the overlay must reclaim peer-owned terminal geometry");
      await toggle.dispatchEvent("click");
      await expect(app).not.toHaveClass(/af-nav-open/);
    });

    // Exercise the real xterm DOM listeners while the stale peer grid remains.
    peer = await openTallPeer("the tall peer must strand the mobile client before the touch check");
    await peer.close();
    await test.step("touch scroll input reclaims the peer grid", async () => {
      await firstHost.locator(".af-pane-host").dispatchEvent("touchmove", {
        bubbles: true,
        cancelable: true,
        touches: [{ identifier: 1, clientX: 180, clientY: 300 }],
        changedTouches: [{ identifier: 1, clientX: 180, clientY: 300 }],
      });
      await expectSettledLocalGeometry("touchmove must reclaim the measurable local grid and rebuild scrollback");
    });

    peer = await openTallPeer("the next tall peer must strand the mobile client before the scrollbar check");
    await peer.close();
    await test.step("scrollbar input reclaims the peer grid", async () => {
      await firstHost.locator(".xterm-viewport").dispatchEvent("pointerdown", {
        bubbles: true,
        cancelable: true,
        pointerType: "mouse",
        button: 0,
        buttons: 1,
      });
      await expectSettledLocalGeometry("a pointer on xterm's scrollbar viewport must reclaim the local grid");
    });

    peer = await openTallPeer("the final tall peer must strand the mobile client before the immediate-wheel check");

    await assertActiveTerminalReclaimsPeerGeometry(
      first,
      peer,
      firstHost,
      lastLine,
      lineCount,
      "mobile agent",
      undefined,
      true,
    );
  } finally {
    await secondCtx?.close();
    await firstCtx.close();
    try {
      af("sessions", "kill", sessionName);
    } catch {
      // The throwaway daemon may already have removed a failed startup.
    }
  }
});

// The #1812 regression guard: a tab created or deleted by ANY actor other than
// this browser window — an agent running `af sessions tab-create` inside its own
// session, the TUI, a script, a second window — must reach an already-open client
// live. Before the fix the daemon persisted the roster silently, so the SPA (which
// only re-Snapshots after its OWN mutation, and never polls) stayed stale until the
// user reloaded or an unrelated session.updated happened to repair it as a
// side-effect. On a quiet session that repair never comes — which broke the web
// tab's stated purpose: letting an agent inject a live browser view into the user's
// screen.
//
// The mutation is driven through the real `af` CLI over the control socket, not the
// HTTP API the SPA uses, so this also proves the event reaches the browser
// regardless of which transport drove the change (both mutate one Manager).
test("#1812: a tab created/deleted out-of-band reaches the open client with no reload", REAL_FIXTURE, async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const OOB_TAB = "livepreview";

  await row(page, SESSION_A).click();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  const oob = tabbar.locator(".af-tab", { hasText: OOB_TAB });
  await expect(oob).toHaveCount(0);

  // Stamp the live document. A reload wipes this, so asserting it survives is what
  // makes "WITHOUT a reload" a real claim rather than an assumption — the whole
  // point of #1812 is that the user must not have to refresh.
  await page.evaluate(() => {
    (window as Window & { __af1812?: boolean }).__af1812 = true;
  });
  const notReloaded = (): Promise<boolean> =>
    page.evaluate(() => (window as Window & { __af1812?: boolean }).__af1812 === true);

  // Repro A: an agent injects a live browser view mid-work. The tab must land in
  // this window, which did not create it.
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--port", "3200", "--name", OOB_TAB);
  await expect(oob, "a tab created out-of-band must appear without a reload").toHaveCount(1, {
    timeout: 15_000,
  });
  expect(await notReloaded(), "the tab must arrive on the LIVE page, not via a reload").toBe(true);

  // The delete side: the tab must vanish from a window that did not close it.
  af("sessions", "tab-delete", SESSION_A, "--name", OOB_TAB);
  await expect(oob, "a tab deleted out-of-band must disappear without a reload").toHaveCount(0, {
    timeout: 15_000,
  });
  expect(await notReloaded()).toBe(true);
});

test("#2330: an older reconnect Snapshot cannot overwrite a newer session event", REAL_FIXTURE, async ({ browser }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, "sessions", ...args], { stdio: "pipe" });
  };

  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  const tabName = "resync-race-2330";
  let created = false;
  let snapshotCalls = 0;
  let releaseStale = (): void => {};
  const staleGate = new Promise<void>((resolve) => {
    releaseStale = resolve;
  });
  let staleCaptured = (): void => {};
  const staleStarted = new Promise<void>((resolve) => {
    staleCaptured = resolve;
  });
  let staleResponseCaptured = false;
  let staleSettled = (): void => {};
  const staleFinished = new Promise<void>((resolve) => {
    staleSettled = resolve;
  });

  try {
    await p.route("**/v1/Snapshot", async (route) => {
      const call = ++snapshotCalls;
      try {
        const response = await route.fetch();
        const body = await response.body();
        if (call === 2) {
          staleResponseCaptured = true;
          staleCaptured();
          await staleGate;
        }
        await route.fulfill({ status: response.status(), headers: response.headers(), body });
      } finally {
        if (call === 2) {
          staleSettled();
        }
      }
    });

    // Install the route BEFORE first navigation: connect performs one bootstrap
    // Snapshot, then the newly-open events socket asks for an authoritative resync.
    // Hold that SECOND response after the daemon has already produced it, making it
    // provably older than the event below. There is no prior page whose delayed
    // first-open resync can steal either call number (Codex review P2).
    await openTokenless(p);
    await row(p, SESSION_A).click();
    await expect(p.locator(".af-tabbar")).toBeVisible();
    await staleStarted;

    af("tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", tabName);
    created = true;
    const liveTab = p.locator(".af-tabbar .af-tab", { hasText: tabName });
    await expect(liveTab, "the real session.updated event lands while the older Snapshot is held").toHaveCount(1);

    releaseStale();
    await expect
      .poll(() => snapshotCalls, {
        message: "discarding a Snapshot crossed by an event schedules a fresh authoritative resync",
        timeout: 5_000,
      })
      .toBeGreaterThanOrEqual(3);
    await expect(liveTab, "the older Snapshot must not rewind the newer event").toHaveCount(1);
  } finally {
    releaseStale();
    if (staleResponseCaptured) {
      await staleFinished;
    }
    await p.unroute("**/v1/Snapshot");
    if (created) {
      af("tab-delete", SESSION_A, "--name", tabName);
    }
    await ctx.close();
  }
});

// The other half of #1812/#1815's bill: the event must arrive WITHOUT disturbing
// what the user is doing. Delivering a tab into the open window is only half the
// feature if it rips the pane they are reading out from under them.
//
// The regression: reconcile re-inserted the split DOM on every resync, and
// re-inserting a pane container detaches it — even when it goes straight back into
// the same parent. The browser drops the scroll offset of every scrollable
// descendant on detach, so the xterm viewport rewound to 0 while xterm's own scroll
// position (ydisp) stayed put. That desync is what killed the WHEEL: a viewport
// pinned at 0 has nothing left to scroll up, emits no scroll event, and xterm never
// moves. It looked like "scrolling is broken", and it healed on the next chunk of
// output (which resyncs scrollTop) — so it bit hardest on a QUIET pane, which is
// exactly the pane someone is scrolled up reading.
//
// Driven through the real `af` CLI, like the #1812 test above: a tab created by
// someone else, on the session being watched, while its scrollback is parked.
test("#1815: a tab created out-of-band must not rewind the scrolled terminal", REAL_FIXTURE, async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const SCROLL_TAB = "scrollprobe";
  const SHELL_TAB = "scrollshell";

  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  // Scrollback needs a real PTY: the seeded agent tab is a fake agent that only
  // echoes. Create a shell tab under a name of our own rather than clicking + — the
  // suite is serial and "Terminal" is the default name every other tab test uses,
  // so a leaked one would make this ambiguous.
  const tabbar = page.locator(".af-tabbar");
  // Whatever this serial suite has left on the session — restored at the end, since
  // the tests after this one inherit it.
  const tabsBefore = await tabbar.locator(".af-tab").count();
  af("sessions", "tab-create", SESSION_A, "--command", "bash", "--name", SHELL_TAB);
  const shell = tabbar.locator(".af-tab", { hasText: SHELL_TAB });
  await expect(shell).toHaveCount(1, { timeout: 15_000 });
  await shell.click();
  await expect(shell).toHaveClass(/af-tab-active/);
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");

  // More than one screen of numbered output, so which line is on screen is exact.
  // xterm renders only the VISIBLE rows, so the pane's text IS its viewport.
  const host = page.locator(".af-term-host");
  await page.keyboard.type("for i in $(seq 1 200); do echo scrollback-line-$i; done");
  await page.keyboard.press("Enter");
  await expect(host).toContainText("scrollback-line-200", { timeout: 15_000 });

  // Park the view up in the scrollback, the way a user reads back through output.
  await host.hover();
  await page.mouse.wheel(0, -900);
  await expect(host, "the wheel must move the viewport off the bottom").not.toContainText(
    "scrollback-line-200",
  );
  const viewport = page.locator(".af-term-host .xterm-viewport");
  const parked = await viewport.evaluate((el) => el.scrollTop);
  expect(parked, "a scrolled-up viewport sits off 0").toBeGreaterThan(0);
  const reading = await host.textContent();

  // Someone else adds a tab to the session being watched. #1815 fans the
  // session.updated out to this window, which re-projects the whole session.
  const probe = page.locator(".af-tabbar .af-tab", { hasText: SCROLL_TAB });
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--port", "3201", "--name", SCROLL_TAB);
  // The event really landed here — otherwise the assertions below would pass on a
  // client that never re-projected, proving nothing.
  await expect(probe, "the out-of-band tab must reach this window").toHaveCount(1, {
    timeout: 15_000,
  });

  // The pane is untouched: same shell tab, same line, same viewport offset.
  await expect(host).not.toContainText("scrollback-line-200");
  expect(await viewport.evaluate((el) => el.scrollTop), "the viewport must not rewind").toBe(parked);
  expect(await host.textContent(), "the parked view must not move").toBe(reading);

  // ...and the wheel still WORKS afterwards, which is the symptom that was reported:
  // the rewound viewport had nothing left to scroll, so wheel-up went dead.
  await page.mouse.wheel(0, -300);
  await expect
    .poll(async () => viewport.evaluate((el) => el.scrollTop), {
      message: "wheel-up must still scroll after an out-of-band resync",
    })
    .toBeLessThan(parked);

  // Put the roster back: this suite is serial, so both tabs would otherwise leak
  // into every test after this one.
  af("sessions", "tab-delete", SESSION_A, "--name", SCROLL_TAB);
  await expect(probe).toHaveCount(0, { timeout: 15_000 });
  af("sessions", "tab-delete", SESSION_A, "--name", SHELL_TAB);
  await expect(shell).toHaveCount(0, { timeout: 15_000 });
  // The roster this test perturbed is back as it found it. Which tab the pane FALLS
  // BACK to once its own tab is deleted out from under it is a separate contract
  // (the tab tests above own it), so this stops at the roster — but the suite is
  // serial, so park the pane back on the agent tab rather than leave the next test
  // to inherit whatever the fallback picked.
  await expect(tabbar.locator(".af-tab")).toHaveCount(tabsBefore, { timeout: 15_000 });
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
});

// The same rewind, one gesture later (local Codex review of #1894).
//
// Resizing a divider persists through setRatio, which rebuilds every SplitNode —
// so the root comes back as a NEW object even though the drag already applied the
// ratios to the live DOM and nothing structural moved. That left builtTree pointing
// at the superseded root, and the next roster resync saw tree !== builtTree, took
// the rebuild branch, and rewound the scrolled terminal exactly as before. The
// primary fix held right up until the user touched a divider.
//
// The resize ITSELF is allowed to move the viewport (a narrower pane reflows, and
// xterm rewraps), so the baseline is re-read once the resize has settled. What must
// not move it is the RESYNC that follows.
test("#1815: a resize must not re-arm the rewind on the next out-of-band resync", REAL_FIXTURE, async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const SHELL_TAB = "resizeshell";
  const PROBE_TAB = "resizeprobe";

  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  // Park on the agent tab explicitly: layouts are retained per session, so this
  // pane shows whatever the last test left it on, not necessarily the agent.
  const tabbar = page.locator(".af-tabbar");
  const tabsBefore = await tabbar.locator(".af-tab").count();
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });

  // A shell tab to scroll, and a split so there IS a divider to drag.
  af("sessions", "tab-create", SESSION_A, "--command", "bash", "--name", SHELL_TAB);
  const shellTab = tabbar.locator(".af-tab", { hasText: SHELL_TAB });
  await expect(shellTab).toHaveCount(1, { timeout: 15_000 });
  await shellTab.click();
  await expect(shellTab).toHaveClass(/af-tab-active/);
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");

  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
  const shellPane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await expect(shellPane).toHaveCount(1);

  // Fill the shell pane's scrollback and park the view up in it.
  await shellPane.locator(".af-pane-host").click();
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  await page.keyboard.type("for i in $(seq 1 200); do echo resize-line-$i; done");
  await page.keyboard.press("Enter");
  await expect(shellPane).toContainText("resize-line-200", { timeout: 15_000 });
  await shellPane.hover();
  await page.mouse.wheel(0, -900);
  await expect(shellPane).not.toContainText("resize-line-200");

  // Drag the divider — real pointer events, since it captures the pointer.
  const divider = page.locator(".af-term-host .af-divider").first();
  const box = await divider.boundingBox();
  expect(box, "a split must have a divider to drag").toBeTruthy();
  const bx = (box as { x: number; width: number }).x + (box as { width: number }).width / 2;
  const by = (box as { y: number; height: number }).y + (box as { height: number }).height / 2;
  await page.mouse.move(bx, by);
  await page.mouse.down();
  await page.mouse.move(bx - 80, by, { steps: 8 });
  await page.mouse.up();

  // Let the resize settle (the fit is debounced) and take the post-resize baseline:
  // the reflow above is legitimate, the resync below is not.
  const viewport = shellPane.locator(".xterm-viewport");
  await expect
    .poll(async () => viewport.evaluate((el) => el.scrollTop), {
      message: "the resized pane must still be parked off the bottom",
      timeout: 10_000,
    })
    .toBeGreaterThan(0);
  await page.waitForTimeout(500);
  const parked = await viewport.evaluate((el) => el.scrollTop);
  const reading = await shellPane.textContent();

  // The resync that used to rewind it: someone else adds a tab.
  const probe = tabbar.locator(".af-tab", { hasText: PROBE_TAB });
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--port", "3202", "--name", PROBE_TAB);
  await expect(probe, "the out-of-band tab must reach this window").toHaveCount(1, {
    timeout: 15_000,
  });

  expect(
    await viewport.evaluate((el) => el.scrollTop),
    "a resync after a resize must not rewind the viewport",
  ).toBe(parked);
  expect(await shellPane.textContent(), "the parked view must not move").toBe(reading);

  // Put it all back for the serial tests after this one.
  af("sessions", "tab-delete", SESSION_A, "--name", PROBE_TAB);
  await expect(probe).toHaveCount(0, { timeout: 15_000 });
  await page
    .locator(".af-term-host .af-pane", { hasText: READY_MARKER })
    .locator(".af-pane-close")
    .click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  af("sessions", "tab-delete", SESSION_A, "--name", SHELL_TAB);
  await expect(shellTab).toHaveCount(0, { timeout: 15_000 });
  await expect(tabbar.locator(".af-tab")).toHaveCount(tabsBefore, { timeout: 15_000 });
});

// The #1812 review guard (Codex): a close is async, and `next` is computed from the
// index that was active when it was ISSUED. A slow CloseTab leaves a window in which
// the user can select another tab — and re-pointing the pane from that stale index
// afterwards would yank their selection away. The user's newer intent must win. The
// roster event races the same window and must still be free to pass the guard, which
// is why it keys off explicit layout mutations rather than the tab index itself.
test("#1812 review: a close held in flight must not clobber a tab the user picks meanwhile", REAL_FIXTURE, async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };

  await row(page, SESSION_A).click();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  const active = page.locator(".af-tab.af-tab-active .af-tab-label");

  // Two named tabs: "doomed" is closed, "keeper" is focused when the close is issued
  // (so the stale `next` would point back at it). Named via the CLI so neither the
  // labels nor the ordinals depend on what earlier tests left behind.
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", "doomed");
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", "keeper");
  const doomed = tabbar.locator(".af-tab", { hasText: "doomed" });
  const keeper = tabbar.locator(".af-tab", { hasText: "keeper" });
  await expect(doomed).toHaveCount(1, { timeout: 15_000 });
  await expect(keeper).toHaveCount(1, { timeout: 15_000 });

  await keeper.click();
  await expect(active).toHaveText("keeper");

  // Hold CloseTab open so the mid-flight window is deterministic rather than a race
  // against a fast local daemon.
  let releaseClose: (() => void) | undefined;
  const closeHeld = new Promise<void>((resolve) => {
    releaseClose = resolve;
  });
  let closeInFlight = false;
  await page.unroute("**/v1/CloseTab");
  await page.route("**/v1/CloseTab", async (route) => {
    closeInFlight = true;
    await closeHeld;
    await route.continue();
  });

  await doomed.locator(".af-tab-close").click();
  await expect.poll(() => closeInFlight, { timeout: 15_000 }).toBe(true);

  // Mid-flight: the user picks the agent tab. This is the intent the close must respect.
  await tabbar.locator(".af-tab", { hasText: "Agent" }).first().click();
  await expect(active).toHaveText("Agent");

  // Let the close land. Its `next` was computed against "keeper" — applying it now
  // would snap the pane back to "keeper" and lose the user's choice.
  releaseClose?.();
  await expect(doomed).toHaveCount(0, { timeout: 15_000 });
  await expect(active, "the user's mid-flight selection must survive the close").toHaveText("Agent");

  await page.unroute("**/v1/CloseTab");
  af("sessions", "tab-delete", SESSION_A, "--name", "keeper");
  await expect(keeper).toHaveCount(0, { timeout: 15_000 });
});

// The post-merge Codex findings on #1815. All three are the same shape: #1815 made
// out-of-band roster changes reach an open client LIVE, which turned three latent
// ordinal-vs-identity assumptions into reachable misroutes. Each drives the exact
// scenario Codex described and asserts on the tab the user is actually looking at.

test("#1815 review: a concurrent out-of-band close cannot re-point the pane to a neighbour", REAL_FIXTURE, async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, "sessions", ...args], { stdio: "pipe" });
  };

  await row(page, SESSION_A).click();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  const active = page.locator(".af-tab.af-tab-active .af-tab-label");
  // Earlier flows leave their own tabs on this session, so every count here is
  // relative to what is already on the bar rather than an absolute the next test to
  // leave a tab behind would break.
  const baseline = await tabbar.locator(".af-tab").count();

  // Codex's scenario: this window closes t3 while ANOTHER client closes the LOWER t1
  // first. The old code re-pointed the pane by subtracting its own close's shift from
  // the pre-close ordinal — but t1's removal had already shifted t4 down underneath
  // it, so the result named t5, the neighbour, and the pane the user was reading was
  // torn down to show the wrong tab.
  // One at a time, each awaited: the events hub DROPS a publish for a subscriber that
  // is behind rather than blocking the daemon (clients re-Snapshot to resync), so a
  // burst of CLI mutations can outrun the browser and lose a roster. Real actors don't
  // burst, and this test is about ordering, not backpressure.
  for (const name of ["t1", "t2", "t3", "t4", "t5"]) {
    af("tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(1, { timeout: 15_000 });
  }

  // Click the LABEL, not the button. A bar this crowded shrinks each tab until the
  // button's CENTRE — where a bare .click() lands — is its × rather than its label,
  // so clicking the button would close the tab instead of selecting it. The label's
  // click still bubbles to the button's handler, which is the real select path.
  await tabbar.locator(".af-tab", { hasText: "t4" }).locator(".af-tab-label").click();
  await expect(active).toHaveText("t4");

  let releaseClose: (() => void) | undefined;
  const closeHeld = new Promise<void>((resolve) => {
    releaseClose = resolve;
  });
  let closeInFlight = false;
  await page.unroute("**/v1/CloseTab");
  await page.route("**/v1/CloseTab", async (route) => {
    closeInFlight = true;
    await closeHeld;
    await route.continue();
  });

  await tabbar.locator(".af-tab", { hasText: "t3" }).locator(".af-tab-close").click();
  await expect.poll(() => closeInFlight, { timeout: 15_000 }).toBe(true);

  // The other client closes a LOWER tab mid-flight. Waiting for it to leave the bar
  // proves the #1815 event landed and the roster shifted BEFORE the close resolves —
  // which is the whole point: this is the race #1815 itself made reachable.
  af("tab-delete", SESSION_A, "--name", "t1");
  await expect(tabbar.locator(".af-tab", { hasText: "t1" })).toHaveCount(0, { timeout: 15_000 });

  releaseClose?.();
  await expect(tabbar.locator(".af-tab", { hasText: "t3" })).toHaveCount(0, { timeout: 15_000 });
  await expect(active, "the pane must still show the tab the user was on, not its neighbour").toHaveText("t4");

  await page.unroute("**/v1/CloseTab");
  for (const name of ["t2", "t4", "t5"]) {
    af("tab-delete", SESSION_A, "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(0, { timeout: 15_000 });
  }
  await expect(tabbar.locator(".af-tab")).toHaveCount(baseline, { timeout: 15_000 });
});

test("#1815 review: a pane focused mid-close keeps its own tab", REAL_FIXTURE, async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, "sessions", ...args], { stdio: "pipe" });
  };

  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const active = page.locator(".af-tab.af-tab-active .af-tab-label");

  // The guard keyed off layoutGeneration, which counted tab rebinds and splits but
  // NOT a change of which PANE holds focus — and the guarded write (setFocusedTab)
  // targets the focused pane. So a close held in flight could rebind the pane the
  // user had just clicked, using an index computed for the pane that issued it.
  // PROCESS tabs, not web tabs: this test has to move focus BETWEEN panes by clicking
  // them, and a web tab's pane is an iframe that swallows the mousedown the pane's
  // focus handler listens for. A PTY pane is the surface a user actually clicks
  // between, which is the gesture the guard has to survive.
  const baseline = await tabbar.locator(".af-tab").count();
  for (const name of ["k1", "k2"]) {
    af("tab-create", SESSION_A, "--command", "sleep 300", "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(1, { timeout: 30_000 });
  }

  await tabbar.locator(".af-tab", { hasText: "k2" }).locator(".af-tab-label").click();
  await expect(active).toHaveText("k2");

  // Split, so there are two panes and "which pane is focused" is a real question. The
  // agent pane is the one streaming the ready marker; the other is k2's.
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
  const agentPane = page.locator(".af-term-host .af-pane", { hasText: READY_MARKER });
  const k2Pane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await expect(agentPane).toHaveCount(1);
  await expect(k2Pane).toHaveCount(1);

  // Focus the k2 pane — this is the pane the close is issued from, and the one whose
  // index the close's `next` is computed against.
  await k2Pane.locator(".af-pane-host").click();
  await expect(active).toHaveText("k2");

  let releaseClose: (() => void) | undefined;
  const closeHeld = new Promise<void>((resolve) => {
    releaseClose = resolve;
  });
  let closeInFlight = false;
  await page.unroute("**/v1/CloseTab");
  await page.route("**/v1/CloseTab", async (route) => {
    closeInFlight = true;
    await closeHeld;
    await route.continue();
  });

  await tabbar.locator(".af-tab", { hasText: "k1" }).locator(".af-tab-close").click();
  await expect.poll(() => closeInFlight, { timeout: 15_000 }).toBe(true);

  // Mid-flight the user clicks the OTHER pane. That is a deliberate move of focus, and
  // the close's stale index means nothing for the pane they just chose.
  await agentPane.locator(".af-pane-host").click();
  await expect(active).toHaveText("Agent");

  releaseClose?.();
  await expect(tabbar.locator(".af-tab", { hasText: "k1" })).toHaveCount(0, { timeout: 15_000 });
  await expect(active, "the pane the user focused mid-close must keep its own tab").toHaveText("Agent");

  // Collapse the split and restore A's single-tab baseline for the later flows.
  await page.unroute("**/v1/CloseTab");
  await agentPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  af("tab-delete", SESSION_A, "--name", "k2");
  await expect(tabbar.locator(".af-tab")).toHaveCount(baseline, { timeout: 15_000 });
});

test("#1815 review: a retained layout follows its tab when the roster changes while away", REAL_FIXTURE, async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, "sessions", ...args], { stdio: "pipe" });
  };

  const tabbar = page.locator(".af-tabbar");
  const active = page.locator(".af-tab.af-tab-active .af-tab-label");

  // The split view retains a layout per session, but only ever remapped the SHOWN
  // session's panes onto a changed roster; a session the user had navigated away from
  // was merely clamped on return. Its leaves hold ORDINALS, so an out-of-band close
  // while away — which #1815 now delivers live — silently rebound them to whatever
  // slid into the slot. Tabs [Agent,r1,r2,r3] with the pane on r2 (index 2): drop r1
  // and the stale ordinal 2 is r3, an entirely different tab.
  await row(page, SESSION_A).click();
  await expect(tabbar).toBeVisible();
  const baseline = await tabbar.locator(".af-tab").count();
  for (const name of ["r1", "r2", "r3"]) {
    af("tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(1, { timeout: 15_000 });
  }
  await tabbar.locator(".af-tab", { hasText: "r2" }).locator(".af-tab-label").click();
  await expect(active).toHaveText("r2");

  // Navigate away, so A's layout is only RETAINED — not the live one being reconciled.
  // B is archived by the time this runs, so reveal the archive to reach it (feat: hide
  // archived by default). B stays the right target for exactly the reason it always
  // was: an agent-tab-only session no other test has moved off its pane.
  await setFilter(page, "archived", true);
  await row(page, SESSION_B).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await resetFilter(page);

  af("tab-delete", SESSION_A, "--name", "r1");

  // Back to A: its pane must still be on r2, which is now index 1.
  await row(page, SESSION_A).click();
  await expect(tabbar.locator(".af-tab", { hasText: "r1" })).toHaveCount(0, { timeout: 15_000 });
  await expect(active, "a retained pane must follow its TAB across an out-of-band close, not its slot").toHaveText(
    "r2",
  );

  for (const name of ["r2", "r3"]) {
    af("tab-delete", SESSION_A, "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(0, { timeout: 15_000 });
  }
  await expect(tabbar.locator(".af-tab")).toHaveCount(baseline, { timeout: 15_000 });
});

// --- PWA: favicon, theme-color, manifest, service worker, install (feat) --------
//
// These run in their OWN context rather than on the shared serial `page`, because a
// service worker and a localStorage dismissal are per-origin state that would leak
// into every test after them. A fresh context each time also means each test starts
// from "never registered, never dismissed", which is the state a real first visit has.
//
// Worth stating plainly: the SPA registers the worker on EVERY load, so every other
// test in this file already runs with it active. The suite staying green is itself
// the broadest evidence the worker doesn't disturb the app — these tests are the
// specific, deliberate proofs on top of that.

/**
 * Opens the app in a fresh page and returns once a service worker is not merely
 * registered but actually CONTROLLING it — the state in which it could do damage, and
 * therefore the only state worth asserting a bypass against.
 *
 * The reload is not defensive padding, it is the point. On a FIRST visit the shell
 * (page, bundle, CSS) is fetched before the worker exists, so those requests never
 * pass through it and nothing is cached; the worker only takes over via clients.claim
 * once everything is already on screen. It is the SECOND load whose requests actually
 * reach the fetch handler. So this reloads to reach the steady state a returning user
 * is in, which is the state worth testing — a first-load-only check would assert the
 * worker does nothing, which is true and useless.
 */
async function openControlledByWorker(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
  await page.evaluate(async () => {
    await navigator.serviceWorker.ready;
    if (navigator.serviceWorker.controller) {
      return;
    }
    // clients.claim() takes control of an already-open page asynchronously, so the
    // controller may land just after `ready`.
    await new Promise<void>((resolve) => {
      navigator.serviceWorker.addEventListener("controllerchange", () => resolve(), { once: true });
    });
  });
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
}

/** Every URL currently held in every Cache Storage bucket, with the bucket names. */
async function cacheContents(page: Page): Promise<{ names: string[]; urls: string[] }> {
  return page.evaluate(async () => {
    const names = await caches.keys();
    const urls: string[] = [];
    for (const name of names) {
      const cache = await caches.open(name);
      for (const request of await cache.keys()) {
        urls.push(request.url);
      }
    }
    return { names, urls };
  });
}

test("the shell links an SVG favicon, PNG fallbacks, an apple-touch icon, and the manifest", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");

  await expect(p.locator('link[rel="icon"][type="image/svg+xml"]')).toHaveAttribute("href", "/icons/icon.svg");
  await expect(p.locator('link[rel="icon"][sizes="32x32"]')).toHaveAttribute("href", "/icons/favicon-32.png");
  await expect(p.locator('link[rel="icon"][sizes="16x16"]')).toHaveAttribute("href", "/icons/favicon-16.png");
  await expect(p.locator('link[rel="apple-touch-icon"]')).toHaveAttribute("href", "/icons/apple-touch-icon-180.png");
  await expect(p.locator('link[rel="manifest"]')).toHaveAttribute("href", "/manifest.webmanifest");

  // Linking them is half the claim; the daemon actually serving them is the other
  // half, and a link to a 404 looks identical in the DOM.
  const results = await p.evaluate(async () => {
    const hrefs = [...document.querySelectorAll('link[rel="icon"], link[rel="apple-touch-icon"]')].map(
      (l) => (l as HTMLLinkElement).getAttribute("href") ?? "",
    );
    return Promise.all(
      hrefs.map(async (href) => {
        const res = await fetch(href);
        return { href, status: res.status, type: res.headers.get("content-type") ?? "" };
      }),
    );
  });
  expect(results.length).toBeGreaterThanOrEqual(4);
  for (const r of results) {
    expect(r.status, `${r.href} must be served`).toBe(200);
    expect(r.type, `${r.href} must carry an image type`).toMatch(/^image\/(png|svg\+xml)/);
  }
  await ctx.close();
});

test("theme-color is declared per scheme, and an explicit theme choice repoints the chrome (#1826)", async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  const metas = p.locator('meta[name="theme-color"]');
  await expect(metas).toHaveCount(2);
  await expect(p.locator('meta[name="theme-color"][media*="light"]')).toHaveAttribute("content", "#ffffff");
  await expect(p.locator('meta[name="theme-color"][media*="dark"]')).toHaveAttribute("content", "#141a22");

  // The audit item is "the chrome matches the app theme", and per-scheme metas alone
  // don't deliver that: they follow the OS, so an explicit Dark on a light OS would
  // leave a white chrome over a dark app. Picking Dark must collapse BOTH metas.
  await p.locator('.af-theme-opt[data-theme-opt="dark"]').click();
  await expect(p.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(p.locator('meta[name="theme-color"][media*="light"]')).toHaveAttribute("content", "#141a22");
  await expect(p.locator('meta[name="theme-color"][media*="dark"]')).toHaveAttribute("content", "#141a22");

  await p.locator('.af-theme-opt[data-theme-opt="light"]').click();
  await expect(p.locator('meta[name="theme-color"][media*="dark"]')).toHaveAttribute("content", "#ffffff");

  // Back to Auto and the metas go per-scheme again, handing the decision back to the
  // media queries.
  await p.locator('.af-theme-opt[data-theme-opt="auto"]').click();
  await expect(p.locator('meta[name="theme-color"][media*="light"]')).toHaveAttribute("content", "#ffffff");
  await expect(p.locator('meta[name="theme-color"][media*="dark"]')).toHaveAttribute("content", "#141a22");
  await ctx.close();
});

test("the manifest is fetched by the browser, typed application/manifest+json, and install-complete", async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");

  const manifest = await p.evaluate(async () => {
    const res = await fetch("/manifest.webmanifest");
    return { status: res.status, type: res.headers.get("content-type"), body: await res.json() };
  });
  expect(manifest.status).toBe(200);
  // Go has no .webmanifest in its MIME table and we serve nosniff, so without the
  // daemon's explicit override this arrives as text/plain.
  expect(manifest.type).toBe("application/manifest+json");
  expect(manifest.body).toMatchObject({
    name: "Agent Factory",
    short_name: "af",
    start_url: "/",
    scope: "/",
    display: "standalone",
  });
  expect(manifest.body.description).toBeTruthy();
  expect(manifest.body.theme_color).toBeTruthy();
  expect(manifest.body.background_color).toBeTruthy();

  // Every icon the manifest promises must actually be served — Chrome silently drops
  // an install offer over a manifest whose icons 404.
  const icons = await p.evaluate(async () => {
    const res = await fetch("/manifest.webmanifest");
    const m = (await res.json()) as { icons: { src: string; purpose: string }[] };
    return Promise.all(
      m.icons.map(async (i) => ({ src: i.src, purpose: i.purpose, status: (await fetch(i.src)).status })),
    );
  });
  for (const icon of icons) {
    expect(icon.status, `${icon.src} must be served`).toBe(200);
  }
  expect(icons.some((i) => i.purpose === "maskable")).toBe(true);
  await ctx.close();
});

test("the service worker registers, controls the page, and caches the static shell", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);

  // Controlling the page is the precondition for every claim below: an installed
  // worker that never took control could not intercept anything, so proving the
  // bypass against an idle worker would prove nothing.
  expect(await p.evaluate(() => navigator.serviceWorker.controller !== null)).toBe(true);

  // Polled because the cache write rides event.waitUntil — it completes within the
  // fetch event's lifetime but not necessarily before the response reaches the page,
  // so the entry lands shortly after the asset finishes loading.
  await expect
    .poll(async () => (await cacheContents(p)).names.filter((n) => /^af-shell-[0-9a-f]{12}$/.test(n)).length)
    .toBeGreaterThan(0);
  // Two things must be cached, and they warm by different paths: the bundle as a
  // sub-resource (networkFirst), and the shell under /index.html as a NAVIGATION
  // (handleNavigation, warmed by the openControlledByWorker reload). The latter is
  // what a deep-route offline fallback resolves to, so assert it explicitly here.
  await expect.poll(async () => (await cacheContents(p)).urls.some((u) => u.endsWith("/af-web.js"))).toBe(true);
  await expect.poll(async () => (await cacheContents(p)).urls.some((u) => u.endsWith("/index.html"))).toBe(true);
  await ctx.close();
});

test("offline, a client-routed deep link serves the cached app shell, not a browser error", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);
  // The offline fallback resolves to the cached /index.html, so it must be warm before
  // we cut the network — otherwise this would fail for want of a cache rather than for
  // the behaviour under test.
  await expect.poll(async () => (await cacheContents(p)).urls.some((u) => u.endsWith("/index.html"))).toBe(true);

  await ctx.setOffline(true);

  // /tasks is a route the app never fetched or cached by name — serveSPA answers it
  // with the shell for client-side routing. Offline, the worker must do the same, or a
  // refresh/bookmark of a deep route shows the browser's network-error page instead of
  // the app booting and rendering its own daemon-unreachable state. Before the
  // navigation fallback existed, this passed straight through and failed offline.
  const resp = await p.goto("/tasks");
  expect(resp?.status(), "the deep-link navigation must be answered, not a network failure").toBe(200);
  expect(resp?.fromServiceWorker(), "offline, only the worker could have answered — proving the shell fallback").toBe(
    true,
  );
  // It is the app shell that came back (its module bundle tag), not some other cached
  // response. The bundle then boots from cache and takes over the route client-side.
  expect(await p.evaluate(() => !!document.querySelector('script[src="/af-web.js"]'))).toBe(true);

  await ctx.setOffline(false);
  await ctx.close();
});

test("the service worker NEVER caches /v1 — not the API, not the stream (the bypass allowlist)", async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);

  // By now the app has made real /v1 traffic on this page: the auth-info probe and
  // the Snapshot fetch both ran to render the rail, and the events WS is open. That
  // matters — a worker that cached indiscriminately would have caught them, so the
  // absence below is a real result rather than an absence of opportunity.
  await expect(p.locator(".af-app")).toHaveAttribute("data-live", "open");
  await p.evaluate(() => fetch("/v1/auth-info"));

  const { urls } = await cacheContents(p);
  const v1 = urls.filter((u) => new URL(u).pathname.startsWith("/v1/"));
  expect(v1, "the worker must never cache anything under /v1").toEqual([]);
  await ctx.close();
});

test("the service worker does not SERVE /v1: offline, the shell survives from cache but /v1 fails", async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);
  // Warm the shell cache and make the /v1 requests the app makes on any load. The
  // offline assertions below are only meaningful once the shell is actually cached —
  // otherwise "the shell survives offline" could fail for want of a cache rather than
  // for the reason under test.
  await expect(p.locator(".af-app")).toHaveAttribute("data-live", "open");
  await expect.poll(async () => (await cacheContents(p)).urls.some((u) => u.endsWith("/af-web.js"))).toBe(true);

  // Offline is the sharpest discriminator available, and the reason this test exists
  // rather than relying on the cache enumeration alone: with the network gone, a path
  // the worker serves still resolves and a path it merely passes through cannot. So
  // this separates "we didn't happen to cache /v1" from "the worker will never answer
  // for /v1", which is the actual guarantee the PTY stream depends on.
  await ctx.setOffline(true);

  const shellOffline = await p.evaluate(async () => (await caches.match("/af-web.js")) !== undefined);
  expect(shellOffline, "the shell must still be answerable offline — proving the worker does serve it").toBe(true);

  const apiOffline = await p.evaluate(async () => {
    try {
      const res = await fetch("/v1/auth-info");
      return { served: true, status: res.status };
    } catch {
      return { served: false, status: 0 };
    }
  });
  expect(
    apiOffline.served,
    "/v1 resolved with the network offline — the worker is answering for the API, which would put the PTY stream and the whole data plane behind a cache",
  ).toBe(false);

  await ctx.setOffline(false);
  await ctx.close();
});

test("a live PTY stream and the events WS survive the service worker controlling the page", REAL_FIXTURE, async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await pinRealFixtureProject(p);
  await openControlledByWorker(p);
  await assertRealRailFixture(p);

  // The functional half of the bypass, and the one that matters to a user: with the
  // worker in control, attach and watch real PTY frames arrive. A worker that
  // intercepted or delayed the stream would show up here as an empty pane.
  await expect(p.locator(".af-app")).toHaveAttribute("data-live", "open");
  await row(p, SESSION_A).click();
  await expect(p.locator(".af-term-host .xterm")).toBeVisible();
  await expect(p.locator(".af-term-host")).toContainText(READY_MARKER);
  await expect(p.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  await ctx.close();
});

/** Fires a synthetic beforeinstallprompt and reports whether our handler took it.
 *
 *  Headless Chromium will not fire the real event, so the flow is driven with a
 *  stand-in carrying the only surface install.ts touches: preventDefault, prompt(),
 *  and userChoice. That still exercises the real listener, the real visibility rule,
 *  and the real DOM — only the browser's own decision to offer is stubbed. */
async function fireInstallOffer(page: Page): Promise<void> {
  await page.evaluate(() => {
    const event = new Event("beforeinstallprompt", { cancelable: true }) as Event & {
      prompt: () => Promise<void>;
      userChoice: Promise<{ outcome: string; platform: string }>;
    };
    const w = window as unknown as { __afPromptCalls: number };
    w.__afPromptCalls = 0;
    event.prompt = () => {
      w.__afPromptCalls++;
      return Promise.resolve();
    };
    event.userChoice = Promise.resolve({ outcome: "accepted", platform: "web" });
    window.dispatchEvent(event);
  });
}

test("the install affordance stays hidden until the browser offers an install", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  // Nothing has offered an install, so there is nothing to offer the user. This is a
  // real assertion, not a stubbed one: headless Chromium genuinely does not fire
  // beforeinstallprompt here, which is the same reason the button correctly never
  // appears over plain-HTTP Tailscale — no event, no button (see install.ts).
  await expect(p.locator(".af-install")).toBeHidden();

  await fireInstallOffer(p);
  await expect(p.locator(".af-install")).toBeVisible();
  await expect(p.locator(".af-install__go")).toHaveText("Install app");
  await ctx.close();
});

test("the install button prompts once and then retires the spent offer", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();
  await fireInstallOffer(p);

  await p.locator(".af-install__go").click();
  // The stashed event's prompt() ran — the browser's install dialog was asked for.
  await expect
    .poll(() => p.evaluate(() => (window as unknown as { __afPromptCalls: number }).__afPromptCalls))
    .toBe(1);
  // A beforeinstallprompt event is single-use: prompt() throws if called twice. The
  // affordance retires with the offer it was showing, so there is no second click to
  // make.
  await expect(p.locator(".af-install")).toBeHidden();
  await ctx.close();
});

test("dismissing the install affordance sticks across reloads — it must never nag", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();
  await fireInstallOffer(p);
  await expect(p.locator(".af-install")).toBeVisible();

  await p.locator(".af-install__dismiss").click();
  await expect(p.locator(".af-install")).toBeHidden();

  // The whole point of persisting the dismissal: a reload that gets a FRESH offer
  // must still stay quiet. Without the localStorage flag this is exactly where the
  // button would come back and start nagging.
  await p.reload();
  await expect(p.locator(".af-app")).toBeVisible();
  await fireInstallOffer(p);
  await expect(p.locator(".af-install")).toBeHidden();
  await ctx.close();
});

test("new-tab menu (#2219): stays visible, hit-testable, and anchored while the tab bar scrolls", REAL_FIXTURE, async () => {
  // The seeded reorder session has enough real tabs to overflow a phone-width bar.
  // Select its project explicitly: after a worker restart the daemon's most-recent
  // project can be the task-only fixture, and this regression must not inherit that.
  await page.setViewportSize({ width: 1280, height: 720 });
  if ((await page.locator(".af-project-switch-name").textContent()) !== "mock-repo") {
    await page.locator(".af-project-switch").click();
    await projectItem(page, "mock-repo").click();
  }
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");
  await row(page, SESSION_ORDER).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await page.setViewportSize({ width: 375, height: 700 });

  const tabbar = page.locator(".af-tabbar");
  await expect
    .poll(() => tabbar.evaluate((bar) => bar.scrollWidth > bar.clientWidth), {
      message: "the real four-tab roster must overflow the narrow tab bar",
    })
    .toBe(true);
  const trigger = tabbar.locator(".af-tab-new");
  await trigger.click();
  const menu = tabbar.locator(".af-tab-menu");
  await expect(menu).toBeVisible();
  const before = await settledHitTestableTabMenu(page, menu, trigger);

  // Playwright scrolls the end-of-row trigger fully into view before clicking it.
  // Move the bar back a few pixels with the menu OPEN: both the caret and its fixed
  // menu should move together, while the menu remains clickable and in the viewport.
  const scroll = await tabbar.evaluate((bar) => {
    const before = bar.scrollLeft;
    bar.scrollLeft = Math.max(0, before - 4);
    return { before, after: bar.scrollLeft };
  });
  expect(scroll.after, "the overflow fixture must permit a real horizontal scroll").toBeLessThan(scroll.before);
  const after = await settledHitTestableTabMenu(page, menu, trigger);
  const triggerShift = after.trigger.x - before.trigger.x;
  const menuShift = after.menu.x - before.menu.x;
  expect(Math.abs(menuShift - triggerShift), "the menu must track the caret's horizontal scroll").toBeLessThan(1);
  expect(Math.abs(after.menu.x + after.menu.width - (after.trigger.x + after.trigger.width))).toBeLessThan(1);

  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await page.setViewportSize({ width: 1280, height: 720 });
});

test("#2224/#2354: desktop keeps title + tabs; mobile keeps only hamburger + tabs", REAL_FIXTURE, async ({ browser }, testInfo) => {
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!mockRepo, "AF_MOCK_REPO is set only by web-selftest-entry.sh");

  // Eight long tabs overflow even the desktop allocation while leaving the ninth
  // slot available, so the New-tab caret whose anchoring we verify still exists.
  const longTabs = Array.from({ length: 8 }, (_, i) => ({
    id: `title-row-tab-${i}`,
    name: i === 0 ? "agent" : `distinguishing-long-tab-${i}`,
    kind: i === 0 ? 0 : 2,
    command: i === 0 ? undefined : `sleep ${300 + i}`,
  }));
  const oneTab = [longTabs[0]];

  for (const width of [1280, 375]) {
    for (const theme of ["light", "dark"] as const) {
      for (const roster of ["one", "overflow"] as const) {
        await test.step(`${width}px · ${theme} · ${roster}`, async () => {
          const ctx = await browser.newContext({ viewport: { width, height: 720 } });
          try {
            await ctx.addInitScript(
              ({ root, savedTheme }) => {
                localStorage.setItem("af-project", root);
                localStorage.setItem("af-theme", savedTheme);
              },
              { root: mockRepo!, savedTheme: theme },
            );
            const p = await ctx.newPage();
            const title = `title-row-${roster}-${width}-${theme}-with-a-useful-distinguishing-suffix`;
            await p.route("**/v1/Snapshot", async (route) => {
              const resp = await route.fetch();
              const body = await resp.json();
              const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
              const list = snap?.instances ?? [];
              const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
              list.push({
                ...proto,
                id: `synth-${title}`,
                title,
                branch: `synth-${roster}-${width}-${theme}`,
                liveness: roster === "overflow" ? 6 : 2,
                in_flight_op: 0,
                lifecycle_action: "archive",
                limit_reset_at: roster === "overflow" ? "2099-01-01T00:00:00Z" : undefined,
                tabs: roster === "overflow" ? longTabs : oneTab,
                worktree: { ...(proto.worktree as Record<string, unknown>), repo_path: mockRepo },
              });
              if (snap) {
                snap.instances = list;
              }
              await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
            });
            // The synthetic row cannot be reordered by the real daemon. The browser
            // assertion only needs the delegated dragover path and its marker, but a
            // successful-shaped response keeps that gesture free of unrelated toasts.
            await p.route("**/v1/ReorderTab", async (route) => {
              await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ data: {} }) });
            });
            await p.goto("/");
            await expect(p.locator(".af-app")).toBeVisible();
            if (width <= 768) {
              await p.locator(".af-nav-toggle").click();
              await expect(p.locator(".af-app")).toHaveClass(/af-nav-open/);
            }
            await row(p, title).click();
            await expect(p.locator(".af-main.af-main-term")).toBeVisible();

            const head = p.locator(".af-term-head");
            const titleBox = head.locator(":scope > .af-term-head-main");
            const titleNode = titleBox.locator(".af-term-title");
            const tabbar = head.locator(":scope > .af-tabbar");
            const retry = head.getByRole("button", { name: "Retry", exact: true });
            const navToggle = p.locator(".af-nav-toggle");
            await expect(tabbar, "the strip belongs to the title row, not a second row").toHaveCount(1);
            await expect(titleNode).toHaveText(title);
            if (width <= 768) {
              await expect(titleBox, "mobile spends no row or width on a repeated session title").toBeHidden();
              await expect(navToggle, "the static hamburger remains the first mobile control").toBeVisible();
              await expect(p.locator(".af-viewnav"), "top-level navigation moves into the open drawer").toBeHidden();
              await expect(p.locator(".af-project-switch"), "project chrome moves into the open drawer").toBeHidden();
              await expect(p.locator(".af-appbar-more"), "app chrome moves into the open drawer").toBeHidden();
            } else {
              await expect(titleBox, "desktop keeps its identifying pane title").toBeVisible();
              await expect(navToggle).toBeHidden();
            }
            if (roster === "overflow") {
              await expect(retry, "Retry remains reachable at the usage-limit wall").toBeVisible();
            } else {
              await expect(retry, "the common path spends no width on hidden actions").toBeHidden();
            }

            const layout = await p.evaluate(() => {
              const rect = (selector: string) => {
                const box = document.querySelector(selector)!.getBoundingClientRect();
                return {
                  left: box.left,
                  right: box.right,
                  top: box.top,
                  bottom: box.bottom,
                  width: box.width,
                  height: box.height,
                  centerY: box.top + box.height / 2,
                };
              };
              const titleEl = document.querySelector<HTMLElement>(".af-term-title")!;
              const bar = document.querySelector<HTMLElement>(".af-tabbar")!;
              const retryEl = document.querySelector<HTMLElement>(".af-term-action:not([hidden])");
              const titleStyle = getComputedStyle(titleEl);
              const barStyle = getComputedStyle(bar);
              return {
                head: rect(".af-term-head"),
                titleBox: rect(".af-term-head-main"),
                title: rect(".af-term-title"),
                bar: rect(".af-tabbar"),
                nav: rect(".af-nav-toggle"),
                retry: retryEl ? rect(".af-term-action:not([hidden])") : null,
                host: rect(".af-term-host"),
                titleClientWidth: titleEl.clientWidth,
                titleScrollWidth: titleEl.scrollWidth,
                titleOverflow: titleStyle.overflow,
                titleTextOverflow: titleStyle.textOverflow,
                titleWhiteSpace: titleStyle.whiteSpace,
                barClientWidth: bar.clientWidth,
                barScrollWidth: bar.scrollWidth,
                barOverflowX: barStyle.overflowX,
                barPosition: barStyle.position,
                barParent: bar.parentElement?.className ?? "",
                hostPrevious: document.querySelector(".af-term-host")?.previousElementSibling?.className ?? "",
              };
            });
            expect(layout.barParent).toContain("af-term-head");
            expect(layout.hostPrevious).toContain("af-term-head");
            expect(layout.head.height, "one row reclaims the old stacked chrome height").toBeLessThan(64);
            expect(layout.bar.top).toBeGreaterThanOrEqual(layout.head.top);
            expect(layout.bar.bottom).toBeLessThanOrEqual(layout.head.bottom);
            expect(layout.host.top).toBeGreaterThanOrEqual(layout.head.bottom - 1);
            if (width <= 768) {
              expect(Math.abs(layout.nav.centerY - layout.bar.centerY), "hamburger and tabs share the sole mobile row").toBeLessThanOrEqual(1);
              expect(layout.titleBox.width, "the repeated mobile title consumes no horizontal space").toBe(0);
              expect(layout.host.top, "only the slim hamburger/tab row precedes mobile content").toBeLessThan(64);
            } else {
              expect(Math.abs(layout.titleBox.centerY - layout.bar.centerY), "desktop title and tabs share a baseline row").toBeLessThanOrEqual(1);
              expect(layout.titleBox.width, "the desktop title keeps a useful allocation").toBeGreaterThanOrEqual(120);
              expect(layout.titleClientWidth, "the readable desktop title never collapses to a token").toBeGreaterThanOrEqual(88);
              expect(layout.titleScrollWidth, "the long desktop title really needs truncation").toBeGreaterThan(layout.titleClientWidth);
              expect({
                overflow: layout.titleOverflow,
                textOverflow: layout.titleTextOverflow,
                whiteSpace: layout.titleWhiteSpace,
              }).toEqual({ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" });
            }
            expect(layout.barClientWidth, "the scrolling strip keeps an operable viewport").toBeGreaterThanOrEqual(96);
            expect(layout.barOverflowX).toBe("auto");
            expect(layout.barPosition, "#1813 marker keeps the tab bar as its containing block").toBe("relative");
            expect(await horizontalOverflow(p), "the combined row never widens the page").toBeLessThanOrEqual(1);
            if (layout.retry) {
              expect(Math.abs(layout.retry.centerY - layout.bar.centerY), "Retry stays aligned with the tab strip").toBeLessThanOrEqual(1);
              expect(layout.retry.width, "Retry is fixed-size rather than squeezed").toBeGreaterThan(40);
              expect(layout.retry.right).toBeLessThanOrEqual(layout.head.right);
            }

            if (roster === "one") {
              expect(layout.barScrollWidth, "one tab fits without a vestigial second row").toBeLessThanOrEqual(
                layout.barClientWidth + 1,
              );
            } else {
              expect(layout.barScrollWidth, "the long roster genuinely overflows").toBeGreaterThan(layout.barClientWidth);
              const trigger = tabbar.locator(".af-tab-new");
              await trigger.click();
              const menu = tabbar.locator(".af-tab-menu");
              await expect(menu).toBeVisible();
              const before = await settledHitTestableTabMenu(p, menu, trigger);
              const scroll = await tabbar.evaluate((bar) => {
                const before = bar.scrollLeft;
                bar.scrollLeft = Math.max(0, before - 4);
                return { before, after: bar.scrollLeft };
              });
              expect(scroll.after).toBeLessThan(scroll.before);
              const after = await settledHitTestableTabMenu(p, menu, trigger);
              expect(Math.abs(after.menu.x - before.menu.x - (after.trigger.x - before.trigger.x))).toBeLessThan(1);
              await p.keyboard.press("Escape");
              await expect(menu).toBeHidden();

              // Activating a tab rebuilds every button. The stable strip must retain
              // its viewport across that rebuild instead of snapping back to Agent —
              // otherwise the row moves under the pointer midway through a gesture.
              const farTab = tabByLabel(p, "distinguishing-long-tab-7");
              await farTab.scrollIntoViewIfNeeded();
              const beforeActivationScroll = await tabbar.evaluate((bar) => bar.scrollLeft);
              expect(beforeActivationScroll, "the activation starts from a genuinely scrolled viewport").toBeGreaterThan(0);
              await farTab.click();
              await expect(tabByLabel(p, "distinguishing-long-tab-7")).toHaveClass(/af-tab-active/);
              const afterActivationScroll = await tabbar.evaluate((bar) => bar.scrollLeft);
              expect(
                Math.abs(afterActivationScroll - beforeActivationScroll),
                "activating a tab preserves the strip's horizontal viewport",
              ).toBeLessThanOrEqual(1);

              await tabbar.evaluate((bar) => {
                bar.scrollLeft = 0;
              });
              const drag = await dragTabWithinBar(
                p,
                "distinguishing-long-tab-2",
                "distinguishing-long-tab-1",
                "before",
              );
              expect(drag.dropAllowed, "the nested strip remains a reorder target").toBe(true);
              expect(drag.markerShown, "the nested strip still draws its insertion marker").toBe(true);

              // The first click of a double-click activates an inactive tab and
              // synchronously rebuilds every button. Rename therefore belongs to the
              // stable bar, not to the button that may disappear halfway through the
              // gesture. Escape avoids mutating the synthetic daemon fixture while
              // proving the replacement button still enters edit mode.
              const renameTarget = tabByLabel(p, "distinguishing-long-tab-2");
              await renameTarget.dblclick();
              const edit = tabbar.locator(".af-tab-edit");
              await expect(edit, "an inactive tab remains renameable after activation rebuilds the bar").toBeVisible();
              await p.keyboard.press("Escape");
              await expect(edit).toBeHidden();
              await expect(tabByLabel(p, "distinguishing-long-tab-2")).toHaveCount(1);
            }

            await testInfo.attach(`2224-${width}-${theme}-${roster}`, {
              body: await p.screenshot(),
              contentType: "image/png",
            });
          } finally {
            await ctx.close();
          }
        });
      }
    }
  }
});

test("icons: the Lucide subset is inline, accessible, and currentColor-themed at desktop and 375px", REAL_FIXTURE, async ({
  browser,
}, testInfo) => {
  const accentByTheme = new Map<string, string>();
  for (const width of [1280, 375]) {
    for (const theme of ["light", "dark"] as const) {
      await test.step(`${width}px · ${theme}`, async () => {
        const ctx = await browser.newContext({ viewport: { width, height: 720 } });
        try {
          await ctx.addInitScript((savedTheme) => localStorage.setItem("af-theme", savedTheme), theme);
          const p = await ctx.newPage();
          await openTokenless(p);
          if (width === 375) {
            await p.getByRole("button", { name: "Toggle sessions" }).click();
            await expect(p.locator(".af-rail")).toBeVisible();
          }

          const projectIcon = p.locator(".af-project-glyph");
          const filterIcon = p.locator(".af-rail-filter-glyph");
          await expect(projectIcon).toBeVisible();
          await expect(filterIcon).toBeVisible();
          const projectPaint = await projectIcon.evaluate((node) => {
            const style = getComputedStyle(node);
            const box = node.getBoundingClientRect();
            return { color: style.color, stroke: style.stroke, width: box.width, height: box.height };
          });
          expect(projectPaint.stroke, "stroke=currentColor must resolve to the icon's theme color").toBe(
            projectPaint.color,
          );
          expect(projectPaint.width).toBeGreaterThanOrEqual(12);
          expect(projectPaint.width).toBeLessThanOrEqual(16);
          expect(projectPaint.height).toBe(projectPaint.width);
          accentByTheme.set(theme, projectPaint.color);

          const audit = await p.locator(".af-icon").evaluateAll((nodes) => {
            const visible = nodes.filter((node) => {
              const style = getComputedStyle(node);
              const box = node.getBoundingClientRect();
              return style.display !== "none" && style.visibility !== "hidden" && box.width > 0 && box.height > 0;
            });
            const unnamedIconControls = Array.from(document.querySelectorAll<HTMLElement>("button:has(.af-icon), a:has(.af-icon)"))
              .filter((control) => {
                const style = getComputedStyle(control);
                const box = control.getBoundingClientRect();
                return (
                  style.display !== "none" &&
                  style.visibility !== "hidden" &&
                  box.width > 0 &&
                  box.height > 0 &&
                  (control.innerText ?? "").trim() === ""
                );
              })
              .filter((control) => (control.getAttribute("aria-label") ?? "").trim() === "")
              .map((control) => control.className);
            const fontFaces = Array.from(document.styleSheets).flatMap((sheet) => {
              try {
                return Array.from(sheet.cssRules).filter((rule) => rule instanceof CSSFontFaceRule);
              } catch {
                return [];
              }
            }).length;
            return {
              count: visible.length,
              allHiddenFromAT: visible.every(
                (node) => node.getAttribute("aria-hidden") === "true" && node.getAttribute("focusable") === "false",
              ),
              allCurrentColor: visible.every((node) => getComputedStyle(node).stroke === getComputedStyle(node).color),
              unnamedIconControls,
              fontFaces,
              fontRequests: performance
                .getEntriesByType("resource")
                .map((entry) => entry.name)
                .filter((name) => /\.(?:woff2?|ttf|otf)(?:[?#]|$)/i.test(name)),
            };
          });
          expect(audit.count, "the live shell must exercise several real icon placements").toBeGreaterThan(5);
          expect(audit.allHiddenFromAT, "decorative SVGs stay out of the accessibility tree").toBe(true);
          expect(audit.allCurrentColor, "every visible icon inherits the active theme color").toBe(true);
          expect(audit.unnamedIconControls, "icon-only controls need an explicit accessible name").toEqual([]);
          expect(audit.fontFaces, "the SVG subset must not smuggle in an icon font").toBe(0);
          expect(audit.fontRequests, "the icon surface must make no font/network request").toEqual([]);
          expect(await horizontalOverflow(p), "icons must not widen the desktop or phone layout").toBeLessThanOrEqual(1);

          await testInfo.attach(`icons-${width}-${theme}`, {
            body: await p.screenshot(),
            contentType: "image/png",
          });
        } finally {
          await ctx.close();
        }
      });
    }
  }
  expect(accentByTheme.get("light"), "light and dark tokens must paint distinct icon colors").not.toBe(
    accentByTheme.get("dark"),
  );
});

test("vscode tab (#2077): the labelled New tab menu creates a VS Code tab and serves it through the proxy", REAL_FIXTURE, async () => {
  // End to end, with no seeded fixture: pick VS Code from the tab bar's kind menu,
  // and the daemon spawns a code-server (the FAKE one on PATH — no CI box has a
  // real one) on a 0600 unix socket it names, rooted at THIS session's worktree,
  // and reverse-proxies it into the pane. Nothing has spawned before this click:
  // the editor starts lazily on the first render.
  //
  // It runs near the end of the file on purpose: it leaves a tab on probe-a, and
  // the earlier tabs test asserts that session's exact tab count.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  // The choice is named on the tab bar itself. The pre-#2077 split control exposed
  // only `+` and an unlabeled caret, so a user had to know the hidden menu existed.
  const newTab = page.locator(".af-tab-new");
  await expect(newTab).toHaveCount(1);
  await expect(newTab).toContainText("New tab");
  await expect(newTab).toHaveAttribute("aria-label", "New tab · Terminal or VS Code");
  await expect(page.locator(".af-tab-new-kind")).toHaveCount(0);
  await expect(page.locator(".af-tab-menu")).toBeHidden();

  await newTab.click();
  const menu = page.locator(".af-tab-menu");
  await expect(menu).toBeVisible();
  await expect(menu.locator(".af-tab-menu-item")).toHaveText(["Terminal", "VS Code"]);

  await menu.locator(".af-tab-menu-item", { hasText: "VS Code" }).click();
  await expect(menu).toBeHidden();

  const editorTab = tabbar.locator(".af-tab", { hasText: "vscode" });
  await expect(editorTab).toHaveCount(1, { timeout: 30_000 });

  // Creating the tab must NOT strand the keyboard in terminal mode. createSessionTab
  // points the focused pane at the new tab and then attaches it — but a VS Code pane
  // is an iframe with no xterm, so there is nothing to attach to. Before the
  // SplitView.focus() boolean contract, focusTerminal() committed the app to
  // focus:"terminal" anyway and the xterm focus silently no-opped, leaving nav.ts
  // resolving every non-Escape key to {kind:"none"}: j/k/digits/t/w reached neither a
  // terminal nor the rail handler, so the user was stuck until they pressed Escape.
  // (The same trap predates VS Code — it applies to any web tab opened from the tab
  // bar — which is why the fallback lives in focusTerminal rather than this path.)
  await expect(
    page.locator(".af-app.af-kb-rail"),
    "creating a tab with no terminal must fall back to rail mode, not claim terminal mode",
  ).toBeVisible();

  // The real proof is that rail keys still DO something: j moves the selection.
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);
  await page.keyboard.press("j");
  await expect(
    row(page, SESSION_A),
    "j must still navigate the rail right after creating a VS Code tab",
  ).not.toHaveClass(/af-row-selected/);
  await expect(page.locator(".af-rail-list .af-row.af-row-selected")).toHaveCount(1);
  // k back to A so the editor assertions below run against this session.
  await page.keyboard.press("k");
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);

  await editorTab.click();

  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  // A vscode tab is ALWAYS daemon-proxied: its code-server listens on a unix socket
  // no browser can address, so the proxy path is the only route to it (#1873).
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\//);
  // It is an editor pane, not a terminal.
  await expect(page.locator(".af-term-host .af-pane-host .xterm")).toHaveCount(0);
  await expect(page.locator(".af-webpane-reload")).toHaveCount(1);
  // Unlike a web tab, the editor MUST have a real origin: VS Code needs
  // localStorage/workers, which an opaque origin denies. See mountWebPane.
  await expect(frame).toHaveAttribute("sandbox", /allow-same-origin/);

  // The daemon really spawned it and really relayed its content. A cold start shows
  // the self-refreshing "starting" notice first, so poll.
  const framed = page.frameLocator(".af-webframe");
  await expect(framed.locator("#marker")).toHaveText(VSCODE_MARKER, { timeout: 30_000 });
  // ...rooted at THIS session's worktree, not some other directory.
  await expect(framed.locator("#folder")).toContainText(SESSION_A, { timeout: 15_000 });

  // Escape closes the menu without creating anything (checked after, so a stray
  // tab from a mis-click can't be mistaken for the one above).
  await newTab.click();
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
});

// ===========================================================================
// #1813 — the tab UX pass: real pane labels, a designed dead-server state,
// rename, and reorder. Each of the four was an observation from the same
// exploratory play-test, and each is asserted here against the real daemon.
// ===========================================================================

/** A tab-bar tab by its EXACT label, anchored so "storefront" never also matches
 *  "storefront-2" — which the dup-suffix test below deliberately creates. */
function tabByLabel(page: Page, label: string): Locator {
  return page
    .locator(".af-tabbar .af-tab")
    .filter({ has: page.locator(".af-tab-label", { hasText: new RegExp(`^${label}$`) }) });
}

/** The tab bar's labels, left to right — the order a reorder permutes. */
function tabLabels(page: Page): Promise<string[]> {
  return page.locator(".af-tabbar .af-tab .af-tab-label").allTextContents();
}

/** Drives the bar's inline rename end to end: double-click the tab, replace the
 *  text, commit with Enter. Returns once the edit input is gone. */
async function renameTabViaUI(page: Page, label: string, newName: string): Promise<void> {
  await tabByLabel(page, label).dblclick();
  const input = page.locator(".af-tabbar .af-tab-edit");
  await expect(input, "a double-click on a renameable tab must open an inline edit").toBeVisible();
  await input.fill(newName);
  await input.press("Enter");
  await expect(input).toHaveCount(0);
}

/**
 * Simulates dragging the tab labelled `label` to a drop point WITHIN THE TAB BAR —
 * the reorder gesture, as opposed to dragTabToPane's drag-to-split.
 *
 * Same synthetic-DnD approach and same reason as dragTabToPane (Playwright's
 * mouse-based dragTo doesn't drive HTML5 drag-and-drop reliably): one shared
 * DataTransfer across dragstart/dragover/drop, so the real delegated listeners stamp
 * and read the payload exactly as a genuine drag would.
 *
 * The events are dispatched ON THE TARGET TAB and left to bubble to the bar, which is
 * where the handlers live — the same path a real drag takes — rather than fired at the
 * bar directly. The aim point is `side` of the target tab's CENTRE, because the centre
 * is what the insertion math resolves against (tabreorder.ts insertionIndexAt).
 *
 * Returns what the bar showed and whether it accepted the drop, so a test can assert
 * the REFUSAL of a drag (the pinned agent tab) rather than only its acceptance:
 * dragover going unprevented is precisely "this is not a drop target".
 */
async function dragTabWithinBar(
  page: Page,
  label: string,
  targetLabel: string,
  side: "before" | "after",
): Promise<{ markerShown: boolean; dropAllowed: boolean }> {
  return await page.evaluate(
    ({ label, targetLabel, side }) => {
      const byLabel = (want: string): Element | undefined =>
        [...document.querySelectorAll(".af-tabbar .af-tab")].find(
          (t) => t.querySelector(".af-tab-label")?.textContent === want,
        );
      const tab = byLabel(label);
      const target = byLabel(targetLabel);
      const marker = document.querySelector<HTMLElement>(".af-tab-insert");
      if (!tab || !target || !marker) {
        throw new Error(`drag source ${label} / target ${targetLabel} / marker not found`);
      }
      const dt = new DataTransfer();
      // The real delegated dragstart stamps the payload AND records the source index
      // the bar's dragover consults to refuse the pinned agent tab.
      tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));

      const r = target.getBoundingClientRect();
      // Just inside the target's far/near edge — unambiguously past / before its
      // centre, which is the gap the insertion math resolves to.
      const x = side === "after" ? r.right - 2 : r.left + 2;
      const y = r.top + r.height / 2;
      const init = { bubbles: true, cancelable: true, dataTransfer: dt, clientX: x, clientY: y };
      target.dispatchEvent(new DragEvent("dragenter", init));
      const over = new DragEvent("dragover", init);
      target.dispatchEvent(over);
      // preventDefault on dragover IS the "I accept a drop here" signal; without it a
      // real browser fires no drop at all, which is how the agent tab is refused.
      const dropAllowed = over.defaultPrevented;
      const markerShown = !marker.hidden;
      if (dropAllowed) {
        target.dispatchEvent(new DragEvent("drop", init));
      }
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
      return { markerShown, dropAllowed };
    },
    { label, targetLabel, side },
  );
}

test("#1813: a pane header names its tab — icon + name — not a positional 'Tab N'", REAL_FIXTURE, async () => {
  // The bug: every pane header read `Tab ${leaf.tab + 1}`, so at the exact moment the
  // label matters — several panes open, which is the whole point of splits — it told
  // you nothing, and it silently meant a different tab after a close.
  await row(page, SESSION_ORDER).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(tabByLabel(page, "alpha")).toHaveCount(1, { timeout: 15_000 });

  // Two panes: the agent tab and the web tab beside it.
  await dragTabToPane(page, "alpha", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);

  // Each header names its own tab, in pane order.
  await expect(page.locator(".af-term-host .af-pane-label")).toHaveText(["Agent", "alpha"]);
  // ...with the kind icon beside it, the same mark the tab bar draws.
  expect(
    await page.locator(".af-term-host .af-pane-glyph .af-icon").evaluateAll((nodes) =>
      nodes.map((node) => node.getAttribute("data-icon")),
    ),
  ).toEqual(["bot", "panels"]);
  // The positional label is GONE — the assertion that would have failed before #1813.
  // Scoped to the pane host, which holds the panes and nothing else (the tab bar is
  // its sibling), and asserted on that single container rather than on the two
  // .af-pane-head nodes, which a negative match can't address without strict mode.
  await expect(page.locator(".af-term-host")).not.toContainText("Tab 1");
  await expect(page.locator(".af-term-host")).not.toContainText("Tab 2");

  // The tab bar carries the same icons, so the two surfaces agree.
  await expect(tabByLabel(page, "alpha").locator('.af-tab-glyph[data-icon="panels"]')).toHaveCount(1);
  await expect(tabByLabel(page, "Agent").locator('.af-tab-glyph[data-icon="bot"]')).toHaveCount(1);
  await expect(tabByLabel(page, "beta").locator('.af-tab-glyph[data-icon="terminal"]')).toHaveCount(1);
});

test("#1813: dragging a tab within the bar reorders it, and the order survives a reload", REAL_FIXTURE, async () => {
  // Reordering is a drag over the BAR; splitting is a drag over a PANE. Same payload,
  // disjoint drop regions — this asserts the bar half exists at all, which it did not
  // before #1813 (the strip had dragstart but no dragover/drop).
  await row(page, SESSION_ORDER).click();
  expect(await tabLabels(page)).toEqual(["Agent", "alpha", "beta", "gamma"]);

  // Move gamma up in front of beta.
  const res = await dragTabWithinBar(page, "gamma", "beta", "before");
  expect(res.dropAllowed, "the bar must accept a reorder drop").toBe(true);
  expect(res.markerShown, "an insertion indicator must show where the drop lands").toBe(true);
  await expect(page.locator(".af-tabbar .af-tab .af-tab-label")).toHaveText(["Agent", "alpha", "gamma", "beta"]);

  // The daemon persisted it: a reload re-reads the roster from the Snapshot, so the
  // new order surviving proves it is real state and not a local DOM shuffle.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await assertRealRailFixture(page);
  await row(page, SESSION_ORDER).click();
  await expect(page.locator(".af-tabbar .af-tab .af-tab-label")).toHaveText(["Agent", "alpha", "gamma", "beta"], {
    timeout: 15_000,
  });
});

test("#1813: a reorder does not misroute an OPEN pane — panes are pinned by tab id", REAL_FIXTURE, async () => {
  // The #1810 guarantee, now reachable from the client for the first time: before
  // #1813 no client could produce a reorder at all, so the id-keyed pane binding had
  // never actually been driven by one. Moving a tab must move the PANE's tab with it,
  // never rebind the pane to whoever takes the vacated ordinal.
  await row(page, SESSION_ORDER).click();
  await dragTabToPane(page, "alpha", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);

  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });
  const srcBefore = await frame.getAttribute("src");

  // Move alpha from index 1 to the end: its ORDINAL changes, which is exactly the
  // condition an ordinal-keyed pane would misroute on.
  const res = await dragTabWithinBar(page, "alpha", "beta", "after");
  expect(res.dropAllowed).toBe(true);
  await expect(page.locator(".af-tabbar .af-tab .af-tab-label")).toHaveText(["Agent", "gamma", "beta", "alpha"]);

  // The pane still shows ALPHA's own dev server...
  await expect(page.locator(".af-term-host .af-pane-label")).toHaveText(["Agent", "alpha"]);
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });
  // ...addressed by the SAME stable tab id as before the move. The src is id-keyed
  // (#1810), so an unchanged src is the proof the pane followed its tab rather than
  // being rebound to the tab that took index 1.
  expect(await frame.getAttribute("src"), "the pane must still address alpha's own tab id").toBe(srcBefore);
});

test("#1813: the agent tab can't be reordered, but still drags onto a pane to split", REAL_FIXTURE, async () => {
  // Go's Tabs[0] is a load-bearing invariant (archive teardown and the agent's own
  // conversation/tmux all index it), so the daemon refuses to move it or to move
  // anything in front of it. The bar refuses up front — but it must refuse ONLY the
  // reorder: dragging the agent tab into a pane is the oldest gesture this feature
  // has, and pinning the tab at the DROP rather than at the source is what keeps it.
  await row(page, SESSION_ORDER).click();
  const before = await tabLabels(page);

  // Refused over the bar: no drop target, no indicator, no request.
  const onBar = await dragTabWithinBar(page, "Agent", "gamma", "after");
  expect(onBar.dropAllowed, "the bar must not accept the pinned agent tab").toBe(false);
  expect(onBar.markerShown, "no insertion indicator for a drop that cannot happen").toBe(false);
  expect(await tabLabels(page), "the roster must be untouched").toEqual(before);

  // ...and nothing may drop in FRONT of it either: aim at the agent tab's left half
  // with a movable tab, and the drop resolves to the gap AFTER the agent tab rather
  // than to index 0. Asserted through a retrying locator, not a tabLabels() snapshot:
  // the drop fires an RPC + resync, so a point-read races the repaint.
  const inFront = await dragTabWithinBar(page, "beta", "Agent", "before");
  expect(inFront.dropAllowed).toBe(true);
  await expect(
    page.locator(".af-tabbar .af-tab .af-tab-label"),
    "beta lands just AFTER the pinned agent tab, never in front of it",
  ).toHaveText(["Agent", "beta", "gamma", "alpha"]);

  // The split gesture still works for the agent tab — the regression this guards, and
  // the reason the tab stays draggable and is pinned at the DROP instead.
  //
  // Reload first, for a single-pane layout (the retained trees are in-memory). Both
  // ends of that matter: dragging the agent tab onto a pane it ALREADY occupies is a
  // no-op under the one-tab-one-pane rule, and starting from two panes would end at
  // two panes whether or not the drag did anything — so neither would fail if the tab
  // were undraggable. Point the single pane at another tab first, then drag the agent
  // tab in beside it: 1 → 2 panes is the assertion that can actually fail.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await assertRealRailFixture(page);
  await row(page, SESSION_ORDER).click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  await tabByLabel(page, "alpha").click();
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2);
  await expect(page.locator(".af-term-host .af-pane-label")).toHaveText(["alpha", "Agent"]);
});

test("#1813: renaming from the UI repaints the tab bar AND an open pane, live", REAL_FIXTURE, async () => {
  await row(page, SESSION_ORDER).click();
  // Put alpha in its own pane, so the rename has an open pane header to repaint —
  // the case reconcile() used to miss entirely (it set a pane's label only when the
  // pane was created or rebound, and a rename does neither).
  await dragTabToPane(page, "alpha", "right");
  await expect(page.locator(".af-term-host .af-pane-label")).toContainText(["alpha"]);

  await renameTabViaUI(page, "alpha", "storefront");

  // The bar renames...
  await expect(tabByLabel(page, "storefront")).toHaveCount(1, { timeout: 15_000 });
  await expect(tabByLabel(page, "alpha")).toHaveCount(0);
  // ...and so does the OPEN pane's header, without a reload and without the pane's
  // iframe being torn down (a rename changes no address, so the live preview stays).
  await expect(page.locator(".af-term-host .af-pane-label")).toContainText(["storefront"], { timeout: 15_000 });
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER);
});

test("#1813: the daemon's RESOLVED name is rendered, not the one that was typed", REAL_FIXTURE, async () => {
  // The daemon applies the same sanitize + dup-suffix rules a create goes through, so
  // what a user types and what the tab is called are different strings. Renaming beta
  // to a name that is already taken must land as "storefront-2" — rendering the typed
  // string would show two identical tabs and silently disagree with the daemon.
  await row(page, SESSION_ORDER).click();
  await renameTabViaUI(page, "beta", "storefront");

  await expect(tabByLabel(page, "storefront-2"), "the dup-suffixed name the daemon returned").toHaveCount(1, {
    timeout: 15_000,
  });
  // Exactly one tab is called "storefront" — the original.
  await expect(tabByLabel(page, "storefront")).toHaveCount(1);
  await expect(tabByLabel(page, "beta")).toHaveCount(0);
});

test("#1813: Escape cancels a rename, and an agent tab offers no rename at all", REAL_FIXTURE, async () => {
  await row(page, SESSION_ORDER).click();

  // Escape abandons the edit — and the blur it causes must not commit it instead.
  await tabByLabel(page, "gamma").dblclick();
  const input = page.locator(".af-tabbar .af-tab-edit");
  await expect(input).toBeVisible();
  await input.fill("thrown-away");
  await input.press("Escape");
  await expect(input).toHaveCount(0);
  await expect(tabByLabel(page, "gamma"), "Escape must leave the name alone").toHaveCount(1);
  await expect(tabByLabel(page, "thrown-away")).toHaveCount(0);

  // The agent tab renders a FIXED label and ignores its name (ui/tree/labels.go), so
  // renaming it would change nothing visible — the daemon refuses, and the affordance
  // is never offered rather than being offered and failing.
  await tabByLabel(page, "Agent").dblclick();
  await expect(page.locator(".af-tabbar .af-tab-edit"), "no rename affordance on the agent tab").toHaveCount(0);
});

test("#1813 (#1812 path): a CLI rename reaches a SECOND open window with no reload", REAL_FIXTURE, async ({ browser }) => {
  // The event half: a rename is published as session.updated, so a window that did
  // not make the change still repaints. This is the shape a real user hits — an agent
  // renames a tab from inside its own session while the browser sits open.
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };

  // A genuinely separate window, not a second tab of the same page.
  const ctx = await browser.newContext();
  const second = await ctx.newPage();
  try {
    await openTokenless(second);
    await row(second, SESSION_ORDER).click();
    await expect(tabByLabel(second, "gamma")).toHaveCount(1, { timeout: 15_000 });

    // Stamp the live document: a reload wipes this, so asserting it survives is what
    // makes "with no reload" a claim rather than an assumption.
    await second.evaluate(() => {
      (window as Window & { __af1813?: boolean }).__af1813 = true;
    });

    af("sessions", "tab-rename", SESSION_ORDER, "--name", "gamma", "--new-name", "renamed-by-cli");

    await expect(tabByLabel(second, "renamed-by-cli"), "a CLI rename must reach an open window").toHaveCount(1, {
      timeout: 15_000,
    });
    await expect(tabByLabel(second, "gamma")).toHaveCount(0);
    expect(
      await second.evaluate(() => (window as Window & { __af1813?: boolean }).__af1813 === true),
      "the rename must arrive on the LIVE page, not via a reload",
    ).toBe(true);
  } finally {
    await second.close();
    await ctx.close();
  }
});

test("#1813: a roster change mid-edit renames the EDITED tab, not whatever slid into its slot", REAL_FIXTURE, async () => {
  // The stale-ordinal bug, driven through the path that makes it ordinary rather than
  // rare. An inline edit captured the ordinal it was opened at and dereferenced it at
  // COMMIT — against a roster that another client may have permuted meanwhile. What
  // makes that reachable is not a narrow race: the session.updated serving the change
  // repaints the bar, the repaint evicts the focused input, and evicting a focused
  // input FIRES ITS BLUR — which is itself the commit. So the roster change and the
  // commit are the same event, and the tab that slid into the slot got renamed.
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const EDITED_TO = "edited-tab-wins";

  await row(page, SESSION_ORDER).click();
  // Assert the roster this needs rather than inherit it (see resetToAgentTab): the
  // slots are load-bearing here, and reading the names keeps the test honest if an
  // earlier one renames them.
  await expect(page.locator(".af-tabbar .af-tab"), "the four-tab roster this permutes").toHaveCount(4, {
    timeout: 15_000,
  });
  const [, pinned, edited, mover] = await tabLabels(page);

  // Open an edit on slot 2 and type a name — WITHOUT committing it. The input stays
  // focused, which is what makes the repaint below blur it.
  await tabByLabel(page, edited).dblclick();
  const input = page.locator(".af-tabbar .af-tab-edit");
  await expect(input, "a double-click on a renameable tab must open an inline edit").toBeVisible();
  await input.fill(EDITED_TO);

  // The SECOND defect on this path, which the assertions below cannot see because it
  // heals itself. Chromium fires a focused node's blur while it is still CONNECTED, so
  // an input evicted by the bar's replaceChildren() runs its handler — and that
  // handler's own input.replaceWith(btn) mutates the very child list replaceChildren is
  // walking, which throws NotFoundError and aborts the repaint half-built. The bar is
  // left holding a partial tab list, and renderTabBar's signature is never updated. It
  // survives only because the rename's own resync repaints it a moment later, so the
  // bar below converges and every text assertion passes either way; the throw is the
  // one durable trace. renderTabBar settles an open edit through its own blur path
  // BEFORE rebuilding (settleTabEdit), so the commit is identical but the child list is
  // quiescent. Collected from here — the reorder is what triggers the repaint.
  const pageErrors: string[] = [];
  page.on("pageerror", (e) => pageErrors.push(e.message));

  // Another client moves `mover` INTO the edited tab's slot. `edited` shifts right.
  af("sessions", "tab-reorder", SESSION_ORDER, "--name", mover, "--index", "2");

  // The commit lands on the tab whose input was edited, wherever it now sits — and the
  // tab that took its slot keeps its name. Asserted as the whole bar, left to right, so
  // it pins the pairing of name to POSITION: renaming the right tab but drawing it in
  // the wrong slot, or renaming `mover` (the bug: it now occupies the captured ordinal),
  // both fail here. Under the bug this read [Agent, pinned, EDITED_TO, edited].
  await expect(page.locator(".af-tabbar .af-tab .af-tab-label")).toHaveText(["Agent", pinned, mover, EDITED_TO], {
    timeout: 15_000,
  });
  await expect(input, "the edit is settled by the repaint, not left orphaned in the bar").toHaveCount(0);
  expect(pageErrors, "the repaint that ends an edit must not throw — see settleTabEdit").toEqual([]);
  // Nothing was renamed twice: exactly one tab answers to the typed name.
  await expect(tabByLabel(page, EDITED_TO)).toHaveCount(1);
});

test("#1813: a close+recreate of the same name mid-edit renames NOTHING — never the replacement", REAL_FIXTURE, async ({
  browser,
}) => {
  // The same class as the spec above, one layer in from the ordinal. Keying the commit
  // on an identity fixes the reorder — but only if the identity is the one the user
  // OPENED the edit on. Re-reading it at commit asks a different question ("who is at
  // this position NOW?"), and tabBarSig covers only what the bar DRAWS (kind/name/
  // active/shown), so a close plus a recreate of the SAME name in the same slot is
  // signature-identical: the bar is never rebuilt, the input survives, and the
  // per-snapshot identity cache underneath it has been restamped with the REPLACEMENT's
  // id. The commit then lands on a tab the user never edited.
  //
  // The snapshot gap that fuses the two is DESIGNED, not exotic: events published while
  // the socket is down are dropped rather than replayed, so the reconnect's single
  // re-Snapshot (events.ts) is exactly where a close and a recreate arrive as one roster
  // change. This test opens that gap the way an outage does — it blocks the events
  // endpoint and drops the live socket — because the fusion is the whole point:
  // delivered as two live events the close and the recreate would each repaint the bar
  // and settle the edit early, which is the path the spec above already covers.
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const VICTIM = "victim-of-recreate";
  const TYPED = "typed-into-a-dead-tab";

  // Driven in its OWN window: the gap is opened by holding this client's event stream
  // DOWN, and the shared `page` must not inherit that — a socket left blocked by a
  // failing assertion would fail every spec after this one rather than just this one.
  const ctx = await browser.newContext();
  const win = await ctx.newPage();
  try {
    // A handle on the SPA's event socket, so the test can drop it. Recording only — the
    // constructor is not otherwise altered, so the app connects exactly as it always
    // does. Needed because closing the LIVE socket is the one thing network emulation
    // will not do for us: going offline blocks new traffic but leaves an established
    // WebSocket open, so the client never notices, never reconnects, and never
    // re-Snapshots (measured — the first cut of this test timed out waiting for it).
    await win.addInitScript(() => {
      const w = window as unknown as { __afEventSockets: WebSocket[] };
      w.__afEventSockets = [];
      const Native = WebSocket;
      window.WebSocket = new Proxy(Native, {
        construct(target, args: [string, (string | string[])?]) {
          const ws = new target(...args);
          if (String(args[0]).includes("/v1/events")) {
            w.__afEventSockets.push(ws);
          }
          return ws;
        },
      });
    });
    // Block the events endpoint at the NETWORK layer, so every reconnect ATTEMPT fails
    // for as long as the flag is set. Together with the close below this reproduces a
    // real outage: the socket drops, retries fail, and the events published meanwhile
    // are dropped by the daemon's hub rather than queued (events.ts) — which is exactly
    // why the reconnect must re-Snapshot, and why that one Snapshot carries a close and
    // a recreate FUSED into a single roster change.
    const cdp = await ctx.newCDPSession(win);
    await cdp.send("Network.enable");

    await openTokenless(win);
    await row(win, SESSION_ORDER).click();
    await expect(win.locator(".af-tabbar .af-tab")).toHaveCount(4, { timeout: 15_000 });
    // A tab of this test's OWN, appended LAST: a recreate appends, so closing and
    // recreating the last tab restores the roster exactly — same kinds, same names, same
    // order — which is what makes the signature identical and the bar hold still. Owning
    // it also keeps this test independent of whatever the specs above renamed.
    af("sessions", "tab-create", SESSION_ORDER, "--command", "sleep 300", "--name", VICTIM);
    await expect(tabByLabel(win, VICTIM)).toHaveCount(1, { timeout: 15_000 });
    const roster = await tabLabels(win);
    // The bar WHILE the edit is open: the input REPLACES the edited tab's button, so
    // that tab draws no label until the edit is settled.
    const editing = roster.filter((label) => label !== VICTIM);

    // Open an edit on it and type a new name, WITHOUT committing.
    await tabByLabel(win, VICTIM).dblclick();
    const input = win.locator(".af-tabbar .af-tab-edit");
    await expect(input, "a double-click on a renameable tab must open an inline edit").toBeVisible();
    await input.fill(TYPED);
    const editedPane = win.locator(".af-term-host .af-pane");
    await expect(editedPane, "the edited tab must own the one visible pane").toHaveCount(1);
    const editedPaneID = await editedPane.getAttribute("data-tab-id");
    expect(editedPaneID, "the edited pane must expose the tab's stable id").toMatch(/\S/);

    // The gap: retries are refused, then the live socket is dropped. Neither the close
    // nor the recreate below is delivered as an event — delivered live they would each
    // repaint the bar and settle the edit early, which is the (already-covered) path
    // above, not this one.
    await cdp.send("Network.setBlockedURLs", { urls: ["*/v1/events*"] });
    const eventSocketClose = await win.evaluate(async () => {
      const w = window as unknown as { __afEventSockets: WebSocket[] };
      await Promise.all(
        w.__afEventSockets.map(
          (ws) =>
            new Promise<void>((resolve) => {
              if (ws.readyState === WebSocket.CLOSED) {
                resolve();
                return;
              }
              ws.addEventListener("close", () => resolve(), { once: true });
              if (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN) {
                ws.close();
              }
            }),
        ),
      );
      return {
        closed: WebSocket.CLOSED,
        states: w.__afEventSockets.map((ws) => ws.readyState),
      };
    });
    expect(eventSocketClose.states, "the test must capture the live event socket").not.toHaveLength(0);
    expect(
      eventSocketClose.states.every((state) => state === eventSocketClose.closed),
      `the event gap must exist before daemon mutations; socket states=${JSON.stringify(eventSocketClose.states)}`,
    ).toBe(true);
    await expect(
      win.locator(".af-app"),
      "the client must enter reconnecting state while the events endpoint stays blocked",
    ).toHaveAttribute("data-live", "reconnecting");
    af("sessions", "tab-delete", SESSION_ORDER, "--name", VICTIM);
    af("sessions", "tab-create", SESSION_ORDER, "--command", "sleep 300", "--name", VICTIM);

    // Unblocked: the next retry opens, and an open re-Snapshots. THAT single roster change
    // carries both. Waited on explicitly, because committing before it lands would make
    // the assertions below pass against the bug too — the commit would then miss on an id
    // that simply has not been replaced yet, which is the one way this test could rot
    // into proving nothing.
    const resync = win.waitForResponse((r) => r.url().includes("/v1/Snapshot") && r.status() === 200, {
      timeout: 30_000,
    });
    await cdp.send("Network.setBlockedURLs", { urls: [] });
    await resync;
    // waitForResponse resolves at the response headers, before the app necessarily
    // reads the body and applies it. Poll the pane's production identity instead: it
    // changes only when the reconnect Snapshot has reached store.set → split reconcile,
    // so Enter below cannot race a stale client cache under a loaded test box (#2387).
    await expect
      .poll(
        async () => {
          const id = await editedPane.getAttribute("data-tab-id");
          return id !== null && id !== "" && id !== editedPaneID;
        },
        {
          message: "the reconnect Snapshot must bind the pane to the replacement tab id",
          timeout: 15_000,
        },
      )
      .toBe(true);

    // The bar held still across all of it — the precondition the bug needs, asserted
    // rather than assumed: had the roster change repainted, the input would be gone and
    // this test would be exercising the (already-covered) settle path instead of this one.
    await expect(input, "a same-name close+recreate must be invisible to tabBarSig").toBeVisible();
    await expect(win.locator(".af-tabbar .af-tab .af-tab-label")).toHaveText(editing);

    await input.press("Enter");
    await expect(input).toHaveCount(0);

    // The miss is REPORTED — and this assertion comes FIRST because it is the only one
    // here that is deterministic. Resolving the miss is synchronous (no request is made
    // at all), so the toast is up within a frame of Enter; a regression that renamed the
    // replacement instead would issue a request, succeed, and never raise it, failing
    // here rather than racing. The two assertions below are the user-visible outcome,
    // but on their own they would also pass against the bug simply by being evaluated
    // before its rename round-tripped — see the note above about proving nothing.
    await expect(win.locator(".af-toast"), "an unresolvable rename must say so").toContainText("nothing was renamed");
    // NOTHING was renamed: no tab answers to what was typed, and the replacement keeps
    // the name it was created with.
    await expect(tabByLabel(win, TYPED), "the rename must land on nothing, never on the replacement").toHaveCount(0);
    await expect(
      win.locator(".af-tabbar .af-tab .af-tab-label"),
      "the replacement must NOT inherit the edit",
    ).toHaveText(roster);
  } finally {
    // Restore the roster for the specs below, whatever happened above. Best-effort: the
    // tab may already be gone, and a teardown failure must not mask the real one.
    try {
      af("sessions", "tab-delete", SESSION_ORDER, "--name", VICTIM);
    } catch {
      /* already gone */
    }
    await win.close();
    await ctx.close();
  }
});

test("#1813: a double-click renames an INACTIVE tab, not only the active one", REAL_FIXTURE, async () => {
  // The first click of a double-click switches tabs, which changes tabBarSig (active)
  // and so REPLACES the very button the gesture began on — a shape that has broken real
  // gestures here before (#1737). Rename is therefore owned by the stable bar: it
  // captures the first click's tab identity, then resolves the replacement button on
  // click two. So rename-by-double-click works from any tab, active or not, even when
  // the replacement shifts away from the original pointer coordinate.
  //
  // Pinned because the reasoning is not local to this file — it is a property of how
  // Blink resolves a click target across a DOM swap — and because the specs above all
  // happen to double-click tabs without pinning which one is ACTIVE, so none of them
  // would notice if this stopped working.
  await row(page, SESSION_ORDER).click();
  await expect(page.locator(".af-tabbar .af-tab"), "the seeded roster").toHaveCount(4, { timeout: 15_000 });
  const [, , , victim] = await tabLabels(page);

  // Make the target provably INACTIVE, rather than inheriting whatever the spec above
  // left active — the whole point of the test is the state it is exercised from.
  await tabByLabel(page, "Agent").click();
  await expect(page.locator(".af-tabbar .af-tab.af-tab-active"), "the agent tab holds the highlight").toContainText(
    "Agent",
  );
  await expect(tabByLabel(page, victim), "the tab under test must not be the active one").not.toHaveClass(
    /af-tab-active/,
  );

  await tabByLabel(page, victim).dblclick();
  await expect(
    page.locator(".af-tabbar .af-tab-edit"),
    "a double-click on an INACTIVE tab must open its rename input",
  ).toBeVisible();
  await page.locator(".af-tabbar .af-tab-edit").press("Escape");
});

test("#1738 invariant: every tab the daemon serves carries a stable id — the premise the bar's rename rests on", REAL_FIXTURE, async () => {
  // A tab BUTTON outlives every snapshot that leaves tabBarSig unchanged: the
  // signature covers only what the bar DRAWS (kind/name/active/shown) and is
  // deliberately blind to tab IDS, because rebuilding on an id change would destroy a
  // button held mid-drag — the #1737 regression. So a button can in principle hold a
  // tab object whose id has since been restamped, and since #1904 made the rename
  // id-keyed, committing that stale identity against the CURRENT roster would resolve
  // to nothing and SILENTLY DO NOTHING.
  //
  // What keeps that off the reachable path is this invariant: every Tab constructor
  // mints an id (session/tab.go) and restoreLocalTabs backfills a legacy pre-#1738
  // record on load (session/instance_data.go), while ToInstanceData copies it
  // verbatim — so the SPA is never served an id-less tab, never synthesizes the
  // kind:name fallback, and the "" → real backfill a stale identity would need simply
  // never happens here. (ui.ts no longer DEPENDS on that: liveTabIdentity resolves an
  // identity from the per-snapshot cache at commit rather than trusting the object the
  // button was built with. This asserts the premise anyway, because it is enforced two
  // layers away in another language and nothing on the web side would notice it bend.)
  //
  // The proxy has no such slack and is why this matters beyond rename: a web/vscode
  // pane's src is /v1/webtab/{session}/{tabId}/, id-keyed since #1810 with no ordinal
  // form to fall back on, so an id-less tab is simply unaddressable (split.ts).
  const res = await page.request.post("/v1/Snapshot", { data: { repo_id: "" } });
  expect(res.status(), "the Snapshot route must answer the tokenless loopback client").toBe(200);
  const env = (await res.json()) as {
    data?: { instances?: { title?: string; tabs?: { id?: string; name?: string }[] }[] } | null;
  };
  const instances = env.data?.instances ?? [];

  // Name every offender rather than asserting a count, so a failure says WHICH tab.
  const idless = instances.flatMap((inst) =>
    (inst.tabs ?? []).filter((t) => (t.id ?? "") === "").map((t) => `${inst.title ?? "?"}/${t.name ?? "?"}`),
  );
  expect(idless, "every tab the SPA is served must carry a stable id (#1738)").toEqual([]);
  // ...and the roster really had tabs to check: an empty tab list would make the
  // assertion above vacuously true, which is the one way this test could rot into
  // proving nothing.
  const total = instances.flatMap((inst) => inst.tabs ?? []).length;
  expect(total, "the seeded sessions must actually carry tabs for the check to mean anything").toBeGreaterThan(0);
});

test("#1813: a dead dev server shows the designed fallback — never the raw JSON envelope", REAL_FIXTURE, async () => {
  // The most common failure in the loop web tabs exist for: the port isn't up yet, or
  // the server crashed. An agent creating a preview tab and a dev server finishing
  // boot are inherently racy, so "the tab exists before the port answers" is the
  // NORMAL first state — and it used to render the daemon's 502 envelope as text.
  await row(page, SESSION_DEAD).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const deadTab = tabByLabel(page, "deadport");
  await expect(deadTab).toHaveCount(1, { timeout: 15_000 });
  await deadTab.click();

  // The designed state, named for the port a developer has to go fix.
  const fallback = page.locator(".af-term-host .af-webpane-fallback.af-webpane-dead");
  await expect(fallback).toBeVisible({ timeout: 15_000 });
  await expect(fallback).toContainText(`No dev server is answering at localhost:${DEAD_PORT} yet.`);

  // No raw JSON, anywhere. The parent probed out of band and never pointed the frame
  // at a target it knew was down, so there is no 502 body in the DOM to leak — the
  // assertion is structural, not a claim that something is merely covered up.
  const host = page.locator(".af-term-host");
  await expect(host, "the API envelope must not be rendered as the preview").not.toContainText("unreachable");
  await expect(host).not.toContainText("connection refused");
  await expect(host).not.toContainText('"error"');
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toBeHidden();
  expect(await frame.getAttribute("src"), "the frame must never be pointed at a dead target").toBeNull();

  // Item B (Codex P3): the BAR's "Open ↗" is withdrawn too, not just the fallback's
  // link. It points at the same proxied URL the probe found 502ing, so clicking it
  // would open a new tab showing the daemon's raw 502 JSON — the exact thing this
  // state exists to hide. Retry is the only offered action.
  await expect(page.locator("a.af-webpane-open"), "the bar's Open ↗ must not route to the 502").toBeHidden();

  // The fallback carries its own retry, and it really re-probes: while the port is
  // still dead, a retry must land back in the same state rather than flashing a frame.
  const retry = fallback.locator(".af-webpane-fallback-retry");
  await expect(retry).toBeVisible();
  await retry.click();
  await expect(fallback).toBeVisible();
  await expect(frame).toBeHidden();

  // ...and when the server finally comes up, the SAME retry recovers into a live
  // preview. This is what makes the state actionable rather than terminal — and it
  // proves the probe is a real request each time, not a one-shot verdict cached at
  // mount. The server is bound here, in the test, precisely because the fixture's
  // whole point is that nothing listens on this port for the rest of the run.
  const { createServer } = await import("node:http");
  const server = createServer((_req, res) => {
    res.setHeader("content-type", "text/html");
    res.end('<!doctype html><html><body><h1 id="marker">AF_DEADPORT_REVIVED</h1></body></html>');
  });
  try {
    await new Promise<void>((resolve) => server.listen(DEAD_PORT, "127.0.0.1", resolve));
    await retry.click();
    await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText("AF_DEADPORT_REVIVED", {
      timeout: 15_000,
    });
    await expect(fallback).toBeHidden();
    // ...and item B in reverse: a live frame's Open ↗ is worth opening again, so
    // showFrame restores the bar link showDead withdrew.
    await expect(page.locator("a.af-webpane-open"), "Open ↗ returns once the frame is live").toBeVisible();
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});

test("item C (Codex P2): a probe that never resolves reaches the fallback within the timeout, not a blank pane", REAL_FIXTURE, async () => {
  // The bug: probeWebTab's fetch had no client timeout, so a loopback target that
  // ACCEPTS the connection but never sends headers left load() awaiting forever — the
  // pane blank, no fallback, no Retry. The fix bounds the probe with an AbortController
  // and treats a timeout like a transport failure: dead.
  //
  // Simulated by routing the proxy path to HANG (never fulfilled), which is exactly
  // "accepts but never answers" from the parent's side. The shrunk fallback ms is also
  // the probe's abort deadline (the caller passes webFallbackMs into the probe), so the
  // abort fires fast and deterministically.
  await page.evaluate(() => {
    (window as unknown as { __afWebtabFallbackMs: number }).__afWebtabFallbackMs = 500;
  });
  // Hang every webtab request — the PROBE (no token) and, were it ever reached, the
  // frame src. Never fulfilled: the connection is open but no response ever comes.
  await page.route("**/v1/webtab/**", () => {
    /* intentionally never fulfill/abort — the fetch hangs until the client aborts it */
  });
  try {
    await row(page, SESSION_DEAD).click();
    await expect(page.locator(".af-main.af-main-term")).toBeVisible();
    const deadTab = tabByLabel(page, "deadport");
    await expect(deadTab).toHaveCount(1, { timeout: 15_000 });
    await deadTab.click();

    // Force a fresh probe through the ↻ control rather than relying on the tab
    // mounting fresh: the preceding test leaves this exact tab mounted, so a re-select
    // alone re-runs nothing. ↻ always calls load(), which arms a new probe — and the
    // route hangs it, so only the AbortController's timeout can end it.
    await page.locator(".af-term-host .af-webpane-reload").click();

    // With the fix, the abort fires at ~500ms and the designed fallback appears. If the
    // probe were still unbounded (the bug) the pane would stay blank forever and this
    // would time out — which is exactly the failure the fix prevents.
    const fallback = page.locator(".af-term-host .af-webpane-fallback.af-webpane-dead");
    await expect(fallback, "a hung probe must reach the fallback, not hang the pane").toBeVisible({ timeout: 10_000 });
    await expect(fallback.locator(".af-webpane-fallback-retry")).toBeVisible();
    // And no frame is left navigated (the probe never returned ok — showDead drops src).
    const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
    await expect(frame).toBeHidden();
    expect(await frame.getAttribute("src")).toBeNull();
  } finally {
    await page.unroute("**/v1/webtab/**");
    // Restore the default probe/timeout window so a later test's proxied probe isn't
    // held to this test's deliberately-tiny deadline.
    await page.evaluate(() => {
      delete (window as unknown as { __afWebtabFallbackMs?: number }).__afWebtabFallbackMs;
    });
  }
});

test("#1813: a refused rename surfaces the daemon's OWN message, verbatim, in the toast", REAL_FIXTURE, async () => {
  // The daemon's refusals (agent/shell rename, new_index 0, archived, remote) are all
  // UNREACHABLE through this UI by construction: the affordance is withheld for a tab
  // the daemon would refuse, and the insertion math clamps a reorder off slot 0. That
  // gating is the feature — but it also means the error path has no natural trigger,
  // so the envelope is forced here instead. What is under test is THIS client's
  // surfacing, not the daemon's validation (which its own Go tests cover): a tab op
  // has no modal, so a swallowed error would leave a rename that silently did nothing.
  //
  // The literal that matters is "[object Object]": the {data,error} envelope carries
  // error as an OBJECT, and interpolating it directly is the repo's own historical bug
  // (see api.ts errorText). This asserts the real message wins instead.
  const REFUSAL = "cannot rename a tab on archived session \"probe-order\"; restore it first (af sessions restore)";
  await page.unroute("**/v1/RenameTab");
  await page.route("**/v1/RenameTab", (route) =>
    route.fulfill({
      status: 400,
      contentType: "application/json",
      body: JSON.stringify({ data: null, error: { message: REFUSAL } }),
    }),
  );

  await row(page, SESSION_ORDER).click();
  await expect(tabByLabel(page, "storefront")).toHaveCount(1, { timeout: 15_000 });
  await renameTabViaUI(page, "storefront", "doesnt-matter");

  // The daemon's sentence, not a generic "rename failed" and not "[object Object]".
  const toast = page.locator(".af-toast.af-toast-show");
  await expect(toast).toContainText(REFUSAL);
  await expect(toast).not.toContainText("[object Object]");
  // A refused rename leaves the tab exactly as it was — nothing was applied
  // optimistically, so there is no wrong name to roll back.
  await expect(tabByLabel(page, "storefront")).toHaveCount(1);
  await expect(tabByLabel(page, "doesnt-matter")).toHaveCount(0);

  await page.unroute("**/v1/RenameTab");
});

test("#1900: ↻ cache-busts a PROXIED preview; an external frame's URL is left untouched", REAL_FIXTURE, async () => {
  // Re-assigning the same URL is not a guarantee of fresh content — the browser's HTTP
  // cache, or any intermediary, may answer from a stale entry, which is exactly the
  // page ↻ exists to escape. A URL that differs per attempt cannot be.
  // probe-vite's tab, NOT probe-web's "preview": the #1779 ordinal-shift test above
  // deletes "preview" (the tab-consuming order the entry script warns about), so it no
  // longer exists by the time this runs. probe-vite is kept separate precisely to be
  // immune to that, and its target is proxied all the same.
  await row(page, SESSION_VITE).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await tabByLabel(page, "vite").click();

  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  // The INITIAL mount stays CLEAN: a fresh preview has nothing stale to escape, and
  // the normal address should read as the normal address.
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\//);
  expect(await frame.getAttribute("src"), "a first mount must not be cache-busted").not.toContain("_afreload");

  // ↻ once: the URL changes — that difference IS the mechanism.
  await page.locator(".af-webpane-reload").click();
  await expect(frame).toHaveAttribute("src", /_afreload=\d+/, { timeout: 15_000 });
  const first = await frame.getAttribute("src");
  // ...and the busted URL still really resolves through the proxy to the dev server,
  // so the param buys freshness without costing the preview. This is the half that
  // would catch an `_afreload` the daemon or the upstream chokes on.
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(VITE_MARKER, {
    timeout: 15_000,
  });

  // ↻ again: the param is REPLACED, not accumulated. A URL that grew an extra
  // _afreload per press would still load fine, so only counting them catches it.
  await page.locator(".af-webpane-reload").click();
  await expect.poll(() => frame.getAttribute("src"), { timeout: 15_000 }).not.toBe(first);
  const second = (await frame.getAttribute("src")) ?? "";
  expect(second.match(/_afreload=/g)?.length, "one _afreload param, not one per press").toBe(1);
  expect(second).toContain("/v1/webtab/");

  // The EXTERNAL case: cache-busting was always excluded for external targets (a
  // presigned / CDN-token URL signs over its query string, so a param would 403 it).
  // Item A made that exclusion structural — an external target is never framed inline
  // at all — which is the strongest possible form of "never busted": there is no
  // framed URL to append a param to.
  //
  // This half used to PRESS ↻ here and assert the src stayed null. It no longer can,
  // and that is the point rather than an inconvenience: ↻ is not offered on an
  // external pane at all now. It used to sit there and, when pressed, re-show this
  // same fallback — no fetch, no navigation, no change, no explanation — which reads
  // as af being broken and gives the user nothing to act on. Withdrawing it makes
  // "never busted" visible rather than merely true, and the gesture this asserted is
  // gone with it. The escape that DOES work (the open link) is asserted by the item A
  // test above.
  // probe-web's "external" tab is never closed by any test, unlike its "preview".
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await tabByLabel(page, "external").click();
  const externalFallback = page.locator(".af-term-host .af-webpane-fallback.af-webpane-external");
  await expect(externalFallback).toBeVisible({ timeout: 15_000 });
  expect(await frame.getAttribute("src"), "an external target is never framed, so never busted").toBeNull();
  await expect(
    page.locator(".af-term-host .af-webpane-reload:visible"),
    "an external pane must not offer a ↻ that cannot reload",
  ).toHaveCount(0);
  // ...and with no ↻ to press and no src to bust, the fallback simply remains the
  // surface. Re-asserted after the check above so this still pins the end state.
  await expect(externalFallback).toBeVisible();
  expect(await frame.getAttribute("src")).toBeNull();
});

test("#1900: the cache-buster is unique across a pane REMOUNT — ↻ never re-issues a URL the cache already holds", REAL_FIXTURE, async () => {
  // The sibling of the dead-↻ bug, and the same shape: the control looks like it
  // worked and didn't. #1900 shipped the buster as a counter local to the pane MOUNT,
  // but the browser's HTTP cache outlives the pane — so a remount reset it to 0 and
  // the next ↻ re-requested a URL the cache still had an entry for, serving exactly
  // the stale page ↻ exists to escape.
  //
  // The essential property, and the only thing this asserts: two reloads separated by
  // a pane recreate must not produce the same URL.
  await row(page, SESSION_VITE).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await tabByLabel(page, "vite").click();

  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });

  // ↻ once. This is the URL the browser has now actually fetched — and cached.
  await page.locator(".af-webpane-reload").click();
  await expect(frame).toHaveAttribute("src", /_afreload=\d+/, { timeout: 15_000 });
  const first = (await frame.getAttribute("src")) ?? "";

  // REMOUNT the pane, by binding it to the agent tab (a terminal) and back. This is a
  // real mountWebPane teardown+rebuild — the same one a target change, an archive flip
  // (#1809) or a session switch performs — and it is what reset the per-mount counter.
  // Driven through tab clicks rather than by calling the mount, so the production gate
  // that decides WHETHER to remount (reconcile's staleAddress/pane.term test) is part
  // of what is under test: a test that reached past it could pass on code where no
  // remount ever happens.
  await page.locator(".af-tabbar .af-tab").first().click();
  await expect(frame).toHaveCount(0, { timeout: 15_000 });
  await tabByLabel(page, "vite").click();
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  // A remount is a fresh mount, so its address is clean again — the same rule as a
  // first mount, and the reason the reset LOOKED harmless.
  expect(await frame.getAttribute("src"), "a remount must not be cache-busted").not.toContain("_afreload");

  // ↻ after the remount: the URL must be one this browser has never fetched. A counter
  // scoped to the mount restarts and re-issues `first` VERBATIM here, which the cache
  // can answer from the entry the first ↻ created.
  await page.locator(".af-webpane-reload").click();
  await expect(frame).toHaveAttribute("src", /_afreload=\d+/, { timeout: 15_000 });
  const second = (await frame.getAttribute("src")) ?? "";
  expect(second, "a reload after a remount must not re-issue the first reload's URL").not.toBe(first);
  // Still exactly one param, and still the same tab's proxied address — the nonce is
  // the only thing that moved.
  expect(second.match(/_afreload=/g)?.length, "one _afreload param, not one per press").toBe(1);
  expect(second).toContain("/v1/webtab/");

  // ...and the busted URL still really resolves through the proxy to the dev server.
  // A nonce the daemon or the upstream chokes on would be a different — and worse —
  // bug than the one this fixes, so freshness is asserted together with the preview
  // surviving, never alone.
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(VITE_MARKER, {
    timeout: 15_000,
  });
});

// ===========================================================================
// #1909: whose 502 is it? The probe's verdict must follow the ANSWER, not the
// status — and must not be steerable by the server it is probing.
//
// Both tests reuse the dead-port fixture's tab by BINDING a real server on
// DEAD_PORT for their duration. That is the point: the tab, the daemon proxy and
// the client are all the real ones, and the only variable is what the dev server
// says. Each closes its server in a finally, restoring the "nothing listens on
// DEAD_PORT" absence the earlier fixtures depend on.
// ===========================================================================

/** Binds `handler` on DEAD_PORT for the duration of `body`, then restores the port's
 *  emptiness — which is itself the fixture every other SESSION_DEAD test rests on. */
async function withDeadPortServer(
  handler: (req: import("node:http").IncomingMessage, res: import("node:http").ServerResponse) => void,
  body: () => Promise<void>,
): Promise<void> {
  const { createServer } = await import("node:http");
  const server = createServer(handler);
  await new Promise<void>((resolve) => server.listen(Number(DEAD_PORT), "127.0.0.1", resolve));
  try {
    await body();
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
}

/** Selects the dead-port tab and forces a FRESH probe through ↻. The tab is left
 *  mounted by earlier tests, so a re-select alone re-probes nothing — only ↻ calls
 *  load() again, which is what these tests need to observe. */
async function reprobeDeadTab(page: Page): Promise<void> {
  await row(page, SESSION_DEAD).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const deadTab = tabByLabel(page, "deadport");
  await expect(deadTab).toHaveCount(1, { timeout: 15_000 });
  await deadTab.click();
  await page.locator(".af-term-host .af-webpane-reload").click();
}

test("#1909: an UPSTREAM's own 502 renders the app's page — only af's own 502 shows the fallback", REAL_FIXTURE, async () => {
  // The bug: the daemon proxy forwards upstream statuses UNCHANGED, so an app that
  // answers 502 on its own (a framework proxy whose backend is down, a local gateway
  // error page) was byte-identical, by status, to af's "nothing is listening" 502. The
  // client keyed on the bare status and suppressed both — hiding a page the app really
  // served behind af's dead-server fallback, and telling the developer their dev server
  // was down when it was answering.
  //
  // The marker header separates them: af's ErrorHandler REPLACES the response when the
  // upstream never answered, so it is set exactly when af generated the failure.
  //
  // Both halves run here, back to back, against the SAME tab and the SAME port. That
  // is what makes it a real test of the distinction rather than of two situations: the
  // only thing that changes between them is who produced the 502.
  const APP_502_MARKER = "AF_APP_OWN_502_PAGE";
  const fallback = page.locator(".af-term-host .af-webpane-fallback.af-webpane-dead");
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");

  await withDeadPortServer(
    (_req, res) => {
      // The app ANSWERED — with its own gateway error page. This is a page to render.
      res.statusCode = 502;
      res.setHeader("content-type", "text/html");
      res.end(`<!doctype html><html><body><h1 id="marker">${APP_502_MARKER}</h1></body></html>`);
    },
    async () => {
      await reprobeDeadTab(page);
      // THE ASSERTION: the app's own error page is rendered. Before the marker, this
      // 502 was read as af's and the pane showed the fallback instead.
      await expect(
        page.frameLocator(".af-webframe").locator("#marker"),
        "an upstream 502 is the APP's page to render",
      ).toHaveText(APP_502_MARKER, { timeout: 15_000 });
      await expect(fallback, "af must not claim the dev server is dead when it answered").toBeHidden();
    },
  );

  // The other half, same tab, same port: with nothing listening the 502 is AF's own,
  // it carries the marker, and the fallback is exactly right. This is what proves the
  // fix narrowed the verdict rather than simply disabling it.
  await reprobeDeadTab(page);
  await expect(fallback, "an af-generated 502 must still reach the dead-server fallback").toBeVisible({
    timeout: 15_000,
  });
  await expect(fallback).toContainText(`No dev server is answering at localhost:${DEAD_PORT} yet.`);
  await expect(frame).toBeHidden();
  expect(await frame.getAttribute("src"), "the frame must never be pointed at a dead target").toBeNull();
});

test("#1909/probe: a preview that redirects CROSS-ORIGIN cannot steer the probe off-origin", REAL_FIXTURE, async () => {
  // fetch follows redirects by DEFAULT, so the health probe — a request the PARENT
  // document makes — could be sent wherever the probed server pointed it. An OAuth/SSO
  // login is the everyday shape: the dev server 302s to an identity provider on another
  // origin, and the proxy passes a foreign Location through untouched (it rewrites only
  // refs naming the upstream it proxies).
  //
  // Two things were wrong with following it. The probe rests on being same-origin to
  // the SPA, and the very thing it probes got to choose otherwise — a probe steerable
  // by its subject. And if the destination disallows CORS the fetch REJECTS, so we
  // called a perfectly live dev server dead and hid it behind the fallback, though the
  // frame would have followed that redirect happily and rendered the login page.
  //
  // The login server here is cross-ORIGIN by port (127.0.0.1:<free>, not :8892) and
  // sends no Access-Control-Allow-Origin — exactly the everyday case, and entirely
  // local, so nothing here touches the network.
  const LOGIN_MARKER = "AF_CROSS_ORIGIN_LOGIN_PAGE";
  const { createServer } = await import("node:http");

  // Every request the off-origin server sees, tagged with what the browser was
  // fetching FOR. The distinction is the whole assertion: a `document`/`iframe` hit is
  // the frame legitimately following the redirect itself, which is correct and
  // expected; an `empty` hit is fetch() — the PROBE — and means it was steered.
  const offOriginHits: string[] = [];
  const login = createServer((req, res) => {
    offOriginHits.push(String(req.headers["sec-fetch-dest"] ?? "unknown"));
    // No CORS headers, deliberately: a followed probe fails here, which is the bug.
    res.setHeader("content-type", "text/html");
    res.end(`<!doctype html><html><body><h1 id="marker">${LOGIN_MARKER}</h1></body></html>`);
  });
  await new Promise<void>((resolve) => login.listen(0, "127.0.0.1", resolve));
  const loginPort = (login.address() as import("node:net").AddressInfo).port;

  try {
    await withDeadPortServer(
      (_req, res) => {
        // A live dev server whose answer is "go log in over there".
        res.statusCode = 302;
        res.setHeader("location", `http://127.0.0.1:${loginPort}/login`);
        res.end();
      },
      async () => {
        await reprobeDeadTab(page);

        // THE ASSERTION: the dev server is alive and redirecting, so the pane must NOT
        // show the dead-server fallback. Before the fix the probe followed the 302,
        // the cross-origin response failed the CORS check, the fetch rejected, and this
        // live server was reported dead.
        const fallback = page.locator(".af-term-host .af-webpane-fallback.af-webpane-dead");
        await expect(fallback, "a redirecting dev server is answering — it is not dead").toBeHidden({
          timeout: 15_000,
        });

        // ...and the frame followed the redirect ITSELF, which is what a preview does.
        await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(LOGIN_MARKER, {
          timeout: 15_000,
        });

        // The steering claim, asserted directly rather than inferred from the outcome:
        // the parent's probe never reached the foreign origin. Only the frame did.
        expect(
          offOriginHits.filter((dest) => dest !== "iframe" && dest !== "document"),
          `the probe must not be steerable off-origin; off-origin server saw ${JSON.stringify(offOriginHits)}`,
        ).toEqual([]);
        expect(offOriginHits.length, "the FRAME is entitled to follow the redirect").toBeGreaterThan(0);
      },
    );
  } finally {
    await new Promise<void>((resolve) => login.close(() => resolve()));
  }
});

test("#1929/#1971: a rename, a reorder and a close from the web carry the tab's STABLE id", REAL_FIXTURE, async () => {
  // The web client already HOLDS the stable tab id (#1738) and threw it away, resolving
  // the tab locally and then sending only its NAME. A name is not an identity — it is
  // the very thing a rename changes — so a request racing a rename from another window
  // or the CLI could name a tab that no longer exists, or one that has since taken the
  // name. The daemon now resolves tab_id first and keeps the name as its fallback.
  //
  // Asserted against the daemon's OWN projection of the id, not merely "non-empty": the
  // failure this guards against is sending a plausible-but-wrong key (a synthesized
  // `kind:name` identity is exactly that, and would 404 a legacy tab), which any
  // non-empty check would wave through.
  await row(page, SESSION_ORDER).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-tabbar .af-tab")).not.toHaveCount(0, { timeout: 15_000 });

  /** The daemon's real id for the tab named `name` in SESSION_ORDER, read from the
   *  Snapshot the SPA itself mirrors (loopback is tokenless here, #1696). */
  const realTabId = async (name: string): Promise<string> =>
    page.evaluate(
      async ({ title, tabName }) => {
        const resp = await fetch("/v1/Snapshot", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ repo_id: "" }),
        });
        const env = (await resp.json()) as {
          data: { instances: { title: string; tabs?: { name: string; id?: string }[] }[] };
        };
        const inst = env.data.instances.find((i) => i.title.includes(title));
        return inst?.tabs?.find((t) => t.name === tabName)?.id ?? "";
      },
      { title: SESSION_ORDER, tabName: name },
    );

  // The bar's first non-agent tab: whatever the preceding rename/reorder tests left,
  // every non-agent tab in this session is a renameable (web/process) kind.
  const labels = await tabLabels(page);
  const victim = labels[1];
  expect(victim, "SESSION_ORDER must still have a non-agent tab to rename").toBeTruthy();
  const victimId = await realTabId(victim);
  expect(victimId, "the fixture tab must have a stable id (#1738) — otherwise this proves nothing").not.toBe("");

  // --- rename ---
  let renameBody: Record<string, unknown> = {};
  await page.route("**/v1/RenameTab", async (route) => {
    renameBody = route.request().postDataJSON() as Record<string, unknown>;
    await route.continue(); // let the REAL daemon apply it — the id must work end to end
  });
  const renamed = `idcarry-${Date.now()}`;
  await renameTabViaUI(page, victim, renamed);
  // It really renamed — so the id the client sent is one the daemon resolved, not just
  // a string that rode along in an ignored field.
  await expect(tabByLabel(page, renamed), "the rename must take effect through the real daemon").toHaveCount(1, {
    timeout: 15_000,
  });
  await page.unroute("**/v1/RenameTab");
  expect(renameBody.tab_id, "a rename must carry the tab's stable id").toBe(victimId);
  expect(renameBody.tab_name, "…and the name, which is the daemon's documented fallback").toBe(victim);
  expect(renameBody.new_name).toBe(renamed);

  // --- reorder ---
  // The id must survive the gesture too: the drag reports ORDINALS, and turning an
  // ordinal back into the right tab is precisely what the id is for.
  const after = await tabLabels(page);
  expect(after.length, "a reorder needs at least two non-agent tabs to permute").toBeGreaterThan(2);
  const moverId = await realTabId(after[1]);
  let reorderBody: Record<string, unknown> = {};
  await page.route("**/v1/ReorderTab", async (route) => {
    reorderBody = route.request().postDataJSON() as Record<string, unknown>;
    await route.continue();
  });
  const res = await dragTabWithinBar(page, after[1], after[2], "after");
  expect(res.dropAllowed, "the bar must accept a reorder drop").toBe(true);
  await expect
    .poll(() => tabLabels(page), { timeout: 15_000 })
    .toEqual([after[0], after[2], after[1], ...after.slice(3)]);
  await page.unroute("**/v1/ReorderTab");
  expect(reorderBody.tab_id, "a reorder must carry the MOVED tab's stable id").toBe(moverId);
  expect(reorderBody.tab_name, "…alongside the name fallback").toBe(after[1]);
  expect(reorderBody.new_index, "the wire field is new_index, read in the FINAL roster").toBe(2);

  // --- close (#1971) ---
  // The verb that needs the id most: rename and reorder misroute into a wrong label or
  // a wrong slot, both undoable, while close kills the tab's tmux session and whatever
  // was running in it. The daemon reissues a freed name to the next tab that asks for
  // one, so a name-keyed close racing another window's close+create destroys a tab this
  // user never addressed.
  const beforeClose = await tabLabels(page);
  const doomed = beforeClose[1];
  const doomedId = await realTabId(doomed);
  expect(doomedId, "the tab being closed must have a stable id — otherwise this proves nothing").not.toBe("");
  let closeBody: Record<string, unknown> = {};
  await page.route("**/v1/CloseTab", async (route) => {
    closeBody = route.request().postDataJSON() as Record<string, unknown>;
    await route.continue(); // the REAL daemon must resolve the id, not just receive it
  });
  await tabByLabel(page, doomed).locator(".af-tab-close").click();
  // It really closed — and only it. A close that resolved the wrong tab would still
  // shrink the bar by one, so assert the surviving ROSTER, not just the count.
  await expect
    .poll(() => tabLabels(page), { timeout: 15_000 })
    .toEqual(beforeClose.filter((l) => l !== doomed));
  await page.unroute("**/v1/CloseTab");
  expect(closeBody.tab_id, "a close must carry the CLOSED tab's stable id").toBe(doomedId);
  expect(closeBody.tab_name, "…alongside the name fallback").toBe(doomed);
});

// ---- Mobile / narrow-viewport pass ------------------------------------------
//
// Sachin's ask: the web UI must be USABLE on a phone — below ~768px the session rail
// auto-collapses to an off-canvas drawer so the terminal gets the full width, a
// hamburger slides it back over as an overlay, and picking a session folds it shut. And
// the classic mobile bug — one overflowing element scrolling the whole page sideways —
// must never happen at a phone or small-tablet width.
//
// These drive a REAL headless Chromium at fixed viewport sizes, each in its OWN context
// so the emulated viewport + fresh localStorage never leak into the shared desktop
// `page` the rest of the file uses. They are the responsive-layout gate: the CSS is
// @media-driven, so only a real sized viewport proves the breakpoint actually engages.

/** Opens the app in a fresh context at the given viewport and waits for the tokenless
 *  shell + a populated rail. Rail rows exist in the DOM even while the mobile drawer is
 *  collapsed (visibility:hidden), so this readiness signal is viewport-agnostic — it
 *  works at a phone width and a desktop width alike. */
async function openAt(browser: Browser, width: number, height: number): Promise<{ ctx: BrowserContext; p: Page }> {
  const ctx = await browser.newContext({ viewport: { width, height } });
  const p = await ctx.newPage();
  await openTokenless(p);
  return { ctx, p };
}

/** The page's horizontal overflow past the viewport, in px — the number that must stay
 *  ≤ 0 (a hair of sub-pixel tolerance) or the page scrolls sideways. */
async function horizontalOverflow(p: Page): Promise<number> {
  return p.evaluate(() => {
    const de = document.documentElement;
    return Math.max(de.scrollWidth, document.body.scrollWidth) - window.innerWidth;
  });
}

/** Waits on the browser's own drawer/descendant animation timeline, then captures the
 *  CURRENT row and all relevant geometry in one page task. Live session events rebuild
 *  the rail with replaceChildren(); resolving action locators and later measuring a row
 *  in separate browser calls can otherwise inspect different row generations. */
async function settledMobileDrawerGeometry(p: Page, title: string) {
  return p.locator(".af-rail").evaluate(async (rail, expectedTitle) => {
    const describeAnimation = (animation: Animation) => {
      const target = animation.effect instanceof KeyframeEffect ? animation.effect.target : null;
      return {
        target: target instanceof Element ? target.getAttribute("class") : null,
        playState: animation.playState,
      };
    };
    const animations = rail.getAnimations({ subtree: true });
    const waitedAnimations = animations.map(describeAnimation);
    await Promise.allSettled(animations.map((animation) => animation.finished));
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));

    const rect = (el: Element) => {
      const box = el.getBoundingClientRect();
      return {
        x: box.x,
        y: box.y,
        width: box.width,
        height: box.height,
        right: box.right,
        bottom: box.bottom,
      };
    };
    const app = rail.closest(".af-app");
    const toggle = app?.querySelector(".af-nav-toggle");
    const rows = Array.from(rail.querySelectorAll<HTMLElement>(".af-row"));
    const matches = rows.filter((candidate) => candidate.textContent?.includes(expectedTitle));
    const selected = matches[0] ?? null;
    const main = selected?.querySelector<HTMLElement>(".af-row-main") ?? null;
    const actions = selected?.querySelector<HTMLElement>(".af-row-actions") ?? null;
    const railStyle = getComputedStyle(rail);

    return {
      appOpen: app?.classList.contains("af-nav-open") ?? false,
      toggleExpanded: toggle?.getAttribute("aria-expanded") ?? null,
      waitedAnimations,
      remainingAnimations: rail.getAnimations({ subtree: true }).map(describeAnimation),
      rail: {
        connected: rail.isConnected,
        rect: rect(rail),
        display: railStyle.display,
        visibility: railStyle.visibility,
        transform: railStyle.transform,
      },
      rows: rows.map((candidate) => ({
        connected: candidate.isConnected,
        text: candidate.textContent,
        rect: rect(candidate),
      })),
      targetMatches: matches.length,
      target:
        selected && main && actions
          ? {
              connected: selected.isConnected,
              rect: rect(selected),
              main: rect(main),
              actions: rect(actions),
              actionStyle: {
                display: getComputedStyle(actions).display,
                opacity: getComputedStyle(actions).opacity,
                visibility: getComputedStyle(actions).visibility,
              },
              buttons: Array.from(actions.querySelectorAll<HTMLButtonElement>("button")).map((button) => ({
                label: button.getAttribute("aria-label"),
                rect: rect(button),
              })),
              actionsInside: actions.getBoundingClientRect().right <= selected.getBoundingClientRect().right + 1,
              overflow: selected.scrollWidth - selected.clientWidth,
            }
          : null,
    };
  }, title);
}

test("#2354 mobile: one slim hamburger/tab row owns the viewport and the drawer stays an overlay", REAL_FIXTURE, async ({
  browser,
}) => {
  for (const width of [320, 375]) {
    await test.step(`${width}px`, async () => {
      const height = 812;
      const { ctx, p } = await openAt(browser, width, height);
      try {
        const app = p.locator(".af-app");
        const toggle = p.locator(".af-nav-toggle");
        const rail = p.locator(".af-rail");
        const viewNav = p.locator(".af-viewnav");
        const project = p.locator(".af-project-switch");
        const more = p.locator(".af-appbar-more");

        await toggle.click();
        await expect(app).toHaveClass(/af-nav-open/);
        await row(p, SESSION_ORDER).click();
        await expect(app).not.toHaveClass(/af-nav-open/);
        await expect(app, "a real selected session enables the condensed mobile shell").toHaveClass(/af-session-selected/);
        await expect(p.locator(".af-main.af-main-term")).toBeVisible();
        await expect(p.locator(".af-term-head-main"), "the title is not a redundant mobile row").toBeHidden();
        await expect(viewNav, "top-level navigation reserves no closed-state row").toBeHidden();
        await expect(project, "project chrome reserves no closed-state row").toBeHidden();
        await expect(more, "secondary app chrome reserves no closed-state row").toBeHidden();

        const geometry = () =>
          p.evaluate(() => {
            const rect = (selector: string) => {
              const box = document.querySelector(selector)!.getBoundingClientRect();
              return {
                x: box.x,
                y: box.y,
                width: box.width,
                height: box.height,
                right: box.right,
                bottom: box.bottom,
                centerY: box.top + box.height / 2,
              };
            };
            return {
              app: rect(".af-app"),
              toggle: rect(".af-nav-toggle"),
              head: rect(".af-term-head"),
              tabs: rect(".af-tabbar"),
              host: rect(".af-term-host"),
            };
          });
        const closed = await geometry();
        expect(Math.abs(closed.toggle.centerY - closed.tabs.centerY), "hamburger and tabs share one row").toBeLessThanOrEqual(1);
        expect(closed.head.height, "the mobile control row stays slim").toBeLessThan(64);
        expect(closed.host.y - closed.app.y, "session content begins immediately after that one row").toBeLessThan(64);
        expect(closed.host.bottom, "the pane reaches the mobile viewport bottom").toBeCloseTo(closed.app.bottom, 0);

        // The hamburger reveals navigation as an overlay. The app/project controls
        // become reachable only inside that transient state, while the pane keeps the
        // exact same usable rectangle underneath it.
        await toggle.click();
        await expect(app).toHaveClass(/af-nav-open/);
        await expect(rail).toBeVisible();
        await expect(viewNav).toBeVisible();
        await expect(project).toBeVisible();
        await expect(more).toBeVisible();
        const opened = await geometry();
        expect(opened.host, "opening the overlay must not resize or displace the pane").toEqual(closed.host);

        // A drawer-only view control is genuinely operable, and leaving Sessions
        // dismisses the drawer. Returning keeps the selected terminal and restores
        // the condensed row rather than stacking the old appbar back above it.
        await viewNav.getByRole("tab", { name: "Tasks", exact: true }).click();
        await expect(app).not.toHaveClass(/af-nav-open/);
        await expect(p.locator(".af-tasks")).toBeVisible();
        await p.getByRole("tab", { name: "Sessions", exact: true }).click();
        await expect(p.locator(".af-main.af-main-term")).toBeVisible();
        await expect(app).toHaveClass(/af-session-selected/);
        await expect(viewNav).toBeHidden();

        // Outside tap is the other drawer exit. Aim at the scrim's exposed right
        // edge because the left portion is intentionally covered by the rail.
        await toggle.click();
        await expect(app).toHaveClass(/af-nav-open/);
        const scrim = p.locator(".af-nav-scrim");
        const scrimBox = await scrim.boundingBox();
        expect(scrimBox, "the open drawer exposes an outside-tap surface").not.toBeNull();
        await scrim.click({ position: { x: scrimBox!.width - 4, y: scrimBox!.height / 2 } });
        await expect(app).not.toHaveClass(/af-nav-open/);

        // Switch to a real process terminal from the strip, then change the usable
        // height. ResizeObserver owns orientation/viewport changes; the row count is
        // the product signal that FitAddon actually consumed the new geometry.
        const processTab = p.locator('.af-tabbar .af-tab:has([data-icon="terminal"])').first();
        await expect(processTab, "the seeded session keeps a real terminal tab for the mobile strip").toBeVisible();
        await processTab.click();
        await expect(processTab).toHaveClass(/af-tab-active/);
        const rows = () => p.locator(".af-term-host .xterm-rows > div").count();
        await expect.poll(rows, { message: "the selected mobile tab has fitted terminal rows" }).toBeGreaterThan(0);
        const beforeResizeRows = await rows();
        await p.setViewportSize({ width, height: height + 120 });
        await expect
          .poll(rows, { message: "a taller mobile viewport refits the active terminal without manual recovery" })
          .toBeGreaterThan(beforeResizeRows);
        expect(await horizontalOverflow(p), "the condensed shell never widens the phone").toBeLessThanOrEqual(1);
      } finally {
        await ctx.close();
      }
    });
  }
});

test("mobile (375px): the rail auto-collapses to a drawer; the hamburger reveals it as an overlay and picking a session folds it shut", REAL_FIXTURE, async ({
  browser,
}) => {
  const { ctx, p } = await openAt(browser, 375, 667);
  const app = p.locator(".af-app");
  const rail = p.locator(".af-rail");
  const toggle = p.locator(".af-nav-toggle");

  // Collapsed by default: the hamburger is offered, the rail is off-canvas (not
  // visible), and the terminal/main pane owns the full width.
  await expect(toggle).toBeVisible();
  await expect(rail).not.toBeVisible();
  await expect(app).not.toHaveClass(/af-nav-open/);
  const widths = () =>
    p.evaluate(() => ({
      app: document.querySelector(".af-app")!.getBoundingClientRect().width,
      main: document.querySelector(".af-main")!.getBoundingClientRect().width,
    }));
  let w = await widths();
  expect(w.main, "the main pane fills the width while the rail is collapsed").toBeGreaterThan(w.app - 2);

  // The hamburger slides the drawer in. It overlays the terminal (the main pane keeps
  // its full width underneath) rather than reflowing it, and it sits within the viewport
  // without covering the whole screen.
  await toggle.click();
  await expect(app).toHaveClass(/af-nav-open/);
  await expect(rail).toBeVisible();
  // Poll the settled position: the drawer slides in over ~0.22s, so a boundingBox read
  // the instant it turns visible catches it mid-transform. Poll until its left edge
  // reaches the viewport.
  await expect
    .poll(async () => (await rail.boundingBox())?.x ?? -9999, {
      message: "the open drawer slides fully into the viewport",
    })
    .toBeGreaterThanOrEqual(-1);
  const railBox = await rail.boundingBox();
  expect(railBox!.width, "the drawer does not swallow the whole screen").toBeLessThan(375);
  w = await widths();
  expect(w.main, "opening the drawer overlays — it does not shrink the terminal").toBeGreaterThan(w.app - 2);

  // Picking a session closes the drawer and reveals that session's terminal.
  await p.locator(".af-rail-list .af-row", { hasText: SESSION_A }).click();
  await expect(app).not.toHaveClass(/af-nav-open/);
  await expect(rail).not.toBeVisible();
  await expect(p.locator(".af-main.af-main-term")).toBeVisible();

  // Re-open the narrow drawer with a selection: both quiet glyphs fit inside the row
  // without squeezing its title away or widening the page.
  //
  // Controlled regression for #2331: browser/daemon load can leave a descendant's
  // layout work behind the drawer's immediate visibility flip. Hold the flexible cell
  // at zero basis on the browser animation timeline. The old visibility-only check
  // deterministically read it mid-animation; the settled snapshot below waits on the
  // actual work rather than sleeping or retrying the assertion.
  await row(p, SESSION_A).evaluate((el) => {
    const main = el.querySelector<HTMLElement>(".af-row-main")!;
    main.animate(
      [
        { flexBasis: "0px", flexGrow: "0" },
        { flexBasis: "0px", flexGrow: "1" },
      ],
      { duration: 1_000, easing: "steps(1, end)" },
    );
  });
  await toggle.click();
  const fit = await settledMobileDrawerGeometry(p, SESSION_A);
  const diagnostic = `settled mobile drawer geometry:\n${JSON.stringify(fit, null, 2)}`;
  expect(fit.appOpen, diagnostic).toBe(true);
  expect(fit.toggleExpanded, diagnostic).toBe("true");
  expect(fit.remainingAnimations.filter((animation) => animation.playState !== "finished"), diagnostic).toEqual([]);
  expect(fit.rail.connected, diagnostic).toBe(true);
  expect(fit.rail.visibility, diagnostic).toBe("visible");
  expect(fit.rail.rect.x, diagnostic).toBeGreaterThanOrEqual(-1);
  expect(fit.targetMatches, diagnostic).toBe(1);
  expect(fit.target, diagnostic).not.toBeNull();
  if (!fit.target) throw new Error(diagnostic);
  expect(
    fit.target.buttons.map((button) => button.label),
    diagnostic,
  ).toEqual([`Archive session “${SESSION_A}”`, `Kill session “${SESSION_A}”`]);
  expect(fit.target.buttons.every((button) => button.rect.width > 0 && button.rect.height > 0), diagnostic).toBe(true);
  expect(fit.target.actionStyle, diagnostic).toMatchObject({ display: "flex", opacity: "1", visibility: "visible" });
  expect(fit.target.main.width, diagnostic).toBeGreaterThan(80);
  expect(fit.target.actionsInside, diagnostic).toBe(true);
  expect(fit.target.overflow, diagnostic).toBeLessThanOrEqual(1);

  await ctx.close();
});

test("#2226 mobile (375px): drawer dismissal follows action intent, not click propagation", REAL_FIXTURE, async ({ browser }) => {
  const { ctx, p } = await openAt(browser, 375, 667);
  const app = p.locator(".af-app");
  const rail = p.locator(".af-rail");
  const toggle = p.locator(".af-nav-toggle");
  const modal = p.locator(".af-modal-card");
  const lifecyclePosts: string[] = [];
  p.on("request", (request) => {
    if (/\/v1\/(?:KillSession|ArchiveSession|RestoreSession)$/.test(request.url())) {
      lifecyclePosts.push(request.url());
    }
  });

  const openDrawer = async () => {
    await toggle.click();
    await expect(app).toHaveClass(/af-nav-open/);
    await expect(rail).toBeVisible();
  };
  const expectDrawerClosed = async () => {
    await expect(app).not.toHaveClass(/af-nav-open/);
    await expect(rail).not.toBeVisible();
  };

  // Create lives in the rail HEAD, outside the old delegated list handler. Opening
  // its form is a leave-the-rail action: fold the drawer first so the form can take
  // focus instead of sitting under the overlay.
  await openDrawer();
  await p.locator(".af-rail-new").click();
  await expectDrawerClosed();
  await expect(modal).toBeVisible();
  const modalLayer = await p.evaluate(() => {
    const appbar = document.querySelector(".af-appbar")!;
    const host = document.querySelector(".af-modal-host")!;
    const covered = [".af-project-switch", ".af-appbar-more"].map((selector) => {
      const box = document.querySelector(selector)!.getBoundingClientRect();
      const hit = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
      return hit?.closest(".af-modal-backdrop") !== null;
    });
    return {
      appbarZ: Number.parseInt(getComputedStyle(appbar).zIndex, 10),
      modalZ: Number.parseInt(getComputedStyle(host).zIndex, 10),
      covered,
    };
  });
  expect(modalLayer.modalZ, "aria-modal content owns the top interaction layer").toBeGreaterThan(modalLayer.appbarZ);
  expect(modalLayer.covered, "the modal backdrop blocks every appbar action").toEqual([true, true]);
  const titleInput = modal.locator('input[aria-label="Session title"]');
  await titleInput.click();
  await expect(titleInput).toBeFocused();
  await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(modal).toBeHidden();

  // Selecting a row leaves the rail for its terminal. This used to pass only because
  // its click happened to bubble to railList; the same action must stay explicit.
  await openDrawer();
  await row(p, SESSION_A).click();
  await expectDrawerClosed();
  await expect(p.locator(".af-main.af-main-term")).toBeVisible();

  // Row actions MUST keep stopPropagation (otherwise they also select the row), so
  // Kill and Archive have to dismiss by intent before opening their own confirms.
  await openDrawer();
  await railAction(p, SESSION_A, "Kill session").click();
  await expectDrawerClosed();
  await expect(modal).toContainText(`Kill ${SESSION_A}?`);
  await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(modal).toBeHidden();
  expect(lifecyclePosts, "cancelling Kill must not post a lifecycle mutation").toEqual([]);
  await expect(row(p, SESSION_A)).toHaveCount(1);

  await openDrawer();
  await railAction(p, SESSION_A, "Archive session").click();
  await expectDrawerClosed();
  await expect(modal).toContainText(`Archive ${SESSION_A}?`);
  await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(modal).toBeHidden();
  expect(lifecyclePosts, "cancelling Archive must not post a lifecycle mutation").toEqual([]);
  await expect(row(p, SESSION_A)).toHaveCount(1);

  // Filtering is a stay-in-the-rail action. Both opening the filter and toggling the
  // archived projection keep the drawer and menu open so several states can be set.
  await openDrawer();
  await p.locator(".af-rail-filter").click();
  await expect(p.locator(".af-filter-menu")).toBeVisible();
  await expect(app).toHaveClass(/af-nav-open/);
  await filterItem(p, "archived").click();
  await expect(filterItem(p, "archived")).toHaveAttribute("aria-checked", "true");
  await expect(p.locator(".af-filter-menu")).toBeVisible();
  await expect(app).toHaveClass(/af-nav-open/);

  // Restore is the archived row's version of the same lifecycle action. On touch,
  // an unselected row has no hover reveal: select the now-visible archived row,
  // let that navigation close the drawer, then reopen it so the selected-row action
  // is exposed. This is the real phone path pinned by #2223's reveal model.
  await expect(row(p, SESSION_B)).toHaveClass(/af-row-archived/);
  await row(p, SESSION_B).click();
  await expectDrawerClosed();
  await expect(row(p, SESSION_B)).toHaveClass(/af-row-selected/);
  await openDrawer();
  const restore = railAction(p, SESSION_B, "Restore session");
  await expect(restore).toBeVisible();
  await restore.click();
  await expectDrawerClosed();
  await expect(modal).toContainText(`Restore ${SESSION_B}?`);
  await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(modal).toBeHidden();
  expect(lifecyclePosts, "cancelling Restore must not post a lifecycle mutation").toEqual([]);

  // The escape hatch remains location-based by design: the scrim's action IS drawer
  // dismissal. Tap its exposed right edge (the left side sits behind the drawer).
  await openDrawer();
  const scrim = p.locator(".af-nav-scrim");
  const scrimBox = await scrim.boundingBox();
  expect(scrimBox, "the open mobile drawer exposes a scrim").not.toBeNull();
  await scrim.click({ position: { x: scrimBox!.width - 5, y: scrimBox!.height / 2 } });
  await expectDrawerClosed();

  await ctx.close();
});

test("#2227 mobile appbar: project context wins scarce width at 320px and 375px", async ({ browser }, testInfo) => {
  const LONG_PROJECT = "project-with-a-deliberately-long-distinguishing-name";
  const LONG_ROOT = `/work/${LONG_PROJECT}`;

  for (const width of [320, 375]) {
    for (const theme of ["light", "dark"] as const) {
      await test.step(`${width}px · ${theme}`, async () => {
        const ctx = await browser.newContext({ viewport: { width, height: 812 } });
        await ctx.addInitScript(
          ({ root, savedTheme }) => {
            localStorage.setItem("af-project", root);
            localStorage.setItem("af-theme", savedTheme);
          },
          { root: LONG_ROOT, savedTheme: theme },
        );
        const p = await ctx.newPage();
        await p.route("**/v1/Snapshot", async (route) => {
          const resp = await route.fetch();
          const body = await resp.json();
          const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
          const list = snap?.instances ?? [];
          const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
          list.push({
            ...proto,
            id: `synth-mobile-appbar-${width}-${theme}`,
            title: `mobile-appbar-${width}-${theme}`,
            branch: `mobile-appbar-${width}-${theme}`,
            liveness: 2,
            in_flight_op: 0,
            lifecycle_action: "archive",
            worktree: { ...(proto.worktree as Record<string, unknown>), repo_path: LONG_ROOT },
          });
          if (snap) {
            snap.instances = list;
          }
          await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
        });
        await p.goto("/");
        await expect(p.locator(".af-app")).toBeVisible();
        await expect(p.locator(".af-project-switch-name")).toHaveText(LONG_PROJECT);
        await expect(p.locator("html")).toHaveAttribute("data-theme", theme);

        const brand = p.locator(".af-brand");
        const toggle = p.locator(".af-nav-toggle");
        const project = p.locator(".af-project-switch");
        const projectName = p.locator(".af-project-switch-name");
        const more = p.locator(".af-appbar-more");
        const tools = p.locator(".af-appbar-tools");
        const viewTabs = p.locator(".af-viewtab");
        await expect(brand, "decoration yields before project context").toBeHidden();
        await expect(toggle).toBeVisible();
        await expect(project).toBeVisible();
        await expect(more, "secondary appbar tools collapse behind one touch target").toBeVisible();
        await expect(tools).toBeHidden();

        const primary = await p.evaluate(() => {
          const rect = (selector: string) => {
            const box = document.querySelector(selector)!.getBoundingClientRect();
            return { left: box.left, right: box.right, top: box.top, height: box.height, width: box.width };
          };
          return {
            toggle: rect(".af-nav-toggle"),
            views: rect(".af-viewnav"),
            project: rect(".af-project-switch"),
            more: rect(".af-appbar-more"),
          };
        });
        for (const [name, box] of Object.entries(primary)) {
          expect(box.left, `${name} starts inside ${width}px`).toBeGreaterThanOrEqual(0);
          expect(box.right, `${name} ends inside ${width}px`).toBeLessThanOrEqual(width);
          expect(box.height, `${name} keeps a comfortable tap height`).toBeGreaterThanOrEqual(44);
        }
        expect(primary.project.width, "project context owns nearly the whole second row").toBeGreaterThan(200);
        expect(Math.abs(primary.toggle.top - primary.views.top), "top-level navigation shares one top edge").toBeLessThanOrEqual(1);
        expect(Math.abs(primary.more.top - primary.project.top), "project and More share one top edge").toBeLessThanOrEqual(1);
        expect(primary.project.top, "the project row follows the top-level navigation row").toBeGreaterThan(
          primary.toggle.top + primary.toggle.height,
        );

        // Visual and focus order must agree at the responsive breakpoint. Positive
        // tabindex or CSS-only reordering would still leave assistive technology
        // jumping between rows, so walk the native DOM sequence end to end.
        await toggle.focus();
        for (let i = 0; i < 3; i++) {
          await p.keyboard.press("Tab");
          await expect(viewTabs.nth(i), `view tab ${i + 1} follows the drawer toggle`).toBeFocused();
        }
        await p.keyboard.press("Tab");
        await expect(project, "project follows the view row in DOM and pixels").toBeFocused();
        await p.keyboard.press("Tab");
        await expect(more, "More follows the project in DOM and pixels").toBeFocused();

        const truncation = await projectName.evaluate((el) => {
          const css = getComputedStyle(el);
          return {
            clientWidth: el.clientWidth,
            scrollWidth: el.scrollWidth,
            overflow: css.overflow,
            textOverflow: css.textOverflow,
            whiteSpace: css.whiteSpace,
          };
        });
        expect(truncation.scrollWidth, "the long name genuinely exceeds its visible slot").toBeGreaterThan(
          truncation.clientWidth,
        );
        expect(truncation).toMatchObject({ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" });
        expect(await horizontalOverflow(p), "the closed drawer never widens the page").toBeLessThanOrEqual(1);

        // The project popover itself must remain inside the phone and offer several
        // projects. Clicking the current long-name item proves it is hit-testable.
        await project.click();
        const projectMenu = p.locator(".af-project-menu");
        await expect(projectMenu).toBeVisible();
        expect(
          await projectMenu.locator(".af-project-item").count(),
          "the phone menu keeps several projects operable",
        ).toBeGreaterThanOrEqual(3);
        const menuBox = await projectMenu.boundingBox();
        expect(menuBox, "project menu has phone geometry").not.toBeNull();
        expect(menuBox!.x).toBeGreaterThanOrEqual(0);
        expect(menuBox!.x + menuBox!.width).toBeLessThanOrEqual(width);

        // Disclosure exclusivity belongs to the actions, not pointer bubbling. The
        // More click stops propagation and keyboard activation has no mousedown, so
        // both directions would otherwise leave overlapping panels open (#2226's
        // event-path gotcha in miniature).
        await more.click();
        await expect(projectMenu).toBeHidden();
        await expect(tools).toBeVisible();
        await expect(more).toHaveAttribute("aria-expanded", "true");
        await project.focus();
        await p.keyboard.press("Enter");
        await expect(tools).toBeHidden();
        await expect(more).toHaveAttribute("aria-expanded", "false");
        await expect(projectMenu).toBeVisible();
        await projectItem(p, LONG_PROJECT).click();
        await expect(projectMenu).toBeHidden();

        // Rare chrome remains operable rather than consuming the project row.
        await more.click();
        await expect(tools).toBeVisible();
        await expect(tools.locator(`.af-theme-opt[data-theme-opt="${theme}"]`)).toHaveClass(/af-theme-opt-active/);
        const disconnect = tools.getByRole("button", { name: "Disconnect" });
        await expect(disconnect).toBeVisible();
        const toolsBox = await tools.boundingBox();
        expect(toolsBox, "More popover has phone geometry").not.toBeNull();
        expect(toolsBox!.x).toBeGreaterThanOrEqual(0);
        expect(toolsBox!.x + toolsBox!.width).toBeLessThanOrEqual(width);
        for (const control of [disconnect, ...(await tools.locator(".af-theme-opt").all())]) {
          const box = await control.boundingBox();
          expect(box, "every More action has geometry").not.toBeNull();
          expect(box!.height, "More actions keep comfortable tap heights").toBeGreaterThanOrEqual(40);
        }
        await p.keyboard.press("Escape");
        await expect(tools).toBeHidden();
        await expect(more).toHaveAttribute("aria-expanded", "false");
        await expect(more).toBeFocused();

        await testInfo.attach(`2227-${width}-${theme}-drawer-closed`, {
          body: await p.screenshot(),
          contentType: "image/png",
        });

        // Opening the drawer must not reflow the bar, and its z-index must not cover
        // the project menu: selecting the same project while open proves both.
        await toggle.click();
        await expect(p.locator(".af-app")).toHaveClass(/af-nav-open/);
        const openProjectBox = await project.boundingBox();
        expect(openProjectBox, "drawer-open project switch has geometry").not.toBeNull();
        expect(openProjectBox!.x).toBeCloseTo(primary.project.left, 0);
        expect(openProjectBox!.width).toBeCloseTo(primary.project.width, 0);
        await project.click();
        await expect(projectMenu).toBeVisible();
        await projectItem(p, LONG_PROJECT).click();
        await expect(projectMenu).toBeHidden();
        expect(await horizontalOverflow(p), "the open drawer never widens the page").toBeLessThanOrEqual(1);

        await testInfo.attach(`2227-${width}-${theme}-drawer-open`, {
          body: await p.screenshot(),
          contentType: "image/png",
        });
        await ctx.close();
      });
    }
  }
});

for (const width of [375, 768]) {
  test(`mobile (${width}px): the page never scrolls sideways, drawer closed or open`, REAL_FIXTURE, async ({ browser }) => {
    const { ctx, p } = await openAt(browser, width, 812);

    expect(await horizontalOverflow(p), "the page must not scroll sideways with the drawer closed").toBeLessThanOrEqual(
      1,
    );

    // Open the drawer and re-check: an off-canvas layer that widened the page would show
    // up here.
    await p.locator(".af-nav-toggle").click();
    await expect(p.locator(".af-app")).toHaveClass(/af-nav-open/);
    expect(await horizontalOverflow(p), "…nor with the drawer slid open").toBeLessThanOrEqual(1);

    await ctx.close();
  });
}

test("desktop (1280px): the mobile drawer never engages — the rail stays in view and the hamburger is hidden", REAL_FIXTURE, async ({
  browser,
}) => {
  // The desktop guard: the responsive rules are scoped to a @media (max-width: 768px)
  // block, so above it the layout must be exactly as it was — rail in the flow, no
  // hamburger. This is the "don't regress desktop" assertion.
  const { ctx, p } = await openAt(browser, 1280, 800);
  await expect(p.locator(".af-rail")).toBeVisible();
  await expect(p.locator(".af-nav-toggle")).toBeHidden();
  await expect(p.locator(".af-brand")).toBeVisible();
  await expect(p.locator(".af-appbar-more")).toBeHidden();
  await expect(p.locator(".af-appbar-tools")).toBeVisible();
  await expect(p.locator(".af-app")).not.toHaveClass(/af-nav-open/);
  await ctx.close();
});
