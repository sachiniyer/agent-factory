import assert from "node:assert/strict";
import { test } from "node:test";
import { StickyModifiers } from "./terminal-keybar.js";
import { TerminalSoftInput } from "./terminal-soft-input.js";

function composition(type: string, data: string): Event {
  return Object.assign(new Event(type), { data });
}

function insertText(data: string | null, type = "input", isComposing = false): Event {
  return Object.assign(new Event(type, { cancelable: true }), { data, inputType: "insertText", isComposing });
}

test("compositionstart → update → end → insertText commits once without consuming Ctrl", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = new EventTarget();
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const writes: string[] = [];
  const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
  // Xterm binds this listener before the keybar. CompositionHelper reads the
  // committed textarea value in setTimeout(0), not a microtask, after end/input.
  textarea.addEventListener("compositionend", () => setTimeout(() => send("字"), 0));
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "字"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  host.dispatchEvent(insertText("字")); // isComposing is already false
  t.mock.timers.tick(0);
  assert.deepEqual(writes, ["字"]);
  assert.equal(modifiers.state("Ctrl"), "once");
  host.dispatchEvent(insertText("c"));
  assert.deepEqual(writes, ["字", "\x03"]);
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("composition ownership survives microtasks and leaves beforeinput uncancelled", async t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget(), textarea = new EventTarget();
  let sent = 0;
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => sent++);
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  await Promise.resolve(); // CompositionHelper has not forwarded its commit yet.
  const before = insertText("字", "beforeinput");
  host.dispatchEvent(before);
  assert.equal(before.defaultPrevented, false);
  host.dispatchEvent(insertText("字"));
  assert.equal(sent, 0);
  assert.equal(soft.transform("字", () => "modified"), "字");
  t.mock.timers.tick(0);
  host.dispatchEvent(insertText("x"));
  assert.equal(sent, 1);
});

