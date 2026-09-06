import { expect, test } from "@playwright/test";
import { writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";

declare global {
  interface Window {
    perfProbe: {
      values: Record<string, number>; shifts: number; snapshots: number;
      railStart: number; echoStart: number; echo: string;
      sockets: WebSocket[];
    };
  }
}

test("three container measurements at 1000 sessions", async ({ browser }) => {
  const runs: Record<string, number>[] = [];
  for (let run = 0; run < 3; run++) {
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    await context.addInitScript(() => {
      if (!PerformanceObserver.supportedEntryTypes.includes("layout-shift")) {
        throw new Error("Chromium must support layout-shift observation");
      }
      const p = window.perfProbe = {
        values: {} as Record<string, number>, shifts: 0, snapshots: 0,
        railStart: 0, echoStart: 0, echo: "", sockets: [] as WebSocket[],
      };
      // Two animation frames bracket a rendering opportunity after the observable
      // DOM write. All timestamps use the browser's monotonic clock, not RPC time.
      const painted = (done: () => void) => requestAnimationFrame(() => requestAnimationFrame(done));
      new PerformanceObserver(list => {
        for (const entry of list.getEntries()) {
          p.shifts += (entry as PerformanceEntry & { value: number }).value;
        }
      }).observe({ type: "layout-shift", buffered: true });
      const fetch = window.fetch.bind(window);
      window.fetch = async (...args) => {
        const response = await fetch(...args);
        if (String(args[0]).endsWith("/v1/Snapshot")) {
          const json = response.json.bind(response);
          response.json = async () => {
            const data = await json();
            p.railStart = performance.now();
            p.snapshots++;
            return data;
          };
        }
        return response;
      };
      const WS = window.WebSocket;
      window.WebSocket = class extends WS {
        constructor(url: string | URL, protocols?: string | string[]) {
          super(url, protocols);
          p.sockets.push(this);
        }
      };
      document.addEventListener("keydown", event => {
        if (event.key === p.echo) p.echoStart = performance.now();
      }, true);
      const pending = new Set<string>();
      const record = (key: string, start: number) => {
        if (key in p.values || pending.has(key)) return;
        pending.add(key);
        painted(() => { p.values[key] = performance.now() - start; });
      };
      new MutationObserver(() => {
        if (p.railStart && document.querySelectorAll(".af-rail-list .af-row").length === 1000) {
          record("rail_ms", p.railStart);
        }
        const text = document.querySelector(".af-term-host .xterm-rows")?.textContent ?? "";
        if (text.trim() !== "") record("first_terminal_ms", 0);
        if (p.echoStart && text.includes(p.echo)) record("echo_ms", p.echoStart);
      }).observe(document, { childList: true, subtree: true, characterData: true });
    });
    const page = await context.newPage();
    const snapshot = await page.request.post(`${process.env.AF_WEB_BASE_URL}/v1/Snapshot`, { data: { repo_id: "" } });
    expect(snapshot.ok()).toBe(true);
    const payload = await snapshot.json();
    expect(payload.error).toBeFalsy();
    expect(payload.data.instances).toHaveLength(1000);
    const selected = payload.data.instances.find((s: { title: string }) => s.title === "add-json-export");
    const diff = selected.tabs.find((t: { name: string }) => t.name.startsWith("diff"));
    expect(diff.id).toBeTruthy();
    await page.goto("/");
    await expect(page.locator(".af-rail-list .af-row")).toHaveCount(1000);
    await page.locator(".af-rail-list .af-row", { hasText: "add-json-export" }).click();
    await page.waitForFunction(() => "first_terminal_ms" in window.perfProbe.values);
    await page.locator("#app[data-af-resync-settled]").waitFor();
    await expect(page.locator(".af-term-host")).toContainText("review it like any branch");
    const loadShift = await page.evaluate(() => window.perfProbe.shifts);
    // Reconnect the actual event stream: the client fetches and applies a real
    // daemon Snapshot. No browser-side fixture substitutes for the API response.
    const snapshots = await page.evaluate(() => {
      const p = window.perfProbe;
      p.shifts = 0;
      document.querySelector("#app")!.removeAttribute("data-af-resync-settled");
      p.sockets.find(s => s.url.includes("/v1/events") && s.readyState === WebSocket.OPEN)!.close();
      return p.snapshots;
    });
    const nextName = `diff-performance-update-${run}`;
    const renamed = await page.request.post(`${process.env.AF_WEB_BASE_URL}/v1/RenameTab`, {
      data: { id: selected.id, title: selected.title, repo_id: "", tab_id: diff.id, tab_name: diff.name, new_name: nextName },
    });
    expect(renamed.ok()).toBe(true);
    expect((await renamed.json()).error).toBeFalsy();
    await expect(page.locator(".af-tabbar .af-tab", { hasText: nextName })).toBeVisible();
    await page.waitForFunction(n => window.perfProbe.snapshots > n, snapshots);
    await page.locator("#app[data-af-resync-settled]").waitFor();
    await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
    const updateShift = await page.evaluate(() => window.perfProbe.shifts);
    // One real typed byte; a fresh character each run avoids matching scrollback.
    const key = ["~", "^", "%"][run];
    await expect(page.locator(".af-term-host .xterm-rows")).not.toContainText(key);
    await page.locator(".af-term-host .xterm-helper-textarea").focus();
    await page.evaluate(key => { window.perfProbe.echo = key; }, key);
    await page.keyboard.press(key);
    await page.waitForFunction(() => "echo_ms" in window.perfProbe.values);
    runs.push(await page.evaluate(({ loadShift, updateShift }) => ({
      ...window.perfProbe.values, load_shift: loadShift, snapshot_shift: updateShift,
    }), { loadShift, updateShift }));
    await context.close();
  }
  const sessions = execFileSync("tmux", ["list-sessions", "-F", "#{session_name}"], { encoding: "utf8" }).trim().split("\n");
  expect(sessions.length, "storage seeding must not spawn 1000 agents").toBeLessThanOrEqual(10);
  writeFileSync("test-results/web-runs.json", JSON.stringify(runs, null, 2));
});
