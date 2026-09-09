import assert from "node:assert/strict";
import { test } from "node:test";
import {
  decodeKeyBytes, KEYBAR_ROWS, keyBytes, keyBytesDomain, KEY_BYTES_NAMED_KEYS, StickyModifiers, TerminalKeybar,
  keybarPointerDown,
} from "./terminal-keybar.js";

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

test("terminal escape sequences and control bytes do not consume a character modifier", () => {
  const state = new StickyModifiers(); state.tap("Ctrl", 0);
  assert.equal(state.input("\x1b[A", "terminal"), "\x1b[A");
  assert.equal(state.input("\x03", "terminal"), "\x03");
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

test("sticky modifiers combine with xterm hardware arrow modifier bits", () => {
  for (const [modifier, arrow, bytes] of [
    ["Ctrl", "\x1b[1;2A", "\x1b[1;6A"],
    ["Alt", "\x1b[1;5D", "\x1b[1;7D"],
  ] as const) {
    const state = new StickyModifiers();
    state.tap(modifier, 0);
    assert.equal(state.input(arrow, "user"), bytes);
    assert.equal(state.state(modifier), "off");
  }

  const reply = new StickyModifiers();
  reply.tap("Ctrl", 0);
  assert.equal(reply.input("\x1b[1;2A", "terminal"), "\x1b[1;2A");
  assert.equal(reply.state("Ctrl"), "once");
});

test("sticky Alt prefixes user-origin hardware controls", () => {
  for (const control of ["\t", "\x1b"] as const) {
    const state = new StickyModifiers();
    state.tap("Alt", 0);
    assert.equal(state.input(control, "user"), "\x1b" + control);
    assert.equal(state.state("Alt"), "off");
  }
});

test("the decoder closes over the complete keyBytes domain", () => {
  const booleans = [false, true];
  const namedKeys = new Set(KEY_BYTES_NAMED_KEYS);
  let keys = 0;
  for (const key of keyBytesDomain()) {
    keys++;
    const codePoint = key.codePointAt(0);
    const scalar = codePoint !== undefined && key.length === (codePoint > 0xffff ? 2 : 1);
    // Outside the 7-bit control-fold range, Ctrl and cursor mode are identity
    // dimensions. Check both extrema once, then walk their full Cartesian
    // product using those proven equivalence classes instead of 35M encodes.
    if (!namedKeys.has(key) && scalar && codePoint > 127) {
      const plain = keyBytes(key, false, false, false);
      const prefixed = keyBytes(key, false, true, false);
      if (keyBytes(key, true, false, true) !== plain || keyBytes(key, true, true, true) !== prefixed)
        assert.fail(`non-ASCII encoder dimensions changed for ${JSON.stringify(key)}`);
      const decodedPlain = decodeKeyBytes(plain);
      const decodedPrefixed = decodeKeyBytes(prefixed);
      assert.ok(decodedPlain && decodedPrefixed, `decode Unicode scalar U+${codePoint.toString(16)}`);
      const mergedPlain = keyBytes(decodedPlain.key, decodedPlain.ctrl, decodedPlain.alt, decodedPlain.applicationCursor);
      const mergedAlt = keyBytes(decodedPlain.key, decodedPlain.ctrl, true, decodedPlain.applicationCursor);
      const retainedAlt = keyBytes(decodedPrefixed.key, decodedPrefixed.ctrl,
        decodedPrefixed.alt, decodedPrefixed.applicationCursor);
      if (mergedPlain !== plain || mergedAlt !== prefixed || retainedAlt !== prefixed)
        assert.fail(`Unicode inverse changed for U+${codePoint.toString(16)}`);
      for (const ctrl of booleans) for (const alt of booleans) for (const applicationCursor of booleans) {
        for (const stickyCtrl of booleans) for (const stickyAlt of booleans) {
          const expected = alt || stickyAlt ? prefixed : plain;
          const actual = alt ? retainedAlt : stickyAlt ? mergedAlt : mergedPlain;
          if (actual !== expected)
            assert.fail(JSON.stringify({ key, ctrl, alt, applicationCursor, stickyCtrl, stickyAlt }));
        }
      }
      continue;
    }
    for (const ctrl of booleans) for (const alt of booleans) for (const applicationCursor of booleans) {
      const encoded = keyBytes(key, ctrl, alt, applicationCursor);
      const decoded = decodeKeyBytes(encoded);
      assert.ok(decoded, `decode ${JSON.stringify({ key, ctrl, alt, applicationCursor, encoded })}`);
      assert.equal(keyBytes(decoded.key, decoded.ctrl, decoded.alt, decoded.applicationCursor), encoded);
      for (const stickyCtrl of booleans) for (const stickyAlt of booleans) {
        const expected = keyBytes(key, ctrl || stickyCtrl, alt || stickyAlt, applicationCursor);
        const actual = keyBytes(decoded.key, decoded.ctrl || stickyCtrl,
          decoded.alt || stickyAlt, decoded.applicationCursor);
        if (actual !== expected)
          assert.equal(actual, expected, JSON.stringify({ key, ctrl, alt, applicationCursor, stickyCtrl, stickyAlt }));
      }
    }
  }
  assert.equal(keys, KEY_BYTES_NAMED_KEYS.length + 0x110000 - 0x800,
    "named encoder keys plus every Unicode scalar");
});

test("sticky modifiers merge into recognized user CSI and SS3 sequence shapes", () => {
  for (const [modifier, input, expected] of [
    ["Ctrl", "\x1b[H", "\x1b[1;5H"],
    ["Ctrl", "\x1b[F", "\x1b[1;5F"],
    ["Ctrl", "\x1b[3~", "\x1b[3;5~"],
    ["Alt", "\x1b[2~", "\x1b[2;3~"],
    ["Ctrl", "\x1b[5~", "\x1b[5;5~"],
    ["Alt", "\x1b[6~", "\x1b[6;3~"],
    ["Ctrl", "\x1bOP", "\x1b[1;5P"],
    ["Alt", "\x1b[15~", "\x1b[15;3~"],
    ["Ctrl", "\x1b[1;10H", "\x1b[1;14H"],
  ] as const) {
    const bare = new StickyModifiers();
    assert.equal(bare.input(input, "user"), input);
    const armed = new StickyModifiers();
    armed.tap(modifier, 0);
    assert.equal(armed.input(input, "user"), expected, JSON.stringify(input));
    assert.equal(armed.state(modifier), "off");
  }

  const prefixedAlt = new StickyModifiers();
  prefixedAlt.tap("Ctrl", 0);
  assert.equal(prefixedAlt.input("\x1b\x1b[H", "user"), "\x1b[1;7H");
});

test("terminal-origin CSI replies bypass sequence rewriting", () => {
  const state = new StickyModifiers();
  state.tap("Ctrl", 0);
  assert.equal(state.input("\x1b[1;2H", "terminal"), "\x1b[1;2H");
  assert.equal(state.state("Ctrl"), "once");
});

test("terminal-origin control replies bypass decoding", () => {
  const state = new StickyModifiers();
  state.tap("Alt", 0);
  assert.equal(state.input("\x18", "terminal"), "\x18");
  assert.equal(state.state("Alt"), "once");
});

test("sticky modifiers combine with complementary hardware letter chords", () => {
  for (const [sticky, hardware, expected] of [
    ["Alt", "\x18", "\x1b\x18"],
    ["Ctrl", "\x1bx", "\x1b\x18"],
    ["Alt", "\x03", "\x1b\x03"],
    ["Ctrl", "\x1bC", "\x1b\x03"],
  ] as const) {
    const bare = new StickyModifiers();
    assert.equal(bare.input(hardware, "user"), hardware);
    const armed = new StickyModifiers();
    armed.tap(sticky, 0);
    assert.equal(armed.input(hardware, "user"), expected);
    assert.equal(armed.state(sticky), "off");
  }
});

test("sticky modifiers encode known xterm user control bytes", () => {
  for (const [sticky, input, expected] of [
    ["Alt", "\r", "\x1b\r"],
    ["Alt", "\x7f", "\x1b\x7f"],
    ["Ctrl", "\x7f", "\x08"],
  ] as const) {
    const state = new StickyModifiers();
    state.tap(sticky, 0);
    assert.equal(state.input(input, "user"), expected);
    assert.equal(state.state(sticky), "off");
    assert.equal(state.input("a", "user"), "a");
  }
});

test("backtab has no generic sticky CSI encoding", () => {
  for (const sticky of ["Ctrl", "Alt"] as const) {
    const state = new StickyModifiers();
    state.tap(sticky, 0);
    assert.equal(state.input("\x1b[Z", "user"), "\x1b[Z");
    assert.equal(state.state(sticky), "off");
  }
});

test("a deferred 229 marker ignores parser replies before its textarea diff", () => {
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const textarea = { value: "ab" };
  const keybar = Object.assign(Object.create(TerminalKeybar.prototype) as object, {
    modifiers, textarea, rows: [], buttons: new Map(), focused: true,
    phone: { matches: true }, userInput: false, inputSource: "terminal", userInputGeneration: 0,
    deferred229Generation: 0,
    softInput: { transform: (text: string, apply: (value: string) => string) => apply(text) },
  }) as unknown as Pick<TerminalKeybar, "markUserInput" | "transform">;

  keybar.markUserInput(true);
  textarea.value = "a";
  assert.equal(keybar.transform("\x1b[?1;2c"), "\x1b[?1;2c");
  assert.equal(modifiers.state("Ctrl"), "once");
  assert.equal(keybar.transform("\x7f"), "\x08");
  assert.equal(modifiers.state("Ctrl"), "off");
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