test("Chrome commit before compositionend keeps no-keydown trailing input outside the commit", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const writes: string[] = [];
  const send = (text: string) => writes.push(soft.transform(text,
    (value, user) => modifiers.input(value, user ? "user" : "terminal")));
  textarea.addEventListener("compositionend", () => setTimeout(() => send(textarea.value), 0));
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "字";
  host.dispatchEvent(insertText("字", "input", true));
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.value += "x";
  host.dispatchEvent(insertText("x")); // Soft keyboards need not emit keydown.
  t.mock.timers.tick(0);

  assert.deepEqual(writes, ["字\x18"]);
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("Safari commit after compositionend establishes the boundary before trailing input", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.value = "字";
  host.dispatchEvent(insertText("字"));
  textarea.value += "x";
  host.dispatchEvent(insertText("x")); // Trailing even without keydown.

  assert.equal(soft.transform("字x", (value, user) => modifiers.input(value, user ? "user" : "terminal")), "字\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("null-data Safari commit freezes the mutated textarea boundary", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.value = "字";
  host.dispatchEvent(insertText(null));
  textarea.value += "x";
  host.dispatchEvent(insertText("x")); // No keydown between either mutation.

  assert.equal(soft.transform("字x", value => modifiers.input(value)), "字\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("post-end textarea growth extends a provisional composition commit", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "ab"));
  textarea.value = "a";
  host.dispatchEvent(insertText("a", "input", true));
  textarea.dispatchEvent(composition("compositionend", "ab"));
  textarea.value = "ab";
  host.dispatchEvent(insertText(null));

  assert.equal(soft.transform("ab", value => modifiers.input(value)), "ab");
  assert.equal(modifiers.state("Ctrl"), "once");
  assert.equal(soft.transform("x", value => modifiers.input(value)), "\x18");
});

test("a complete Chrome commit does not absorb a same-character ordinary key", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "x";
  textarea.dispatchEvent(composition("compositionend", "x"));
  textarea.value = "xx";
  host.dispatchEvent(insertText("x"));

  assert.equal(soft.transform("xx", value => modifiers.input(value)), "x\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("a complete textarea commit ignores a stale compositionend payload", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "ab";
  textarea.dispatchEvent(composition("compositionend", "x"));
  textarea.value = "abx";
  host.dispatchEvent(insertText("x"));

  assert.equal(soft.transform("abx", (value, user) => modifiers.input(value, user ? "user" : "terminal")), "ab\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("a stale compositionend extension cannot claim the next ordinary character", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "a";
  host.dispatchEvent(insertText("a", "input", true));
  textarea.dispatchEvent(composition("compositionend", "ab"));
  textarea.value = "ab";
  host.dispatchEvent(insertText("b"));

  assert.equal(soft.transform("ab", (value, user) => modifiers.input(value, user ? "user" : "terminal")), "a\x02");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("canceled composition gives no-keydown ordinary input a zero-length commit boundary", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const writes: string[] = [];
  const send = (text: string) => writes.push(soft.transform(text,
    (value, user) => modifiers.input(value, user ? "user" : "terminal")));
  textarea.addEventListener("compositionend", () => setTimeout(() => send(textarea.value), 0));
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", ""));
  textarea.value = "x";
  host.dispatchEvent(insertText("x")); // No keydown after the canceled IME.
  t.mock.timers.tick(0);

  assert.deepEqual(writes, ["\x18"]);
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("empty Safari composition payload defers to its final textarea mutation", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", ""));
  textarea.dispatchEvent(composition("compositionend", ""));
  textarea.value = "字";
  host.dispatchEvent(insertText(null));
  textarea.value = "字x";
  host.dispatchEvent(insertText("x"));

  assert.equal(soft.transform("字x", value => modifiers.input(value)), "字\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("updated composition rollback leaves the next no-keydown input ordinary", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "字"));
  textarea.value = "字";
  host.dispatchEvent(insertText("字", "input", true));
  textarea.value = "";
  textarea.dispatchEvent(composition("compositionend", ""));
  textarea.value = "x";
  host.dispatchEvent(insertText("x"));

  assert.equal(soft.transform("x", (value, user) => modifiers.input(value, user ? "user" : "terminal")), "\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("insertCompositionText mutation makes an updated rollback authoritative", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "字"));
  textarea.value = "字";
  host.dispatchEvent(Object.assign(new Event("input"), {
    data: "字", inputType: "insertCompositionText", isComposing: true,
  }));
  textarea.value = "";
  textarea.dispatchEvent(composition("compositionend", ""));
  textarea.value = "x";
  host.dispatchEvent(insertText("x"));

  assert.equal(soft.transform("x", (value, user) => modifiers.input(value, user ? "user" : "terminal")), "\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("Safari keycode 229 after compositionend belongs to the IME commit", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Alt", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.dispatchEvent(Object.assign(new Event("keydown"), { keyCode: 229 }));
  textarea.value = "字";
  host.dispatchEvent(insertText("字"));

  assert.equal(soft.transform("字", value => modifiers.input(value)), "字");
  assert.equal(modifiers.state("Alt"), "once");
  textarea.dispatchEvent(Object.assign(new Event("keyup"), { keyCode: 229 }));
  assert.equal(soft.transform("x", value => modifiers.input(value)), "\x1bx");
  assert.equal(modifiers.state("Alt"), "off");
});

for (const [keyCode, modifier, modified] of [
  [16, "Ctrl", "\x18"], [17, "Ctrl", "\x18"], [18, "Alt", "\x1bx"],
] as const) {
  test(`Safari modifier keycode ${keyCode} remains inside the pending IME commit`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget();
    const textarea = Object.assign(new EventTarget(), { value: "" });
    const modifiers = new StickyModifiers();
    modifiers.tap(modifier, 0);
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
    t.after(() => soft.dispose());

    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.dispatchEvent(composition("compositionend", "字"));
    textarea.dispatchEvent(Object.assign(new Event("keydown"), { keyCode }));
    textarea.value = "字";
    host.dispatchEvent(insertText("字"));

    assert.equal(soft.transform("字", value => modifiers.input(value)), "字");
    assert.equal(modifiers.state(modifier), "once");
    textarea.dispatchEvent(Object.assign(new Event("keyup"), { keyCode }));
    assert.equal(soft.transform("x", value => modifiers.input(value)), modified);
    assert.equal(modifiers.state(modifier), "off");
  });
}

test("old release leaves the new composition owned; reset and disposal clear all ownership", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget(), textarea = new EventTarget();
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "字"));
  t.mock.timers.tick(0);
  assert.equal(soft.transform("字", () => "modified"), "字");
  soft.reset();
  assert.equal(soft.transform("x", () => "modified"), "modified");
  textarea.dispatchEvent(composition("compositionend", "字"));
  soft.dispose();
  textarea.dispatchEvent(new Event("compositionstart"));
  t.mock.timers.tick(0);
  assert.equal(soft.transform("x", () => "modified"), "modified");
});

test("trailing soft input is forwarded before a new composition commit", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const writes: string[] = [];
  const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
  textarea.addEventListener("compositionend", event => {
    if ((event as CompositionEvent).data === "字") setTimeout(() => send("字"), 0);
  });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "字";
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.value = "字x";
  host.dispatchEvent(insertText("x"));
  textarea.dispatchEvent(new Event("compositionstart"));
  t.mock.timers.tick(0);

  assert.deepEqual(writes, ["字", "\x18"]);
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("forwarded trailing input is not mistaken for the new composition prefix", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const writes: string[] = [];
  const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
  t.after(() => soft.dispose());

  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "字";
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.value = "字x";
  host.dispatchEvent(insertText("x"));
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "字xx";
  textarea.dispatchEvent(composition("compositionupdate", "x"));
  t.mock.timers.tick(0);

  assert.deepEqual(writes, ["\x18"]);
  assert.equal(modifiers.state("Ctrl"), "off");
});

for (const [payload, expected, state] of [
  ["字x", "字\x18", "off"],
  ["字", "字", "once"],
  ["x", "\x18", "off"],
] as const) {
  test(`composition commit payload ${payload} modifies only ordinary input`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget(), textarea = new EventTarget();
    const modifiers = new StickyModifiers();
    modifiers.tap("Ctrl", 0);
    const writes: string[] = [];
    const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
    textarea.addEventListener("compositionend", () => setTimeout(() => send(payload), 0));
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
    t.after(() => soft.dispose());
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.dispatchEvent(composition("compositionupdate", "仮"));
    textarea.dispatchEvent(composition("compositionend", "字"));
    t.mock.timers.tick(0);
    assert.deepEqual(writes, [expected]);
    assert.equal(modifiers.state("Ctrl"), state);
    host.dispatchEvent(insertText("x"));
    assert.deepEqual(writes, [expected, state === "once" ? "\x18" : "x"]);
    assert.equal(modifiers.state("Ctrl"), "off");
  });
}

test("compositionupdate supplies the prefix when compositionend omits data", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget(), textarea = new EventTarget();
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "字"));
  textarea.dispatchEvent(new Event("compositionend"));
  assert.equal(soft.transform("字x", text => text.toUpperCase()), "字X");
  soft.reset();
  assert.equal(soft.transform("字x", () => "ordinary"), "ordinary");
});

