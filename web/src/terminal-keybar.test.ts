import assert from "node:assert/strict";
import { test } from "node:test";
import { keyBytes, StickyModifiers, keybarPointerDown } from "./terminal-keybar.js";

test("terminal key bytes match physical keys", () => {
  for (const [key, bytes] of Object.entries({ Esc: "\x1b", Tab: "\t", "←": "\x1b[D", "↑": "\x1b[A", "↓": "\x1b[B", "→": "\x1b[C", "^C": "\x03" })) {
    assert.equal(keyBytes(key), bytes);
  }
  for (const [key, bytes] of [["c", "\x03"], ["X", "\x18"], ["d", "\x04"]]) assert.equal(keyBytes(key, true), bytes);
  assert.equal(keyBytes("x", false, true), "\x1bx");
  assert.equal(keyBytes("↑", false, false, true), "\x1bOA");
  assert.equal(keyBytes("ß", true), "ß");
});

test("one shot consumes only the next character, including committed composition text", () => {
  const state = new StickyModifiers();
  state.tap("Ctrl", 0);
  assert.equal(state.input("cd"), "\x03d");
  assert.equal(state.input("c"), "c");
  state.tap("Alt", 1000);
  assert.equal(state.input("é中"), "\x1bé中");
  assert.equal(state.input("x"), "x");
});

test("double tap locks; tap unlocks; slow second tap cancels; reset clears both", () => {
  const state = new StickyModifiers();
  state.tap("Ctrl", 0); state.tap("Ctrl", 100);
  assert.equal(state.state("Ctrl"), "locked");
  assert.equal(state.input("cx"), "\x03\x18");
  state.tap("Ctrl", 200);
  assert.equal(state.input("c"), "c");
  state.tap("Alt", 1000); state.tap("Alt", 2000);
  assert.equal(state.state("Alt"), "off");
  state.tap("Ctrl", 3000); state.tap("Alt", 3000);
  assert.equal(state.input("c"), "\x1b\x03");
  state.tap("Ctrl", 4000); state.reset();
  assert.equal(state.state("Ctrl"), "off");
});

test("escape sequences and control bytes do not consume a character modifier", () => {
  const state = new StickyModifiers(); state.tap("Ctrl", 0);
  assert.equal(state.input("\x1b[A"), "\x1b[A");
  assert.equal(state.input("\x03"), "\x03");
  assert.equal(state.input("c"), "\x03");
});

test("pointerdown prevents focus transfer before acting", () => {
  let prevented = false;
  keybarPointerDown({ preventDefault() { prevented = true; } }, () => assert.equal(prevented, true));
});

test("one-row keybar uses six primary targets and a replacement arrows row at 360px", async () => {
  const { KEYBAR_ROWS } = await import("./terminal-keybar.js");
  assert.deepEqual(KEYBAR_ROWS, [["Ctrl", "Alt", "Esc", "Tab", "^C", "Arrows"], ["Back", "←", "↑", "↓", "→"]]);
  for (const row of KEYBAR_ROWS) assert.ok(row.length * 44 + (row.length - 1) * 4 + 16 <= 360);
});
