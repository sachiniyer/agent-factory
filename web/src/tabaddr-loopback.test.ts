import { readFileSync } from "node:fs";
import { test } from "node:test";
import assert from "node:assert/strict";
import { isLoopbackWebUrl, iframeIsProxied, iframeRoute, blockedWebTargetMessage, canUsePreviewOrigin } from "./tabaddr.js";
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
const vectors: { host: string; proxied: boolean; direct_unsafe: boolean }[] = JSON.parse(
  readFileSync(new URL("../../parity/web-loopback.json", import.meta.url), "utf8"),
);
for (const { host, proxied, direct_unsafe } of vectors) {
  test(`Go/TS loopback parity: ${host}`, () => {
    assert.equal(isLoopbackWebUrl(`http://${host}/`), proxied);
    let browserLoopback = false;
    try {
      const url = new URL(`http://${host}/`);
      browserLoopback = canUsePreviewOrigin(url);
    } catch { /* malformed numeric hosts cannot resolve to browser loopback */ }
    assert.equal(!proxied && browserLoopback, direct_unsafe);
    if (direct_unsafe) assert.equal(iframeRoute({ kind: TabKind.Web, target: `http://${host}/` }), "blocked");
    assert.equal(canUsePreviewOrigin({ protocol: "http:", hostname: host }), proxied);
  });
}
test("daemon projection overrides the compatibility predicate in both directions", () => {
  assert.equal(iframeIsProxied({ kind: TabKind.Web, target: "https://example.com", web_proxied: true }), true);
  assert.equal(iframeIsProxied({ kind: TabKind.Web, target: "http://localhost", web_proxied: false }), false);
});

for (const host of ["127.1", "0177.0.0.1", "2130706433"]) {
  test(`legacy shorthand is blocked: ${host}`, () => {
    assert.equal(iframeRoute({ kind: TabKind.Web, target: `http://${host}:3000` }), "blocked");
    assert.equal(iframeRoute({ kind: TabKind.Web, target: `http://${host}:3000`, web_proxied: false }), "blocked");
    assert.ok(blockedWebTargetMessage(`http://${host}:3000`).includes(host));
  });
}
test("legacy external host is safe for direct navigation", () => {
  assert.equal(iframeRoute({ kind: TabKind.Web, target: "http://127.example.com/" }), "direct");
});
