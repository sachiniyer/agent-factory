import { readFileSync } from "node:fs";
import { test } from "node:test";
import assert from "node:assert/strict";
import { isLoopbackWebUrl, iframeIsProxied, canUsePreviewOrigin } from "./tabaddr.js";
import { TabKind } from "./types.js";

test("web mirror agrees with Go on *.localhost", () => {
  assert.equal(isLoopbackWebUrl("http://app.localhost:3000/"), true);
  assert.equal(isLoopbackWebUrl("https://deep.app.localhost./x"), true);
  assert.equal(iframeIsProxied({ kind: TabKind.Web, target: "http://app.localhost:3000/" }), true);
});
test("web mirror agrees with Go on a 127.-prefixed DNS name", () => {
  assert.equal(isLoopbackWebUrl("http://127.example.com/"), false);
});
test("web mirror agrees with Go on IPv4-mapped loopback", () => {
  assert.equal(isLoopbackWebUrl("http://[::ffff:7f00:1]:3000/"), true);
});

// The same vectors and verdicts are checked against Go's serving predicate by
// session/TestWebLoopbackParity. Neither implementation owns a private copy.
const vectors: { host: string; proxied: boolean }[] = JSON.parse(
  readFileSync(new URL("../../parity/web-loopback.json", import.meta.url), "utf8"),
);
for (const { host, proxied } of vectors) {
  test(`Go/TS loopback parity: ${host}`, () => {
    assert.equal(isLoopbackWebUrl(`http://${host}/`), proxied);
    assert.equal(canUsePreviewOrigin({ protocol: "http:", hostname: host }), proxied);
  });
}
test("daemon projection overrides the compatibility predicate in both directions", () => {
  assert.equal(iframeIsProxied({ kind: TabKind.Web, target: "https://example.com", web_proxied: true }), true);
  assert.equal(iframeIsProxied({ kind: TabKind.Web, target: "http://localhost", web_proxied: false }), false);
});
