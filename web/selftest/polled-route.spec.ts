import { expect, test } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { stopPolledRoutes } from "./polled-route.js";

// No daemon: the server holds a real route.fetch() until context teardown aborts
// it. The last assertion is guaranteed to run with that fetch still in flight.
test("#4080: a polled Snapshot fetch can outlive the last assertion", async ({ browser }) => {
  let received!: () => void;
  const fetching = new Promise<void>(resolve => { received = resolve; });
  let polls = 0;
  const server = createServer((request, response) => {
    if (request.url !== "/v1/Snapshot") response.end("<title>ready</title>");
    else if (++polls <= 2) response.end(JSON.stringify({ data: { instances: [{ tabs: [] }] } }));
    else received();
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("expected TCP server");
  const context = await browser.newContext();
  const page = await context.newPage();
  let calls = 0;
  let finished!: () => void;
  const handled = new Promise<void>(resolve => { finished = resolve; });
  try {
    await page.route("**/v1/Snapshot", async route => {
      // Unrouting may forward the browser request upstream again; count handler
      // invocations separately from HTTP requests when signaling completion.
      const call = ++calls;
      try {
        const response = await route.fetch();
        const body = await response.json();
        body.data.instances[0].tabs.push({ id: "legacy", url: "http://127.1:3000" });
        await route.fulfill({ response, json: body });
      } finally {
        if (call === 3) finished();
      }
    });
    await page.goto(`http://127.0.0.1:${address.port}`);
    // A one-shot route would lose the legacy projection on the second poll.
    for (let poll = 0; poll < 2; poll++) {
      const body = await page.evaluate(async () => (await fetch("/v1/Snapshot")).json());
      expect(body.data.instances[0].tabs).toEqual([{ id: "legacy", url: "http://127.1:3000" }]);
    }
    await page.evaluate(() => { void fetch("/v1/Snapshot").catch(() => {}); });
    await fetching;
    await expect(page).toHaveTitle("ready");
    console.log("#4080 witness: last assertion passed with route.fetch() in flight");
    await stopPolledRoutes(context);
    await context.close();
    await handled;
  } finally {
    await context.close();
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  }
});
