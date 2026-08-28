// Pins the probe-failure fallback copy to the hop that failed (#3239): a probe the
// browser could not even send to the daemon must not blame the dev server — the one
// component that request never observed. The end-to-end dead path (daemon-marked 502
// → fallback card) lives in the Playwright selftest; this pins the attribution.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

// split.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of which plain
// node can load. Stub them (split_focus.test.ts precedent), then import dynamically
// so the hook is registered first.
register("./browser_stub_loader.mjs", import.meta.url);
const { probeFallbackMsg } = (await import("./split.js")) as {
  probeFallbackMsg: (health: "dead" | "unreachable", host: string) => string;
};

test("dead names the dev server — the daemon was reached and nothing answered behind it", () => {
  assert.equal(probeFallbackMsg("dead", "localhost:5173"), "No dev server is answering at localhost:5173 yet.");
});

test("unreachable names the daemon and leaves the dev server out of it (#3239)", () => {
  const msg = probeFallbackMsg("unreachable", "localhost:5173");
  assert.equal(msg, "Could not reach the daemon to check localhost:5173.");
  assert.ok(!msg.includes("dev server"), "a failure to reach the daemon observed nothing about the dev server");
});