for (const modifier of ["Ctrl", "Alt"] as const) {
  for (const trailing of ["", "x"]) {
    test(`textarea Korean reshape with ${modifier} and trailing ${JSON.stringify(trailing)}`, t => {
      t.mock.timers.enable({ apis: ["setTimeout"] });
      const host = new EventTarget();
      const textarea = Object.assign(new EventTarget(), { value: "old", selectionStart: 0 });
      const modifiers = new StickyModifiers();
      modifiers.tap(modifier, 0);
      const writes: string[] = [];
      const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
      textarea.addEventListener("compositionend", () => setTimeout(() => send(textarea.value.substring(3)), 0));
      const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
      t.after(() => soft.dispose());
      textarea.dispatchEvent(new Event("compositionstart"));
      textarea.dispatchEvent(composition("compositionupdate", "가"));
      textarea.value = "old각";
      textarea.dispatchEvent(composition("compositionend", "가"));
      host.dispatchEvent(insertText("가")); // native commit observation
      t.mock.timers.tick(0); // The next ordinary key arrives in a later turn.
      if (trailing) host.dispatchEvent(insertText(trailing));
      const modified = modifier === "Ctrl" ? "\x18" : "\x1bx";
      assert.deepEqual(writes, ["각", ...(trailing ? [modified] : [])]);
      assert.equal(modifiers.state(modifier), trailing ? "off" : "once");
      host.dispatchEvent(insertText("x"));
      assert.deepEqual(writes, ["각", ...(trailing ? [modified] : []), trailing ? "x" : modified]);
    });
  }
}

