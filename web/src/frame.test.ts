// The TypeScript half of the cross-language codec contract (#1592 Phase 5 PR1).
// It validates web/src/frame.ts against the SAME fixture the Go test uses
// (agentproto/testdata/frame_vectors.json), so the browser and daemon codecs are
// pinned to byte-identical framing and cannot silently diverge. The Go half is
// agentproto/frame_test.go: TestFrameGoldenVectors — same file, same assertions.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

import { Op, decode, encode, helloFrame, inputFrame, ptyOutFrame, repaintFrame, resizeFrame, type Frame } from "./frame.js";

const here = dirname(fileURLToPath(import.meta.url));
// web/src → repo-root/agentproto/testdata/frame_vectors.json (single source of truth).
const FIXTURE_PATH = join(here, "..", "..", "agentproto", "testdata", "frame_vectors.json");

interface Vector {
  name: string;
  op: "PTY_OUT" | "INPUT" | "RESIZE" | "REPAINT" | "HELLO";
  dataHex?: string;
  rows?: number;
  cols?: number;
  seq?: string; // decimal uint64 string — parsed with BigInt so it never truncates
  wireHex: string;
}

function fromHex(s: string): Uint8Array {
  if (s.length % 2 !== 0) throw new Error(`odd-length hex: ${s}`);
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(s.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

function toHex(b: Uint8Array): string {
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

function frameForVector(v: Vector): Frame {
  switch (v.op) {
    case "PTY_OUT":
      return ptyOutFrame(fromHex(v.dataHex ?? ""));
    case "INPUT":
      return inputFrame(fromHex(v.dataHex ?? ""));
    case "REPAINT":
      return repaintFrame(fromHex(v.dataHex ?? ""));
    case "RESIZE":
      return resizeFrame(v.rows ?? 0, v.cols ?? 0);
    case "HELLO":
      return helloFrame(BigInt(v.seq ?? "0"));
    default:
      throw new Error(`unknown op ${(v as Vector).op}`);
  }
}

function framesEqual(a: Frame, b: Frame): boolean {
  return (
    a.op === b.op &&
    a.rows === b.rows &&
    a.cols === b.cols &&
    a.seq === b.seq &&
    a.data.length === b.data.length &&
    a.data.every((x, i) => x === b.data[i])
  );
}

const fixture = JSON.parse(readFileSync(FIXTURE_PATH, "utf8")) as { vectors: Vector[] };

test("fixture has vectors", () => {
  assert.ok(fixture.vectors.length > 0, "no vectors in fixture");
});

for (const v of fixture.vectors) {
  test(`golden vector: ${v.name}`, () => {
    const f = frameForVector(v);

    // encode(f) must produce the fixture's exact wire bytes.
    assert.equal(toHex(encode(f)), v.wireHex, `encode mismatch for ${v.name}`);

    // decode(wire) must round-trip back to the same logical frame.
    const back = decode(fromHex(v.wireHex));
    assert.ok(framesEqual(back, f), `decode mismatch for ${v.name}: ${JSON.stringify({ f, back }, bigintReplacer)}`);
  });
}

// A couple of targeted OpHello assertions beyond the fixture loop, documenting the
// browser's reason for existing: the in-band start-seq it cannot read off the header.
test("hello carries the start seq as a big-endian uint64", () => {
  const wire = encode(helloFrame(4294967297n)); // 2^32 + 1
  assert.equal(toHex(wire), "040000000100000001");
  assert.equal(decode(wire).op, Op.Hello);
  assert.equal(decode(wire).seq, 4294967297n);
});

test("hello preserves the full uint64 range without truncation", () => {
  const max = 18446744073709551615n; // 2^64 - 1
  assert.equal(decode(encode(helloFrame(max))).seq, max);
});

test("decode rejects an unknown opcode (parity with Go DecodeFrame)", () => {
  assert.throws(() => decode(Uint8Array.from([0x7f, 0x78])), /unknown opcode/);
});

function bigintReplacer(_key: string, value: unknown): unknown {
  return typeof value === "bigint" ? value.toString() : value;
}
