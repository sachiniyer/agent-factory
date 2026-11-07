// Unit tests for the postMessage wire codec (#6669): the only channel between
// the Docs pane and the opaque-origin render frame is a JSON string, and each
// end must ignore anything off-shape — the shape alone is the gate, because
// the listener decides before it acts.
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  decodeFrameReply,
  decodeRenderRequest,
  encodeFrameReply,
  encodeRenderRequest,
  frameReplyFor,
  renderRequestFor,
} from "./mermaid_wire.js";

test("a render request round-trips as a JSON string", () => {
  const wire = encodeRenderRequest("r7", "flowchart LR\nA-->B", { primaryColor: "#fff" });
  assert.equal(typeof wire, "string");
  const req = decodeRenderRequest(wire);
  assert.equal(req?.t, "af-mmd-render");
  assert.equal(req?.id, "r7");
  assert.equal(req?.source, "flowchart LR\nA-->B");
  assert.deepEqual({ ...req?.theme }, { primaryColor: "#fff" });
});

test("decodeRenderRequest accepts only strings", () => {
  // An object payload is NOT the protocol — structured clone could carry what
  // a string never can, so a well-shaped object still must not decode.
  for (const bad of [
    null,
    undefined,
    42,
    { t: "af-mmd-render", id: "r", source: "x", theme: {} },
    ["af-mmd-render"],
  ]) {
    assert.equal(decodeRenderRequest(bad), undefined);
  }
});

test("decodeRenderRequest rejects malformed and off-shape payloads", () => {
  const cases = [
    "not json",
    "{}",
    JSON.stringify(null),
    JSON.stringify([1, 2]),
    JSON.stringify({ t: "af-mmd-rendered", id: "r" }), // replies are not requests
    JSON.stringify({ t: "af-mmd-ready" }),
    JSON.stringify({ t: "af-mmd-render" }),
    JSON.stringify({ t: "af-mmd-render", id: 1, source: "x", theme: {} }),
    JSON.stringify({ t: "af-mmd-render", id: "r", source: {}, theme: {} }),
    JSON.stringify({ t: "af-mmd-render", id: "r", source: "x" }), // no theme
    JSON.stringify({ t: "af-mmd-render", id: "r", source: "x", theme: null }),
    JSON.stringify({ t: "af-mmd-render", id: "r", source: "x", theme: ["#fff"] }),
    JSON.stringify({ t: "af-mmd-render", id: "r", source: "x", theme: { a: 1 } }),
    JSON.stringify({ t: "af-mmd-render", id: "r", source: "x", theme: { a: null } }),
  ];
  for (const bad of cases) assert.equal(decodeRenderRequest(bad), undefined, bad);
});

test("decodeRenderRequest rebuilds the theme as clean data", () => {
  const wire = '{"t":"af-mmd-render","id":"r","source":"x","theme":{"__proto__":"x","ok":"y"}}';
  const req = decodeRenderRequest(wire);
  assert.equal(req?.theme.ok, "y");
  assert.equal(req?.theme.__proto__, "x", "__proto__ lands as data, never a prototype");
  assert.equal(Object.getPrototypeOf(req?.theme as object), null);
});

test("frame replies round-trip", () => {
  const replies = [
    { t: "af-mmd-ready" },
    { t: "af-mmd-rendered", id: "r3", ok: true, svg: "<svg/>" },
    { t: "af-mmd-rendered", id: "r3", ok: false, error: "bad diagram" },
  ] as const;
  for (const reply of replies) {
    const wire = encodeFrameReply(reply);
    assert.equal(typeof wire, "string");
    assert.deepEqual(decodeFrameReply(wire), reply);
  }
});

test("decodeFrameReply rejects anything off-shape", () => {
  const cases = [
    "not json",
    "{}",
    "42",
    '"af-mmd-ready"',
    JSON.stringify({ t: "af-mmd-render", id: "r", source: "x", theme: {} }), // requests are not replies
    JSON.stringify({ t: "af-mmd-rendered" }),
    JSON.stringify({ t: "af-mmd-rendered", id: 1, ok: true, svg: "<svg/>" }),
    JSON.stringify({ t: "af-mmd-rendered", id: "r", ok: true }), // ok without svg
    JSON.stringify({ t: "af-mmd-rendered", id: "r", ok: true, svg: 7 }),
    JSON.stringify({ t: "af-mmd-rendered", id: "r", ok: false }), // error without error text
    JSON.stringify({ t: "af-mmd-rendered", id: "r", ok: false, error: 3 }),
    JSON.stringify({ t: "af-mmd-rendered", id: "r", ok: "yes", svg: "<svg/>" }),
    JSON.stringify({ t: "af-mmd-other" }),
    { t: "af-mmd-ready" }, // objects never decode
  ];
  for (const bad of cases) assert.equal(decodeFrameReply(bad), undefined, JSON.stringify(bad));
});

test("replies tolerate extra fields — required fields are the protocol", () => {
  const reply = decodeFrameReply(
    JSON.stringify({ t: "af-mmd-rendered", id: "r", ok: true, svg: "<svg/>", extra: 1 }),
  );
  assert.deepEqual(reply, { t: "af-mmd-rendered", id: "r", ok: true, svg: "<svg/>" });
});

// The source gates are the protocol's other half: message SHAPE alone never
// trusted a window (#6669). renderRequestFor is the frame's only path to a
// render (only the parent may drive it) and frameReplyFor is the parent's
// only path to painting (only the frame's own window may answer). Both must
// be removed-proof — a regression where either side skips the source check
// has to fail here, not just read wrong.
test("renderRequestFor gates requests to the parent window alone", () => {
  const parent = { name: "parent" };
  const stranger = { name: "not-parent" };
  const good = encodeRenderRequest("r1", "graph TD;A-->B", { a: "#fff" });
  assert.equal(renderRequestFor({ source: parent, data: good }, parent)?.id, "r1");
  assert.equal(
    renderRequestFor({ source: stranger, data: good }, parent),
    undefined,
    "a well-shaped request from any other window is dropped",
  );
  assert.equal(
    renderRequestFor({ source: null, data: good }, parent),
    undefined,
    "a null source is never the parent",
  );
  assert.equal(
    renderRequestFor({ source: parent, data: "not json" }, parent),
    undefined,
    "the source check does not rescue a bad payload",
  );
});

test("frameReplyFor gates replies to the render frame's window alone", () => {
  const frameWin = { name: "frame" };
  const stranger = { name: "not-the-frame" };
  const good = encodeFrameReply({ t: "af-mmd-rendered", id: "r2", ok: true, svg: "<svg/>" });
  const reply = frameReplyFor({ source: frameWin, data: good }, frameWin);
  assert.deepEqual(reply, { t: "af-mmd-rendered", id: "r2", ok: true, svg: "<svg/>" });
  assert.equal(
    frameReplyFor({ source: stranger, data: good }, frameWin),
    undefined,
    "a well-shaped reply from any other window is dropped",
  );
  assert.deepEqual(
    frameReplyFor({ source: frameWin, data: encodeFrameReply({ t: "af-mmd-ready" }) }, frameWin),
    { t: "af-mmd-ready" },
    "ready decodes through the same gate",
  );
  assert.equal(
    frameReplyFor({ source: stranger, data: encodeFrameReply({ t: "af-mmd-ready" }) }, frameWin),
    undefined,
    "even a ready only counts from the frame's own window",
  );
  assert.equal(
    frameReplyFor({ source: undefined, data: good }, frameWin),
    undefined,
    "a missing source is never the frame",
  );
});