test("deferred textarea commit ignores event data and splits a payload suffix", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "old" });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionend", "가"));
  host.dispatchEvent(insertText("가", "beforeinput"));
  textarea.value = "old각";
  host.dispatchEvent(insertText("가"));
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  assert.equal(soft.transform("각x", text => modifiers.input(text)), "각\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("finalize reads reshaped textarea contents rather than the compositionend snapshot", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "old" });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "old가";
  textarea.dispatchEvent(composition("compositionend", "가"));
  textarea.value = "old각";
  host.dispatchEvent(insertText("가"));
  const modifiers = new StickyModifiers();
  modifiers.tap("Alt", 0);
  assert.equal(soft.transform("각x", text => modifiers.input(text)), "각\x1bx");
  assert.equal(modifiers.state("Alt"), "off");
});

for (const modifier of ["Ctrl", "Alt"] as const) {
  test(`pending composition A survives B starting with ${modifier} armed`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget();
    const textarea = Object.assign(new EventTarget(), { value: "" });
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
    t.after(() => soft.dispose());
    const modifiers = new StickyModifiers();
    modifiers.tap(modifier, 0);
    const transform = (text: string) => soft.transform(text, value => modifiers.input(value));
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value = "字";
    textarea.dispatchEvent(composition("compositionend", "字"));
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value = "字文";
    textarea.dispatchEvent(composition("compositionupdate", "文"));
    assert.equal(transform("字"), "字");
    assert.equal(modifiers.state(modifier), "once");
    t.mock.timers.tick(0); // A's release must not clear B.
    textarea.dispatchEvent(composition("compositionend", "文"));
    assert.equal(transform("文"), "文");
    assert.equal(modifiers.state(modifier), "once");
    t.mock.timers.tick(0);
    assert.equal(transform("x"), modifier === "Ctrl" ? "\x18" : "\x1bx");
  });

  for (const committed of ["가", "가나"]) {
    test(`Chrome textarea value ${committed} freezes the ${modifier} commit boundary at compositionend`, t => {
      t.mock.timers.enable({ apis: ["setTimeout"] });
      const host = new EventTarget();
      const textarea = Object.assign(new EventTarget(), { value: "old" });
      const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
      t.after(() => soft.dispose());
      const modifiers = new StickyModifiers();
      modifiers.tap(modifier, 0);
      textarea.dispatchEvent(new Event("compositionstart"));
      textarea.value = "old" + committed;
      host.dispatchEvent(insertText("ᄀ", "input", true));
      textarea.dispatchEvent(composition("compositionend", "ᄀ"));
      assert.equal(soft.transform(committed + "x", value => modifiers.input(value)),
        committed + (modifier === "Ctrl" ? "\x18" : "\x1bx"));
      assert.equal(modifiers.state(modifier), "off");
    });
  }
}

test("two finalized commits match oldest first and each entry is consumed once", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  for (const letter of ["字", "文"]) {
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value += letter;
    textarea.dispatchEvent(composition("compositionend", letter));
  }
  assert.equal(soft.transform("字", () => "modified"), "字");
  assert.equal(soft.transform("文", () => "modified"), "文");
  assert.equal(soft.transform("字", () => "modified"), "modified");
  t.mock.timers.tick(0);
});

test("reset cancels every pending release and drops every finalized range", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget(), textarea = new EventTarget();
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  for (const letter of ["字", "文"]) {
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.dispatchEvent(composition("compositionend", letter));
  }
  soft.reset();
  assert.equal(soft.transform("字", () => "modified"), "modified");
  assert.equal(soft.transform("文", () => "modified"), "modified");
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.dispatchEvent(composition("compositionupdate", "新"));
  t.mock.timers.tick(0);
  assert.equal(soft.transform("新", () => "modified"), "新");
});


test("one xterm payload can include two pending commits and an ordinary suffix", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  for (const letter of ["字", "文"]) {
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value += letter;
    textarea.dispatchEvent(composition("compositionend", letter));
  }
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  assert.equal(soft.transform("字文x", text => modifiers.input(text)), "字文\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
  assert.equal(soft.transform("字文", () => "ordinary"), "ordinary");
});

