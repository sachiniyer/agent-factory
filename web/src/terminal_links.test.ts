// Pins the web terminal's OSC 8 link click: it opens http(s) links in a new tab
// with no confirm() dialog, opens nothing for any other scheme, and shows the
// real URL while the pointer is over a link.

import { test } from "node:test";
import assert from "node:assert/strict";

import { terminalLinkHandler } from "./terminal_links.js";

const range = { start: { x: 1, y: 1 }, end: { x: 1, y: 1 } };
const click = new Event("click") as MouseEvent;

function clicks(texts: string[]): [string, string, string][] {
  const opened: [string, string, string][] = [];
  const handler = terminalLinkHandler((url, target, features) => opened.push([url, target, features]), () => {});
  for (const t of texts) handler.activate(click, t, range);
  return opened;
}

test("an http or https link opens in a new tab, detached from af", () => {
  assert.deepEqual(clicks(["https://github.com/x/y/pull/1", "http://localhost:8080/"]), [
    ["https://github.com/x/y/pull/1", "_blank", "noopener,noreferrer"],
    ["http://localhost:8080/", "_blank", "noopener,noreferrer"],
  ]);
});

// xterm itself drops non-http(s) OSC 8 links before they become clickable
// (allowNonHttpProtocols is unset), so this pins our second guard behind it.
test("a non-http link opens nothing", () => {
  assert.deepEqual(clicks(["javascript:alert(1)", " javascript:alert(1)", "JaVaScRiPt:alert(1)", "file:///etc/passwd", "data:text/html,x", "vbscript:x", "blob:https://a.test/x", "https://", "//evil.test/", "not a url", ""]), []);
});

test("hover shows the link's real URL and leave clears it", () => {
  const tips: string[] = [];
  const handler = terminalLinkHandler(() => {}, (tip) => tips.push(tip));
  handler.hover!(click, "https://example.test/real", range);
  handler.leave!(click, "https://example.test/real", range);
  assert.deepEqual(tips, ["https://example.test/real", ""]);
});

test("hover shows a lookalike Unicode host in its punycode form", () => {
  const tips: string[] = [];
  const handler = terminalLinkHandler(() => {}, (tip) => tips.push(tip));
  handler.hover!(click, "https://\u0430pple.com/", range);
  assert.deepEqual(tips, ["https://xn--pple-43d.com/"]);
});
