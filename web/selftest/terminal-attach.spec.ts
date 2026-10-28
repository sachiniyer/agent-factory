import { expect, test, type WebSocketRoute } from "@playwright/test";
import { decode, encode, Op, ptyOutFrame } from "../src/frame.js";

test("#3914: terminal accepts focus and input before its first PTY frame", async ({ page }) => {
  let stream: WebSocketRoute | undefined;
  let input = "";
  const frames: Op[] = [];
  await page.routeWebSocket(url => url.pathname.endsWith("/stream"), socket => {
    stream = socket;
    // The transport opens but the PTY deliberately sends nothing until the
    // assertions below. Input is intercepted, never sent to a real agent.
    socket.onMessage(message => {
      if (typeof message === "string") return;
      const frame = decode(message);
      frames.push(frame.op);
      if (frame.op === Op.Input) input += new TextDecoder().decode(frame.data);
    });
  });
  await page.goto("/");
  const title = process.env.AF_WEB_SESSION_A ?? "probe-a";
  await page.locator(".af-row").filter({ has: page.locator(".af-row-title", { hasText: title }) }).click();
  const surface = page.locator(".af-term-host .xterm");
  await expect(surface).toBeVisible();
  const textarea = surface.locator(".xterm-helper-textarea");
  await expect(textarea).toBeFocused();
  expect((await surface.locator(".xterm-rows").textContent())?.trim()).toBe("");
  await page.keyboard.type("early-input");
  await expect.poll(() => input).toBe("early-input");
  expect(frames[0]).toBe(Op.Resize);
  expect(stream).toBeDefined();
  stream!.send(Buffer.from(encode(ptyOutFrame(new TextEncoder().encode("first frame arrived")))));
  await expect(surface).toContainText("first frame arrived");
  await expect(textarea).toBeFocused();
});

test("#3914: a routed terminal starts attaching before the initial 1000-row rail", async ({ page, request }) => {
  const response = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
  const payload = await response.json();
  const selected = payload.data.instances.find((session: { title: string }) =>
    session.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  expect(selected?.id).toBeTruthy();
  const sessions = [selected, ...Array.from({ length: 999 }, (_, i) => ({
    ...selected, id: `rail-fixture-${i}`, title: `rail-fixture-${i}`, is_root: false,
    liveness: 3, in_flight_op: 0, lifecycle_action: undefined, can_kill: false,
  }))];
  await page.route("**/v1/Snapshot", route => route.fulfill({ json: {
    ...payload, data: { ...payload.data, instances: sessions },
  } }));
  await page.routeWebSocket(url => url.pathname.endsWith("/stream"), () => {});
  await page.addInitScript(() => {
    const WS = window.WebSocket;
    window.WebSocket = class extends WS {
      constructor(url: string | URL, protocols?: string | string[]) {
        const attach = new URL(String(url), location.href).pathname.endsWith("/stream");
        const rows = document.querySelectorAll(".af-rail-list .af-row").length;
        const mounted = Boolean(document.querySelector(".af-term-host .xterm-helper-textarea"));
        super(url, protocols);
        if (attach) (window as unknown as { attachProbe: unknown }).attachProbe = { rows, mounted };
      }
    };
  });
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await expect(page.locator(".af-term-host .xterm")).toBeVisible();
  await expect(page.locator(".af-rail-list .af-row")).toHaveCount(1000);
  expect(await page.evaluate(() => (window as unknown as { attachProbe: unknown }).attachProbe))
    .toEqual({ rows: 0, mounted: true });
});