for (const modifier of ["Ctrl", "Alt"] as const) {
  test(`ordinary input without a post-composition commit event consumes ${modifier}`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget();
    const textarea = Object.assign(new EventTarget(), { value: "old" });
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
    t.after(() => soft.dispose());
    const modifiers = new StickyModifiers();
    modifiers.tap(modifier, 0);
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.value = "old字";
    textarea.dispatchEvent(composition("compositionend", "字"));
    // No commit insertText is required; finalize before the next key's turn.
    assert.equal(soft.transform("字", value => modifiers.input(value)), "字");
    t.mock.timers.tick(0);
    assert.equal(soft.transform("x", value => modifiers.input(value)),
      modifier === "Ctrl" ? "\x18" : "\x1bx");
    assert.equal(modifiers.state(modifier), "off");
    assert.equal(soft.transform("z", value => modifiers.input(value)), "z");
  });
}

for (const modifier of ["Ctrl", "Alt"] as const) {
  test(`Chrome append-shaped final IME mutation stays inside the commit with ${modifier}`, t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const host = new EventTarget();
    const textarea = Object.assign(new EventTarget(), { value: "old" });
    const modifiers = new StickyModifiers();
    modifiers.tap(modifier, 0);
    const writes: string[] = [];
    const send = (text: string) => writes.push(soft.transform(text, value => modifiers.input(value)));
    textarea.addEventListener("compositionend", () => setTimeout(() => send(textarea.value.substring(3)), 0));
    const soft = new TerminalSoftInput(host, textarea, () => true, () => false, send);
    t.after(() => soft.dispose());
    textarea.dispatchEvent(new Event("compositionstart"));
    textarea.dispatchEvent(composition("compositionupdate", "a"));
    textarea.value = "olda";
    host.dispatchEvent(insertText("b", "beforeinput", true));
    textarea.value += "b";
    host.dispatchEvent(insertText("b", "input", true));
    textarea.dispatchEvent(composition("compositionend", "a"));
    t.mock.timers.tick(0);
    assert.deepEqual(writes, ["ab"]);
    assert.equal(modifiers.state(modifier), "once");
    host.dispatchEvent(insertText("x"));
    assert.deepEqual(writes, ["ab", modifier === "Ctrl" ? "\x18" : "\x1bx"]);
    assert.equal(modifiers.state(modifier), "off");
  });
}

test("unmodified soft input remains native so xterm can observe Backspace", () => {
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const writes: string[] = [];
  host.addEventListener("input", event => {
    if ((event as InputEvent).inputType === "deleteContentBackward") writes.push("\x7f");
  });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => false);
  textarea.dispatchEvent(new Event("keydown"));
  const typed = insertText("a", "beforeinput");
  host.dispatchEvent(typed);
  assert.equal(typed.defaultPrevented, false);
  textarea.value = "a";
  host.dispatchEvent(insertText("a"));
  assert.equal(textarea.value, "a");
  textarea.dispatchEvent(new Event("keyup"));
  const backspace = Object.assign(new Event("beforeinput", { cancelable: true }), {
    inputType: "deleteContentBackward", data: null,
  });
  host.dispatchEvent(backspace);
  assert.equal(backspace.defaultPrevented, false);
  textarea.value = "";
  host.dispatchEvent(Object.assign(new Event("input"), { inputType: "deleteContentBackward", data: null }));
  assert.deepEqual(writes, ["\x7f"]);
  soft.dispose();
});

