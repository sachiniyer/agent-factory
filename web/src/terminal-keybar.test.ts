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

test("soft control input consumes a one-shot while terminal replies do not", () => {
  const state = new StickyModifiers();
  state.tap("Ctrl", 0);
  assert.equal(state.input("\r", "user"), "\r");
  assert.equal(state.state("Ctrl"), "off");
  assert.equal(state.input("a"), "a");

  state.tap("Ctrl", 1000);
  assert.equal(state.input("\x1b[?1;2c", "terminal"), "\x1b[?1;2c");
  assert.equal(state.state("Ctrl"), "once");
  assert.equal(state.input("a"), "\x01");
});

test("hardware arrow input applies and consumes sticky modifiers", () => {
  for (const [modifier, arrow, bytes] of [
    ["Ctrl", "\x1b[A", "\x1b[1;5A"],
    ["Alt", "\x1bOD", "\x1b[1;3D"],
  ] as const) {
    const state = new StickyModifiers();
    state.tap(modifier, 0);
    assert.equal(state.input(arrow, "user"), bytes);
    assert.equal(state.state(modifier), "off");
    assert.equal(state.input("a", "user"), "a");
  }
});

test("pointerdown prevents focus transfer before acting", () => {
  let prevented = false;
  keybarPointerDown({ preventDefault() { prevented = true; } }, () => assert.equal(prevented, true));
});

test("one-row keybar uses six primary targets and a replacement arrows row at 360px", async () => {
  const { KEYBAR_ROWS } = await import("./terminal-keybar.js");
  assert.deepEqual(KEYBAR_ROWS, [["Ctrl", "Alt", "Esc", "Tab", "^C", "Arrows"], ["More keys", "←", "↑", "↓", "→"]]);
  for (const row of KEYBAR_ROWS) assert.ok(row.length * 44 + (row.length - 1) * 4 + 16 <= 360);
});

test("keyBytes drops ctrl/alt for arrows and specials", () => {
  assert.notEqual(keyBytes("←", false, true), "\x1b[D");
  assert.notEqual(keyBytes("Tab", false, true), "\t");
});

test("an armed one-shot survives a keybar arrow press and hits the NEXT letter", () => {
  const m = new StickyModifiers();
  m.tap("Ctrl", 0);
  // Bar keys use the explicit source path; xterm replies keep input().
  assert.equal(m.key("←"), "\x1b[1;5D");
  assert.equal(m.state("Ctrl"), "off");
  assert.equal(m.input("l"), "l");
});

test("bar keys apply and consume one-shots before the following letter", () => {
  for (const [modifier, key, bytes] of [
    ["Ctrl", "↑", "\x1b[1;5A"], ["Alt", "←", "\x1b[1;3D"],
    ["Alt", "Tab", "\x1b\t"], ["Alt", "Esc", "\x1b\x1b"],
    ["Alt", "^C", "\x1b\x03"], ["Ctrl", "Tab", "\t"],
    ["Ctrl", "Esc", "\x1b"], ["Ctrl", "^C", "\x03"],
  ] as const) {
    const state = new StickyModifiers();
    state.tap(modifier, 0);
    assert.equal(state.key(key), bytes);
    assert.equal(state.state(modifier), "off");
    assert.equal(state.input("ls"), "ls");
  }
});

test("locked Ctrl survives an arrow and combined modifiers use CSI in both cursor modes", () => {
  const state = new StickyModifiers();
  state.tap("Ctrl", 0); state.tap("Ctrl", 100);
  assert.equal(state.key("↑", true), "\x1b[1;5A");
  assert.equal(state.state("Ctrl"), "locked");
  state.tap("Alt", 200);
  assert.equal(state.key("←"), "\x1b[1;7D");
  assert.equal(state.state("Alt"), "off");
  assert.equal(state.state("Ctrl"), "locked");
  for (const app of [false, true]) {
    for (const [key, suffix] of [["↑", "A"], ["↓", "B"], ["→", "C"], ["←", "D"]]) {
      assert.equal(keyBytes(key, true, true, app), `\x1b[1;7${suffix}`);
      assert.equal(keyBytes(key, false, false, app), `\x1b${app ? "O" : "["}${suffix}`);
    }
  }
});

test("resolved bar keys pass through xterm transform without applying locked modifiers twice", () => {
  const state = new StickyModifiers();
  state.tap("Ctrl", 0); state.tap("Ctrl", 100);
  state.tap("Alt", 0); state.tap("Alt", 100);
  for (const [key, bytes] of [["↑", "\x1b[1;7A"], ["Tab", "\x1b\t"], ["Esc", "\x1b\x1b"], ["^C", "\x1b\x03"]]) {
    assert.equal(state.input(state.key(key)), bytes);
    assert.equal(state.state("Ctrl"), "locked");
    assert.equal(state.state("Alt"), "locked");
  }
});