test("reshaped commit is the first mutation and leaves Ctrl for the next key", t => {
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "old" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "oldᄀ";
  textarea.dispatchEvent(composition("compositionend", "가"));
  textarea.value = "old각";
  host.dispatchEvent(insertText("가"));
  assert.equal(soft.transform("각", value => modifiers.input(value)), "각");
  assert.equal(modifiers.state("Ctrl"), "once");
  assert.equal(soft.transform("x", value => modifiers.input(value)), "\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("repeated commit character then ordinary character consumes Ctrl once", t => {
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "x";
  textarea.dispatchEvent(composition("compositionend", "x"));
  host.dispatchEvent(insertText("x"));
  textarea.value = "xx";
  host.dispatchEvent(insertText("x"));
  assert.equal(soft.transform("xx", value => modifiers.input(value)), "x\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("stale keydown recovery preserves textarea contents for a later 229 Backspace", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const writes: string[] = [];
  textarea.addEventListener("keydown", event => {
    if ((event as KeyboardEvent).keyCode !== 229) return;
    const oldValue = textarea.value;
    setTimeout(() => {
      if (textarea.value.length < oldValue.length) writes.push("\x7f");
    }, 0);
  });
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => false);
  textarea.dispatchEvent(new Event("keydown"));
  textarea.dispatchEvent(new Event("blur"));
  textarea.dispatchEvent(new Event("focus"));
  for (const letter of ["a", "b"]) {
    const before = insertText(letter, "beforeinput", true);
    host.dispatchEvent(before);
    assert.equal(before.defaultPrevented, false);
    textarea.value += letter;
    host.dispatchEvent(insertText(letter, "input", true));
  }
  assert.equal(textarea.value, "ab");
  assert.deepEqual(writes, ["a", "b"]);

  textarea.dispatchEvent(Object.assign(new Event("keydown"), { keyCode: 229 }));
  textarea.value = textarea.value.slice(0, -1);
  t.mock.timers.tick(0);
  assert.equal(textarea.value, "a");
  assert.deepEqual(writes, ["a", "b", "\x7f"]);
  soft.dispose();
});

test("stale recovery falls back to input data when beforeinput data is null", () => {
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const writes: string[] = [];
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => false);
  textarea.dispatchEvent(new Event("keydown"));
  textarea.dispatchEvent(new Event("blur"));

  const before = insertText(null, "beforeinput", true);
  host.dispatchEvent(before);
  assert.equal(before.defaultPrevented, false);
  textarea.value = "a";
  host.dispatchEvent(insertText("a", "input", true));

  assert.equal(textarea.value, "a");
  assert.deepEqual(writes, ["a"]);
  soft.dispose();
});

test("stale recovery derives both-null insertText data from the textarea", () => {
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "old" });
  const writes: string[] = [];
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => false);
  textarea.dispatchEvent(new Event("keydown"));
  textarea.dispatchEvent(new Event("blur"));

  const before = insertText(null, "beforeinput", true);
  host.dispatchEvent(before);
  assert.equal(before.defaultPrevented, false);
  textarea.value = "old字";
  host.dispatchEvent(insertText(null, "input", true));

  assert.equal(textarea.value, "old字");
  assert.deepEqual(writes, ["字"]);
  soft.dispose();
});

test("a fresh keydown after blur keeps native input ownership", () => {
  const host = new EventTarget();
  const textarea = new EventTarget();
  const writes: string[] = [];
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => false);
  textarea.dispatchEvent(new Event("keydown"));
  textarea.dispatchEvent(new Event("blur"));
  textarea.dispatchEvent(new Event("keydown"));
  const input = insertText("a", "beforeinput", true);
  host.dispatchEvent(input);
  assert.equal(input.defaultPrevented, false);
  assert.deepEqual(writes, []);
  textarea.dispatchEvent(new Event("keyup"));
  soft.dispose();
});

test("keydown after compositionend marks first ordinary input as trailing", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "字";
  textarea.dispatchEvent(composition("compositionend", "字"));
  textarea.dispatchEvent(new Event("keydown"));
  textarea.value = "字x";
  host.dispatchEvent(insertText("x"));
  assert.equal(soft.transform("字x", value => modifiers.input(value)), "字\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("commit mutation before keydown freezes boundary before ordinary input", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const host = new EventTarget();
  const textarea = Object.assign(new EventTarget(), { value: "" });
  const modifiers = new StickyModifiers();
  modifiers.tap("Ctrl", 0);
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, () => {});
  t.after(() => soft.dispose());
  textarea.dispatchEvent(new Event("compositionstart"));
  textarea.value = "字";
  textarea.dispatchEvent(composition("compositionend", "字"));
  host.dispatchEvent(insertText("字"));
  textarea.dispatchEvent(new Event("keydown"));
  textarea.value = "字x";
  host.dispatchEvent(insertText("x"));
  assert.equal(soft.transform("字x", value => modifiers.input(value)), "字\x18");
  assert.equal(modifiers.state("Ctrl"), "off");
});

test("armed modifier intercepts ordinary input without stale keydown", () => {
  const host = new EventTarget();
  const textarea = new EventTarget();
  const writes: string[] = [];
  const soft = new TerminalSoftInput(host, textarea, () => true, () => false, text => writes.push(text), () => true);
  const input = insertText("a", "beforeinput");
  host.dispatchEvent(input);
  assert.equal(input.defaultPrevented, true);
  assert.deepEqual(writes, ["a"]);
  soft.dispose();
});
